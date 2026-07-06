// Package integration runs the full stack (HTTP → service → Postgres store →
// outbox worker) against a real PostgreSQL. Gated by DATABASE_URL:
//
//	DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable \
//	  go test ./internal/integration/... -race -count=1
//
// These tests prove the guarantees the README claims: no double debit under
// concurrent duplicates, no deadlock under bidirectional fire, deterministic
// replay of business failures, and at-least-once outbox delivery with backoff.
package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pixledger/internal/httpapi"
	"pixledger/internal/jwt"
	"pixledger/internal/metrics"
	"pixledger/internal/outbox"
	"pixledger/internal/service"
	"pixledger/internal/store/postgres"
	"pixledger/migrations"
)

var (
	dbOnce sync.Once
	db     *sql.DB
	dbErr  error
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration tests")
	}
	dbOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		db, dbErr = postgres.Open(ctx, dsn)
		if dbErr == nil {
			dbErr = postgres.Migrate(ctx, db, migrations.FS)
		}
	})
	if dbErr != nil {
		t.Fatalf("test database setup: %v", dbErr)
	}
	if _, err := db.Exec(`TRUNCATE idempotency_keys, events, transactions, accounts`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

type env struct {
	t      *testing.T
	db     *sql.DB
	server *httptest.Server
	token  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	database := testDB(t)
	codec := jwt.NewCodec("integration-secret")
	store := postgres.NewStore(database)
	ledger := service.NewLedger(store, "00000000", nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := httpapi.New(ledger, codec, metrics.NewRegistry(), log)

	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)

	token, err := codec.Sign("integration", time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return &env{t: t, db: database, server: server, token: token}
}

func (e *env) do(method, path string, body any, headers map[string]string) (*http.Response, map[string]any) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		json.Unmarshal(raw, &decoded)
	}
	return resp, decoded
}

func (e *env) createAccount(number string, balance int64) string {
	e.t.Helper()
	resp, body := e.do(http.MethodPost, "/api/accounts", map[string]any{
		"number": number, "holder": "Holder " + number, "initial_balance_cents": balance,
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("create account: want 201, got %d (%v)", resp.StatusCode, body)
	}
	return body["id"].(string)
}

func (e *env) balance(id string) int64 {
	e.t.Helper()
	var balance int64
	if err := e.db.QueryRow(`SELECT balance_cents FROM accounts WHERE id = $1`, id).Scan(&balance); err != nil {
		e.t.Fatalf("balance query: %v", err)
	}
	return balance
}

// T1 — 50 concurrent transfers with DISTINCT keys draining A (10 000) into B,
// 1 000 each: exactly 10 succeed (201), 40 fail with 402, balances land at
// A=0 / B=10 000 and the database agrees.
func TestConcurrentTransfersDistinctKeys(t *testing.T) {
	e := newEnv(t)
	src := e.createAccount("0001", 10_000)
	dst := e.createAccount("0002", 0)

	const workers = 50
	var created, insufficient, other int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _ := e.do(http.MethodPost, "/api/pix", map[string]any{
				"source_account_id": src, "dest_account_id": dst,
				"amount_cents": 1_000, "description": "storm",
			}, map[string]string{"X-Idempotency-Key": fmt.Sprintf("t1-%d", i)})
			switch resp.StatusCode {
			case http.StatusCreated:
				atomic.AddInt64(&created, 1)
			case http.StatusPaymentRequired:
				atomic.AddInt64(&insufficient, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}(i)
	}
	wg.Wait()

	if created != 10 || insufficient != 40 || other != 0 {
		t.Fatalf("want 10x201 / 40x402 / 0 other, got %d / %d / %d", created, insufficient, other)
	}
	if got := e.balance(src); got != 0 {
		t.Fatalf("source balance: want 0, got %d", got)
	}
	if got := e.balance(dst); got != 10_000 {
		t.Fatalf("dest balance: want 10000, got %d", got)
	}
	var completedRows int
	if err := e.db.QueryRow(`SELECT count(*) FROM transactions WHERE status = 'completed'`).Scan(&completedRows); err != nil {
		t.Fatal(err)
	}
	if completedRows != 10 {
		t.Fatalf("db must hold exactly 10 completed rows, got %d", completedRows)
	}
}

// T2 — 30 goroutines racing the SAME key: exactly one 201, twenty-nine 200s
// with Idempotent-Replayed, a single row in transactions, one debit.
func TestConcurrentTransfersSameKey(t *testing.T) {
	e := newEnv(t)
	src := e.createAccount("0001", 10_000)
	dst := e.createAccount("0002", 0)

	const workers = 30
	var created, replayed, other int64
	ids := make([]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, body := e.do(http.MethodPost, "/api/pix", map[string]any{
				"source_account_id": src, "dest_account_id": dst,
				"amount_cents": 1_000, "description": "same-key",
			}, map[string]string{"X-Idempotency-Key": "t2-shared"})
			if id, ok := body["transaction_id"].(string); ok {
				ids[i] = id
			}
			switch {
			case resp.StatusCode == http.StatusCreated:
				atomic.AddInt64(&created, 1)
			case resp.StatusCode == http.StatusOK && resp.Header.Get("Idempotent-Replayed") == "true":
				atomic.AddInt64(&replayed, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}(i)
	}
	wg.Wait()

	if created != 1 || replayed != 29 || other != 0 {
		t.Fatalf("want 1x201 / 29x200-replay / 0 other, got %d / %d / %d", created, replayed, other)
	}
	for i := 1; i < workers; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("diverging transaction ids under one key: %s vs %s", ids[i], ids[0])
		}
	}
	var txRows int
	if err := e.db.QueryRow(`SELECT count(*) FROM transactions`).Scan(&txRows); err != nil {
		t.Fatal(err)
	}
	if txRows != 1 {
		t.Fatalf("want exactly 1 transaction row, got %d", txRows)
	}
	if got := e.balance(src); got != 9_000 {
		t.Fatalf("source debited %d times", (10_000-got)/1_000)
	}
}

