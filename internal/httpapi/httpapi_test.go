package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pixledger/internal/httpapi"
	"pixledger/internal/jwt"
	"pixledger/internal/metrics"
	"pixledger/internal/service"
	"pixledger/internal/store/memory"
)

type testAPI struct {
	server *httptest.Server
	token  string
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	codec := jwt.NewCodec("test-secret")
	ledger := service.NewLedger(memory.New(), "00000000", nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := httpapi.New(ledger, codec, metrics.NewRegistry(), log)

	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)

	token, err := codec.Sign("tester", time.Hour)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return &testAPI{server: server, token: token}
}

func (a *testAPI) do(t *testing.T, method, path string, body any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.server.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	var decoded map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		json.Unmarshal(raw, &decoded)
	}
	return resp, decoded
}

func (a *testAPI) createAccount(t *testing.T, number string, balance int64) string {
	t.Helper()
	resp, body := a.do(t, http.MethodPost, "/api/accounts", map[string]any{
		"number": number, "holder": "Holder " + number, "initial_balance_cents": balance,
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create account: want 201, got %d (%v)", resp.StatusCode, body)
	}
	return body["id"].(string)
}

func errCode(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code
	}
	return ""
}

func TestAuthRequired(t *testing.T) {
	api := newTestAPI(t)
	req, _ := http.NewRequest(http.MethodPost, api.server.URL+"/api/accounts", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}

	req2, _ := http.NewRequest(http.MethodPost, api.server.URL+"/api/accounts", strings.NewReader("{}"))
	req2.Header.Set("Authorization", "Bearer not-a-token")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 with garbage token, got %d", resp2.StatusCode)
	}
}

func TestHealthEndpoints(t *testing.T) {
	api := newTestAPI(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(api.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", path, resp.StatusCode)
		}
	}
}

