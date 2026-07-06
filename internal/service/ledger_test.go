package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"pixledger/internal/domain"
	"pixledger/internal/service"
	"pixledger/internal/store/memory"
)

func newLedger(t *testing.T) (*service.Ledger, *memory.Store) {
	t.Helper()
	store := memory.New()
	fixed := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	return service.NewLedger(store, "00000000", func() time.Time { return fixed }), store
}

func mustAccount(t *testing.T, l *service.Ledger, number string, balance int64) domain.Account {
	t.Helper()
	account, err := l.CreateAccount(context.Background(), service.CreateAccountInput{
		Number: number, Holder: "Holder " + number, InitialBalanceCents: balance,
	})
	if err != nil {
		t.Fatalf("create account %s: %v", number, err)
	}
	return account
}

func TestTransferHappyPath(t *testing.T) {
	ledger, store := newLedger(t)
	a := mustAccount(t, ledger, "0001", 10_000)
	b := mustAccount(t, ledger, "0002", 0)

	out, err := ledger.Transfer(context.Background(), service.TransferInput{
		IdempotencyKey: "k1", SourceAccountID: a.ID, DestAccountID: b.ID,
		AmountCents: 2_500, Description: "lunch",
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if out.Replayed {
		t.Fatal("first execution must not be a replay")
	}
	if out.Transfer.Status != domain.StatusCompleted {
		t.Fatalf("want completed, got %s", out.Transfer.Status)
	}
	if len(out.Transfer.EndToEndID) != 32 {
		t.Fatalf("bad end-to-end id %q", out.Transfer.EndToEndID)
	}

	gotA, _ := ledger.GetAccount(context.Background(), a.ID)
	gotB, _ := ledger.GetAccount(context.Background(), b.ID)
	if gotA.BalanceCents != 7_500 || gotB.BalanceCents != 2_500 {
		t.Fatalf("balances wrong: %d / %d", gotA.BalanceCents, gotB.BalanceCents)
	}
	if len(store.Events) != 1 {
		t.Fatalf("want 1 outbox event, got %d", len(store.Events))
	}
}

func TestTransferReplayReturnsSameTransaction(t *testing.T) {
	ledger, store := newLedger(t)
	a := mustAccount(t, ledger, "0001", 10_000)
	b := mustAccount(t, ledger, "0002", 0)

	in := service.TransferInput{
		IdempotencyKey: "same-key", SourceAccountID: a.ID, DestAccountID: b.ID,
		AmountCents: 1_000, Description: "once",
	}
	first, err := ledger.Transfer(context.Background(), in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := ledger.Transfer(context.Background(), in)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Replayed {
		t.Fatal("second call must be flagged as replay")
	}
	if second.Transfer.ID != first.Transfer.ID || second.Transfer.EndToEndID != first.Transfer.EndToEndID {
		t.Fatal("replay must return the exact original transaction")
	}
	gotA, _ := ledger.GetAccount(context.Background(), a.ID)
	if gotA.BalanceCents != 9_000 {
		t.Fatalf("balance debited more than once: %d", gotA.BalanceCents)
	}
	if len(store.Events) != 1 {
		t.Fatalf("replay must not emit a second event, got %d", len(store.Events))
	}
}

func TestTransferIdempotencyConflict(t *testing.T) {
	ledger, _ := newLedger(t)
	a := mustAccount(t, ledger, "0001", 10_000)
	b := mustAccount(t, ledger, "0002", 0)

	base := service.TransferInput{
		IdempotencyKey: "conflict-key", SourceAccountID: a.ID, DestAccountID: b.ID,
		AmountCents: 1_000, Description: "original",
	}
	if _, err := ledger.Transfer(context.Background(), base); err != nil {
		t.Fatalf("first: %v", err)
	}
	changed := base
	changed.AmountCents = 2_000
	if _, err := ledger.Transfer(context.Background(), changed); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}
}

func TestInsufficientFundsIsRecordedAndReplays(t *testing.T) {
	ledger, _ := newLedger(t)
	a := mustAccount(t, ledger, "0001", 500)
	b := mustAccount(t, ledger, "0002", 0)

	in := service.TransferInput{
		IdempotencyKey: "poor-key", SourceAccountID: a.ID, DestAccountID: b.ID,
		AmountCents: 1_000, Description: "too much",
	}
	first, err := ledger.Transfer(context.Background(), in)
	if err != nil {
		t.Fatalf("insufficient funds must not be a transport error: %v", err)
	}
	if first.Transfer.Status != domain.StatusFailed || first.Transfer.FailureReason != "insufficient_funds" {
		t.Fatalf("want recorded failure, got %+v", first.Transfer)
	}

	second, err := ledger.Transfer(context.Background(), in)
	if err != nil {
		t.Fatalf("replay of failure: %v", err)
	}
	if !second.Replayed || second.Transfer.ID != first.Transfer.ID {
		t.Fatal("failed transfer must replay deterministically under the same key")
	}
	gotA, _ := ledger.GetAccount(context.Background(), a.ID)
	if gotA.BalanceCents != 500 {
		t.Fatalf("balance must be untouched, got %d", gotA.BalanceCents)
	}
}

func TestTransferValidation(t *testing.T) {
	ledger, _ := newLedger(t)
	a := mustAccount(t, ledger, "0001", 1_000)
	b := mustAccount(t, ledger, "0002", 0)

	cases := []struct {
		name string
		in   service.TransferInput
		want error
	}{
		{"missing key", service.TransferInput{SourceAccountID: a.ID, DestAccountID: b.ID, AmountCents: 1}, domain.ErrMissingIdempotencyKey},
		{"empty source", service.TransferInput{IdempotencyKey: "k", DestAccountID: b.ID, AmountCents: 1}, domain.ErrInvalidPayload},
		{"long description", service.TransferInput{IdempotencyKey: "k", SourceAccountID: a.ID, DestAccountID: b.ID, AmountCents: 1, Description: string(make([]byte, 141))}, domain.ErrInvalidPayload},
		{"zero amount", service.TransferInput{IdempotencyKey: "k", SourceAccountID: a.ID, DestAccountID: b.ID, AmountCents: 0}, domain.ErrInvalidAmount},
		{"negative amount", service.TransferInput{IdempotencyKey: "k", SourceAccountID: a.ID, DestAccountID: b.ID, AmountCents: -5}, domain.ErrInvalidAmount},
		{"same account", service.TransferInput{IdempotencyKey: "k", SourceAccountID: a.ID, DestAccountID: a.ID, AmountCents: 1}, domain.ErrSameAccount},
		{"unknown account", service.TransferInput{IdempotencyKey: "k", SourceAccountID: a.ID, DestAccountID: "3b8e6d1c-0000-0000-0000-000000000000", AmountCents: 1}, domain.ErrAccountNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ledger.Transfer(context.Background(), tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// TestConcurrentStormDistinctKeys: 50 goroutines, distinct keys, each moving
// 1000 from A (balance 10000) to B. Exactly 10 must complete and 40 must fail
// with insufficient funds; A ends at 0, B at 10000, money conserved.
func TestConcurrentStormDistinctKeys(t *testing.T) {
	ledger, _ := newLedger(t)
	a := mustAccount(t, ledger, "0001", 10_000)
	b := mustAccount(t, ledger, "0002", 0)

	const workers = 50
	results := make([]domain.TransferStatus, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := ledger.Transfer(context.Background(), service.TransferInput{
				IdempotencyKey:  string(rune('A'+i%26)) + "-" + time.Now().String() + string(rune(i)),
				SourceAccountID: a.ID, DestAccountID: b.ID, AmountCents: 1_000,
			})
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			results[i] = out.Transfer.Status
		}(i)
	}
	wg.Wait()

	var completed, failed int
	for _, status := range results {
		switch status {
		case domain.StatusCompleted:
			completed++
		case domain.StatusFailed:
			failed++
		}
	}
	if completed != 10 || failed != 40 {
		t.Fatalf("want 10 completed / 40 failed, got %d / %d", completed, failed)
	}
	gotA, _ := ledger.GetAccount(context.Background(), a.ID)
	gotB, _ := ledger.GetAccount(context.Background(), b.ID)
	if gotA.BalanceCents != 0 || gotB.BalanceCents != 10_000 {
		t.Fatalf("money not conserved: A=%d B=%d", gotA.BalanceCents, gotB.BalanceCents)
	}
}

// TestConcurrentStormSameKey: 30 goroutines racing the SAME key must produce
// exactly one debit; everyone converges on the same transaction ID.
func TestConcurrentStormSameKey(t *testing.T) {
	ledger, _ := newLedger(t)
	a := mustAccount(t, ledger, "0001", 10_000)
	b := mustAccount(t, ledger, "0002", 0)

	const workers = 30
	ids := make([]string, workers)
	replays := make([]bool, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := ledger.Transfer(context.Background(), service.TransferInput{
				IdempotencyKey:  "shared-key",
				SourceAccountID: a.ID, DestAccountID: b.ID, AmountCents: 1_000, Description: "same",
			})
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			ids[i] = out.Transfer.ID
			replays[i] = out.Replayed
		}(i)
	}
	wg.Wait()

	fresh := 0
	for i := 0; i < workers; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("diverging transaction ids: %s vs %s", ids[i], ids[0])
		}
		if !replays[i] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("exactly one execution must be fresh, got %d", fresh)
	}
	gotA, _ := ledger.GetAccount(context.Background(), a.ID)
	if gotA.BalanceCents != 9_000 {
		t.Fatalf("debited %d times", (10_000-gotA.BalanceCents)/1_000)
	}
}