// T3 — crossfire: 20 goroutines × 25 iterations transferring 1 cent A→B and
// B→A simultaneously. Ordered row locks make deadlocks impossible; every
// request must come back 201 and both balances must end exactly where they
// started, total conserved.
func TestBidirectionalCrossfireNoDeadlock(t *testing.T) {
	e := newEnv(t)
	const initial = 1_000_000
	a := e.createAccount("0001", initial)
	b := e.createAccount("0002", initial)

	const workers = 20
	const iterations = 25
	var failures int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			src, dst := a, b
			if w%2 == 1 {
				src, dst = b, a
			}
			for i := 0; i < iterations; i++ {
				resp, body := e.do(http.MethodPost, "/api/pix", map[string]any{
					"source_account_id": src, "dest_account_id": dst,
					"amount_cents": 1, "description": "crossfire",
				}, map[string]string{"X-Idempotency-Key": fmt.Sprintf("t3-%d-%d", w, i)})
				if resp.StatusCode != http.StatusCreated {
					atomic.AddInt64(&failures, 1)
					t.Errorf("worker %d iter %d: status %d (%v)", w, i, resp.StatusCode, body)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if failures != 0 {
		t.Fatalf("%d crossfire requests failed (possible deadlock/timeout)", failures)
	}
	gotA, gotB := e.balance(a), e.balance(b)
	if gotA+gotB != 2*initial {
		t.Fatalf("money not conserved: %d + %d != %d", gotA, gotB, 2*initial)
	}
	// 10 workers push A→B and 10 push B→A, 25 cents each way: net zero.
	if gotA != initial || gotB != initial {
		t.Fatalf("want %d/%d, got %d/%d", initial, initial, gotA, gotB)
	}
}

// T4 — replay and conflict semantics over real SQL.
func TestReplayAndConflict(t *testing.T) {
	e := newEnv(t)
	src := e.createAccount("0001", 5_000)
	dst := e.createAccount("0002", 0)

	payload := map[string]any{
		"source_account_id": src, "dest_account_id": dst,
		"amount_cents": 1_500, "description": "original",
	}
	key := map[string]string{"X-Idempotency-Key": "t4-key"}

	resp, first := e.do(http.MethodPost, "/api/pix", payload, key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first: want 201, got %d", resp.StatusCode)
	}
	resp, second := e.do(http.MethodPost, "/api/pix", payload, key)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: want 200 + header, got %d", resp.StatusCode)
	}
	if second["transaction_id"] != first["transaction_id"] || second["end_to_end_id"] != first["end_to_end_id"] {
		t.Fatal("replay must return the original transaction verbatim")
	}
	if got := e.balance(src); got != 3_500 {
		t.Fatalf("replay debited again: %d", got)
	}

	conflicting := map[string]any{
		"source_account_id": src, "dest_account_id": dst,
		"amount_cents": 9_999, "description": "original",
	}
	resp, body := e.do(http.MethodPost, "/api/pix", conflicting, key)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflict: want 409, got %d (%v)", resp.StatusCode, body)
	}
}

