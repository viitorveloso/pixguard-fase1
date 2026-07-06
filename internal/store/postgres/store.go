package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"pixledger/internal/domain"
	"pixledger/internal/service"
)

// Store implements service.Store on PostgreSQL.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) CreateAccount(ctx context.Context, account domain.Account) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (id, number, holder, balance_cents, created_at) VALUES ($1, $2, $3, $4, $5)`,
		account.ID, account.Number, account.Holder, account.BalanceCents, account.CreatedAt,
	)
	if isUniqueViolation(err) {
		return domain.ErrNumberTaken
	}
	return err
}

func (s *Store) GetAccount(ctx context.Context, id string) (domain.Account, error) {
	var account domain.Account
	err := s.db.QueryRowContext(ctx,
		`SELECT id, number, holder, balance_cents, created_at FROM accounts WHERE id = $1`, id,
	).Scan(&account.ID, &account.Number, &account.Holder, &account.BalanceCents, &account.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	return account, err
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// ExecuteTransfer retries only on serialization/deadlock SQLSTATEs. The lock
// ordering below makes deadlocks impossible by design; the retry is a seatbelt,
// not the safety mechanism.
func (s *Store) ExecuteTransfer(ctx context.Context, req service.TransferRequest) (service.TransferOutcome, error) {
	var (
		outcome service.TransferOutcome
		err     error
	)
	for attempt := 0; attempt < 3; attempt++ {
		outcome, err = s.executeTransferOnce(ctx, req)
		if err == nil || !isRetryableTxError(err) {
			return outcome, err
		}
		select {
		case <-ctx.Done():
			return service.TransferOutcome{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 25 * time.Millisecond):
		}
	}
	return outcome, err
}

// executeTransferOnce runs the whole financial operation in ONE transaction:
//
//  1. Atomically reserve the idempotency key (INSERT ... ON CONFLICT DO
//     NOTHING RETURNING id). Losing the race means someone else owns the key:
//     load their result and replay it. Concurrent duplicates block on the
//     unique index until the winner commits, so exactly one debit happens.
//  2. Lock both account rows with SELECT ... FOR UPDATE in lexicographic ID
//     order. A global lock order (key insert first, then sorted accounts)
//     removes any lock cycle — deadlocks cannot form.
//  3. Insufficient funds is a BUSINESS decision, not an error path: record a
//     failed transfer, bind it to the key and COMMIT, so the same key replays
//     the same 402 forever. Validation/not-found errors roll back and do NOT
//     consume the key.
//  4. Apply balance updates, insert the transfer and the outbox event, bind
//     the key, commit. The webhook is delivered later by the outbox worker —
//     the commit never waits on the network.
func (s *Store) executeTransferOnce(ctx context.Context, req service.TransferRequest) (out service.TransferOutcome, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return service.TransferOutcome{}, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// (1) Atomic key reservation.
	keyID := uuid.NewString()
	var reservedID string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO idempotency_keys (id, key, request_hash) VALUES ($1, $2, $3)
		 ON CONFLICT (key) DO NOTHING
		 RETURNING id`,
		keyID, req.IdempotencyKey, req.RequestHash,
	).Scan(&reservedID)
	if errors.Is(err, sql.ErrNoRows) {
		// Key already owned: replay path.
		var (
			storedHash string
			transferID sql.NullString
		)
		err = tx.QueryRowContext(ctx,
			`SELECT request_hash, transaction_id FROM idempotency_keys WHERE key = $1`,
			req.IdempotencyKey,
		).Scan(&storedHash, &transferID)
		if err != nil {
			return service.TransferOutcome{}, fmt.Errorf("load idempotency key: %w", err)
		}
		if storedHash != req.RequestHash {
			return service.TransferOutcome{}, domain.ErrIdempotencyConflict
		}
		if !transferID.Valid {
			// Key exists but was never bound to a result (crashed owner whose
			// tx rolled back would have removed the row; a visible unbound row
			// is a same-key request still in flight that somehow committed —
			// treat defensively as conflict rather than double-execute).
			return service.TransferOutcome{}, domain.ErrIdempotencyConflict
		}
		transfer, lerr := loadTransfer(ctx, tx, transferID.String)
		if lerr != nil {
			return service.TransferOutcome{}, lerr
		}
		if err = tx.Commit(); err != nil {
			return service.TransferOutcome{}, fmt.Errorf("commit replay: %w", err)
		}
		return service.TransferOutcome{Transfer: transfer, Replayed: true}, nil
	}
	if err != nil {
		return service.TransferOutcome{}, fmt.Errorf("reserve idempotency key: %w", err)
	}

	// (2) Ordered row locks.
	first, second := req.Candidate.SourceAccountID, req.Candidate.DestAccountID
	if second < first {
		first, second = second, first
	}
	balances := make(map[string]int64, 2)
	for _, id := range []string{first, second} {
		var balance int64
		err = tx.QueryRowContext(ctx,
			`SELECT balance_cents FROM accounts WHERE id = $1 FOR UPDATE`, id,
		).Scan(&balance)
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			err = domain.ErrAccountNotFound
			return service.TransferOutcome{}, err
		}
		if err != nil {
			return service.TransferOutcome{}, fmt.Errorf("lock account %s: %w", id, err)
		}
		balances[id] = balance
	}

	transfer := req.Candidate

	// (3) Insufficient funds: committed business failure.
	if balances[transfer.SourceAccountID] < transfer.AmountCents {
		transfer.Status = domain.StatusFailed
		transfer.FailureReason = "insufficient_funds"
		if err = insertTransfer(ctx, tx, transfer); err != nil {
			return service.TransferOutcome{}, err
		}
		if err = bindKey(ctx, tx, keyID, transfer.ID); err != nil {
			return service.TransferOutcome{}, err
		}
		if err = tx.Commit(); err != nil {
			return service.TransferOutcome{}, fmt.Errorf("commit failed transfer: %w", err)
		}
		return service.TransferOutcome{Transfer: transfer}, nil
	}

	// (4) Move money + outbox, all-or-nothing.
	if _, err = tx.ExecContext(ctx,
		`UPDATE accounts SET balance_cents = balance_cents - $1 WHERE id = $2`,
		transfer.AmountCents, transfer.SourceAccountID,
	); err != nil {
		return service.TransferOutcome{}, fmt.Errorf("debit: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE accounts SET balance_cents = balance_cents + $1 WHERE id = $2`,
		transfer.AmountCents, transfer.DestAccountID,
	); err != nil {
		return service.TransferOutcome{}, fmt.Errorf("credit: %w", err)
	}
	if err = insertTransfer(ctx, tx, transfer); err != nil {
		return service.TransferOutcome{}, err
	}

	payload, err := json.Marshal(map[string]any{
		"transaction_id":    transfer.ID,
		"end_to_end_id":     transfer.EndToEndID,
		"source_account_id": transfer.SourceAccountID,
		"dest_account_id":   transfer.DestAccountID,
		"amount_cents":      transfer.AmountCents,
		"description":       transfer.Description,
		"occurred_at":       transfer.CreatedAt,
	})
	if err != nil {
		return service.TransferOutcome{}, fmt.Errorf("marshal event: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO events (id, type, payload) VALUES ($1, $2, $3)`,
		uuid.NewString(), "transfer.completed", payload,
	); err != nil {
		return service.TransferOutcome{}, fmt.Errorf("insert event: %w", err)
	}
	if err = bindKey(ctx, tx, keyID, transfer.ID); err != nil {
		return service.TransferOutcome{}, err
	}
	if err = tx.Commit(); err != nil {
		return service.TransferOutcome{}, fmt.Errorf("commit: %w", err)
	}
	return service.TransferOutcome{Transfer: transfer}, nil
}

func insertTransfer(ctx context.Context, tx *sql.Tx, t domain.Transfer) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO transactions
		   (id, source_account_id, dest_account_id, amount_cents, description, status, failure_reason, end_to_end_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.ID, t.SourceAccountID, t.DestAccountID, t.AmountCents, t.Description, string(t.Status), t.FailureReason, t.EndToEndID, t.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert transfer: %w", err)
	}
	return nil
}

func loadTransfer(ctx context.Context, tx *sql.Tx, id string) (domain.Transfer, error) {
	var (
		t      domain.Transfer
		status string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, source_account_id, dest_account_id, amount_cents, description, status, failure_reason, end_to_end_id, created_at
		   FROM transactions WHERE id = $1`, id,
	).Scan(&t.ID, &t.SourceAccountID, &t.DestAccountID, &t.AmountCents, &t.Description, &status, &t.FailureReason, &t.EndToEndID, &t.CreatedAt)
	if err != nil {
		return domain.Transfer{}, fmt.Errorf("load transfer: %w", err)
	}
	t.Status = domain.TransferStatus(status)
	return t, nil
}

func bindKey(ctx context.Context, tx *sql.Tx, keyID, transferID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET transaction_id = $1 WHERE id = $2`, transferID, keyID,
	); err != nil {
		return fmt.Errorf("bind idempotency key: %w", err)
	}
	return nil
}

// isRetryableTxError matches serialization_failure (40001) and
// deadlock_detected (40P01).
func isRetryableTxError(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "40001" || pqErr.Code == "40P01"
	}
	return false
}

// isUniqueViolation matches unique_violation (23505).
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}

// isInvalidUUID matches invalid_text_representation (22P02) so a malformed ID
// in the URL behaves as "not found" instead of a 500.
func isInvalidUUID(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "22P02"
	}
	return false
}
