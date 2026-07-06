package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"pixledger/internal/domain"
)

// Store is defined where it is consumed (Go interface convention). Both the
// Postgres store and the in-memory test store implement it. ExecuteTransfer
// owns all financial guarantees: atomic idempotency, ordered row locks and
// balance invariants live inside a single database transaction.
type Store interface {
	CreateAccount(ctx context.Context, account domain.Account) error
	GetAccount(ctx context.Context, id string) (domain.Account, error)
	ExecuteTransfer(ctx context.Context, req TransferRequest) (TransferOutcome, error)
	Ping(ctx context.Context) error
}

// TransferRequest carries the idempotency key, a hash of the client payload
// (to detect same-key/different-payload conflicts) and the candidate transfer
// the store should persist if this is a first execution.
type TransferRequest struct {
	IdempotencyKey string
	RequestHash    string
	Candidate      domain.Transfer
}

// TransferOutcome distinguishes a fresh execution from an idempotent replay so
// the HTTP layer can answer 201 vs 200 + Idempotent-Replayed: true.
type TransferOutcome struct {
	Transfer domain.Transfer
	Replayed bool
}

// Ledger is the application service: pure validation and orchestration, no
// SQL. Time is injected for deterministic tests.
type Ledger struct {
	store Store
	ispb  string
	now   func() time.Time
}

func NewLedger(store Store, ispb string, now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	return &Ledger{store: store, ispb: ispb, now: now}
}

// CreateAccountInput is the validated boundary for account creation.
type CreateAccountInput struct {
	Number              string
	Holder              string
	InitialBalanceCents int64
}

func (l *Ledger) CreateAccount(ctx context.Context, in CreateAccountInput) (domain.Account, error) {
	account, err := domain.NewAccount(in.Number, in.Holder, in.InitialBalanceCents, l.now())
	if err != nil {
		return domain.Account{}, err
	}
	if err := l.store.CreateAccount(ctx, account); err != nil {
		return domain.Account{}, err
	}
	return account, nil
}

func (l *Ledger) GetAccount(ctx context.Context, id string) (domain.Account, error) {
	if strings.TrimSpace(id) == "" {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	return l.store.GetAccount(ctx, id)
}

// Ping reports storage health for the readiness probe.
func (l *Ledger) Ping(ctx context.Context) error { return l.store.Ping(ctx) }

// TransferInput is the raw client intent for POST /api/pix.
type TransferInput struct {
	IdempotencyKey  string
	SourceAccountID string
	DestAccountID   string
	AmountCents     int64
	Description     string
}

const maxDescriptionLen = 140

// Transfer validates the request, mints the candidate transfer (UUID +
// EndToEndId) and delegates the atomic execution to the store. Validation
// failures never consume the idempotency key — only requests that reach the
// store's transaction do.
func (l *Ledger) Transfer(ctx context.Context, in TransferInput) (TransferOutcome, error) {
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return TransferOutcome{}, domain.ErrMissingIdempotencyKey
	}
	src := strings.TrimSpace(in.SourceAccountID)
	dst := strings.TrimSpace(in.DestAccountID)
	if src == "" || dst == "" || len(in.Description) > maxDescriptionLen {
		return TransferOutcome{}, domain.ErrInvalidPayload
	}
	if in.AmountCents <= 0 {
		return TransferOutcome{}, domain.ErrInvalidAmount
	}
	if src == dst {
		return TransferOutcome{}, domain.ErrSameAccount
	}

	now := l.now()
	candidate := domain.Transfer{
		ID:              uuid.NewString(),
		SourceAccountID: src,
		DestAccountID:   dst,
		AmountCents:     in.AmountCents,
		Description:     in.Description,
		Status:          domain.StatusCompleted,
		EndToEndID:      domain.NewEndToEndID(l.ispb, now),
		CreatedAt:       now.UTC(),
	}
	return l.store.ExecuteTransfer(ctx, TransferRequest{
		IdempotencyKey: in.IdempotencyKey,
		RequestHash:    HashRequest(src, dst, in.AmountCents, in.Description),
		Candidate:      candidate,
	})
}

// HashRequest fingerprints the business payload so a reused idempotency key
// with a different payload is detected as a 409 conflict.
func HashRequest(src, dst string, amountCents int64, description string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s", src, dst, amountCents, description)))
	return hex.EncodeToString(sum[:])
}
