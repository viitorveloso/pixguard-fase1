// Package memory provides an in-memory Store used by unit tests. It mirrors
// the Postgres semantics exactly: atomic key reservation, replay on same
// hash, conflict on different hash, and insufficient funds recorded as a
// failed transfer that consumes the key.
package memory

import (
	"context"
	"sync"

	"pixledger/internal/domain"
	"pixledger/internal/service"
)

type keyRecord struct {
	requestHash string
	transferID  string
}

type Store struct {
	mu        sync.Mutex
	accounts  map[string]domain.Account
	numbers   map[string]struct{}
	keys      map[string]keyRecord
	transfers map[string]domain.Transfer
	// Events stands in for the outbox table so tests can assert emission.
	Events []domain.Transfer
}

func New() *Store {
	return &Store{
		accounts:  make(map[string]domain.Account),
		numbers:   make(map[string]struct{}),
		keys:      make(map[string]keyRecord),
		transfers: make(map[string]domain.Transfer),
	}
}

func (s *Store) CreateAccount(_ context.Context, account domain.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.numbers[account.Number]; taken {
		return domain.ErrNumberTaken
	}
	s.numbers[account.Number] = struct{}{}
	s.accounts[account.ID] = account
	return nil
}

func (s *Store) GetAccount(_ context.Context, id string) (domain.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[id]
	if !ok {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	return account, nil
}

// ExecuteTransfer serializes on a single mutex — the in-memory equivalent of
// the row locks + unique key insert the Postgres store relies on.
func (s *Store) ExecuteTransfer(_ context.Context, req service.TransferRequest) (service.TransferOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, seen := s.keys[req.IdempotencyKey]; seen {
		if rec.requestHash != req.RequestHash {
			return service.TransferOutcome{}, domain.ErrIdempotencyConflict
		}
		return service.TransferOutcome{Transfer: s.transfers[rec.transferID], Replayed: true}, nil
	}

	src, ok := s.accounts[req.Candidate.SourceAccountID]
	if !ok {
		return service.TransferOutcome{}, domain.ErrAccountNotFound
	}
	dst, ok := s.accounts[req.Candidate.DestAccountID]
	if !ok {
		return service.TransferOutcome{}, domain.ErrAccountNotFound
	}

	transfer := req.Candidate
	if src.BalanceCents < transfer.AmountCents {
		// Business failure: recorded and key consumed, so the same key
		// replays the same 402 deterministically.
		transfer.Status = domain.StatusFailed
		transfer.FailureReason = "insufficient_funds"
		s.transfers[transfer.ID] = transfer
		s.keys[req.IdempotencyKey] = keyRecord{requestHash: req.RequestHash, transferID: transfer.ID}
		return service.TransferOutcome{Transfer: transfer}, nil
	}

	src.BalanceCents -= transfer.AmountCents
	dst.BalanceCents += transfer.AmountCents
	s.accounts[src.ID] = src
	s.accounts[dst.ID] = dst
	s.transfers[transfer.ID] = transfer
	s.keys[req.IdempotencyKey] = keyRecord{requestHash: req.RequestHash, transferID: transfer.ID}
	s.Events = append(s.Events, transfer)
	return service.TransferOutcome{Transfer: transfer}, nil
}

func (s *Store) Ping(context.Context) error { return nil }