func TestAccountLifecycle(t *testing.T) {
	api := newTestAPI(t)
	id := api.createAccount(t, "0001", 5_000)

	resp, body := api.do(t, http.MethodGet, "/api/accounts/"+id, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	if body["number"] != "0001" || body["balance_cents"].(float64) != 5_000 {
		t.Fatalf("unexpected account body: %v", body)
	}

	// Duplicate number → 409.
	resp, body = api.do(t, http.MethodPost, "/api/accounts", map[string]any{
		"number": "0001", "holder": "Dup", "initial_balance_cents": 0,
	}, nil)
	if resp.StatusCode != http.StatusConflict || errCode(body) != "number_taken" {
		t.Fatalf("duplicate number: want 409 number_taken, got %d %v", resp.StatusCode, body)
	}

	// Invalid payloads.
	resp, body = api.do(t, http.MethodPost, "/api/accounts", map[string]any{"number": "", "holder": ""}, nil)
	if resp.StatusCode != http.StatusBadRequest || errCode(body) != "invalid_payload" {
		t.Fatalf("empty payload: want 400 invalid_payload, got %d %v", resp.StatusCode, body)
	}
	resp, body = api.do(t, http.MethodPost, "/api/accounts", map[string]any{
		"number": "0002", "holder": "Neg", "initial_balance_cents": -10,
	}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || errCode(body) != "invalid_amount" {
		t.Fatalf("negative balance: want 422 invalid_amount, got %d %v", resp.StatusCode, body)
	}

	// Unknown fields rejected.
	resp, body = api.do(t, http.MethodPost, "/api/accounts", map[string]any{
		"number": "0003", "holder": "X", "initial_balance_cents": 1, "hacker": true,
	}, nil)
	if resp.StatusCode != http.StatusBadRequest || errCode(body) != "invalid_payload" {
		t.Fatalf("unknown field: want 400 invalid_payload, got %d %v", resp.StatusCode, body)
	}

	// Not found.
	resp, body = api.do(t, http.MethodGet, "/api/accounts/3b8e6d1c-0000-0000-0000-000000000000", nil, nil)
	if resp.StatusCode != http.StatusNotFound || errCode(body) != "account_not_found" {
		t.Fatalf("missing account: want 404 account_not_found, got %d %v", resp.StatusCode, body)
	}
}

func TestPixContract(t *testing.T) {
	api := newTestAPI(t)
	src := api.createAccount(t, "0001", 10_000)
	dst := api.createAccount(t, "0002", 0)

	payload := map[string]any{
		"source_account_id": src, "dest_account_id": dst,
		"amount_cents": 2_500, "description": "lunch",
	}

	// Missing idempotency key → 400.
	resp, body := api.do(t, http.MethodPost, "/api/pix", payload, nil)
	if resp.StatusCode != http.StatusBadRequest || errCode(body) != "missing_idempotency_key" {
		t.Fatalf("missing key: want 400 missing_idempotency_key, got %d %v", resp.StatusCode, body)
	}

	// First execution → 201 with e2e id.
	key := map[string]string{"X-Idempotency-Key": "pix-1"}
	resp, body = api.do(t, http.MethodPost, "/api/pix", payload, key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first pix: want 201, got %d %v", resp.StatusCode, body)
	}
	txID := body["transaction_id"].(string)
	if len(body["end_to_end_id"].(string)) != 32 {
		t.Fatalf("bad end_to_end_id: %v", body["end_to_end_id"])
	}
	if resp.Header.Get("Idempotent-Replayed") != "" {
		t.Fatal("first execution must not carry replay header")
	}

	// Replay → 200, replay header, same transaction.
	resp, body = api.do(t, http.MethodPost, "/api/pix", payload, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay: want 200, got %d %v", resp.StatusCode, body)
	}
	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("replay must set Idempotent-Replayed: true")
	}
	if body["transaction_id"] != txID {
		t.Fatalf("replay changed transaction id: %v vs %s", body["transaction_id"], txID)
	}

	// Same key, different payload → 409.
	changed := map[string]any{
		"source_account_id": src, "dest_account_id": dst,
		"amount_cents": 9_999, "description": "lunch",
	}
	resp, body = api.do(t, http.MethodPost, "/api/pix", changed, key)
	if resp.StatusCode != http.StatusConflict || errCode(body) != "idempotency_conflict" {
		t.Fatalf("conflict: want 409 idempotency_conflict, got %d %v", resp.StatusCode, body)
	}

	// Insufficient funds → 402 with recorded transaction_id, then replayed 402.
	poor := map[string]any{
		"source_account_id": src, "dest_account_id": dst,
		"amount_cents": 999_999, "description": "too much",
	}
	poorKey := map[string]string{"X-Idempotency-Key": "pix-poor"}
	resp, body = api.do(t, http.MethodPost, "/api/pix", poor, poorKey)
	if resp.StatusCode != http.StatusPaymentRequired || errCode(body) != "insufficient_funds" {
		t.Fatalf("insufficient: want 402 insufficient_funds, got %d %v", resp.StatusCode, body)
	}
	failedTx := body["error"].(map[string]any)["transaction_id"].(string)
	if failedTx == "" {
		t.Fatal("402 must expose the recorded transaction_id")
	}
	resp, body = api.do(t, http.MethodPost, "/api/pix", poor, poorKey)
	if resp.StatusCode != http.StatusPaymentRequired || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("402 replay: want 402 + replay header, got %d", resp.StatusCode)
	}
	if body["error"].(map[string]any)["transaction_id"].(string) != failedTx {
		t.Fatal("402 replay must return the same recorded transaction")
	}

	// 422s.
	for i, tc := range []struct {
		amount int64
		dest   string
		code   string
	}{
		{0, dst, "invalid_amount"},
		{-5, dst, "invalid_amount"},
		{100, src, "same_account"},
	} {
		resp, body = api.do(t, http.MethodPost, "/api/pix", map[string]any{
			"source_account_id": src, "dest_account_id": tc.dest, "amount_cents": tc.amount,
		}, map[string]string{"X-Idempotency-Key": fmt.Sprintf("pix-422-%d", i)})
		if resp.StatusCode != http.StatusUnprocessableEntity || errCode(body) != tc.code {
			t.Fatalf("case %d: want 422 %s, got %d %v", i, tc.code, resp.StatusCode, body)
		}
	}

	// Unknown destination → 404.
	resp, body = api.do(t, http.MethodPost, "/api/pix", map[string]any{
		"source_account_id": src, "dest_account_id": "3b8e6d1c-0000-0000-0000-000000000000", "amount_cents": 100,
	}, map[string]string{"X-Idempotency-Key": "pix-404"})
	if resp.StatusCode != http.StatusNotFound || errCode(body) != "account_not_found" {
		t.Fatalf("unknown dest: want 404 account_not_found, got %d %v", resp.StatusCode, body)
	}
}

func TestMetricsExposed(t *testing.T) {
	api := newTestAPI(t)
	src := api.createAccount(t, "0001", 1_000)
	dst := api.createAccount(t, "0002", 0)
	api.do(t, http.MethodPost, "/api/pix", map[string]any{
		"source_account_id": src, "dest_account_id": dst, "amount_cents": 100,
	}, map[string]string{"X-Idempotency-Key": "metrics-key"})

	resp, err := http.Get(api.server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	for _, want := range []string{
		"ledger_http_requests_total",
		`ledger_transfers_total{result="completed"} 1`,
		"ledger_http_request_duration_seconds_bucket",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("/metrics missing %q\n---\n%s", want, text)
		}
	}
}