// T5 — insufficient funds is committed as a failed transaction and replays the
// same 402 (with header) under the same key.
func TestInsufficientFundsRecordedAndReplayed(t *testing.T) {
	e := newEnv(t)
	src := e.createAccount("0001", 100)
	dst := e.createAccount("0002", 0)

	payload := map[string]any{
		"source_account_id": src, "dest_account_id": dst,
		"amount_cents": 50_000, "description": "too much",
	}
	key := map[string]string{"X-Idempotency-Key": "t5-key"}

	resp, body := e.do(http.MethodPost, "/api/pix", payload, key)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("want 402, got %d (%v)", resp.StatusCode, body)
	}
	failedTx := body["error"].(map[string]any)["transaction_id"].(string)

	var status, reason string
	if err := e.db.QueryRow(`SELECT status, failure_reason FROM transactions WHERE id = $1`, failedTx).Scan(&status, &reason); err != nil {
		t.Fatalf("failed transfer not persisted: %v", err)
	}
	if status != "failed" || reason != "insufficient_funds" {
		t.Fatalf("want failed/insufficient_funds row, got %s/%s", status, reason)
	}

	resp, body = e.do(http.MethodPost, "/api/pix", payload, key)
	if resp.StatusCode != http.StatusPaymentRequired || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("402 replay: want 402 + header, got %d", resp.StatusCode)
	}
	if body["error"].(map[string]any)["transaction_id"].(string) != failedTx {
		t.Fatal("replayed 402 must reference the same recorded transaction")
	}
	if got := e.balance(src); got != 100 {
		t.Fatalf("balance must be untouched, got %d", got)
	}
}

// T6 — outbox: the webhook fails twice then accepts; the worker must retry
// with backoff and mark the event delivered with attempts == 3. The commit of
// the transfer itself never depends on the webhook being up.
func TestOutboxDeliversWithRetry(t *testing.T) {
	e := newEnv(t)

	var hits int64
	var received atomic.Value
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		if n <= 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		received.Store(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	src := e.createAccount("0001", 10_000)
	dst := e.createAccount("0002", 0)
	resp, body := e.do(http.MethodPost, "/api/pix", map[string]any{
		"source_account_id": src, "dest_account_id": dst,
		"amount_cents": 4_200, "description": "outbox",
	}, map[string]string{"X-Idempotency-Key": "t6-key"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("transfer: want 201, got %d (%v)", resp.StatusCode, body)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := &outbox.Worker{
		DB:          e.db,
		WebhookURL:  webhook.URL,
		Interval:    20 * time.Millisecond,
		BaseBackoff: 10 * time.Millisecond,
		MaxBackoff:  50 * time.Millisecond,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:     metrics.NewRegistry(),
	}
	go worker.Run(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for {
		var delivered sql.NullTime
		var attempts int
		err := e.db.QueryRow(`SELECT delivered_at, attempts FROM events LIMIT 1`).Scan(&delivered, &attempts)
		if err == nil && delivered.Valid {
			if attempts != 3 {
				t.Fatalf("want exactly 3 attempts (2 failures + 1 success), got %d", attempts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("event not delivered in time (attempts so far: %d, webhook hits: %d)", attempts, atomic.LoadInt64(&hits))
		}
		time.Sleep(25 * time.Millisecond)
	}

	raw, _ := received.Load().([]byte)
	var envelope struct {
		Type    string `json:"type"`
		Payload struct {
			TransactionID string `json:"transaction_id"`
			AmountCents   int64  `json:"amount_cents"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("webhook body: %v (%s)", err, raw)
	}
	if envelope.Type != "transfer.completed" || envelope.Payload.AmountCents != 4_200 {
		t.Fatalf("unexpected event payload: %s", raw)
	}
	if envelope.Payload.TransactionID != body["transaction_id"] {
		t.Fatal("event must reference the committed transaction")
	}
}
