package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryRendersPrometheusFormat(t *testing.T) {
	reg := NewRegistry()
	reg.ObserveHTTP("POST", "/api/pix", 201, 12*time.Millisecond)
	reg.ObserveHTTP("POST", "/api/pix", 201, 40*time.Millisecond)
	reg.ObserveHTTP("GET", "/healthz", 200, 1*time.Millisecond)
	reg.IncTransfer("completed")
	reg.IncTransfer("failed_insufficient_funds")
	reg.IncOutbox("delivered")

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("unexpected content type %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`ledger_http_requests_total{method="POST",route="/api/pix",status="201"} 2`,
		`ledger_http_requests_total{method="GET",route="/healthz",status="200"} 1`,
		`ledger_transfers_total{result="completed"} 1`,
		`ledger_transfers_total{result="failed_insufficient_funds"} 1`,
		`ledger_outbox_deliveries_total{result="delivered"} 1`,
		`ledger_http_request_duration_seconds_count 3`,
		`ledger_http_request_duration_seconds_bucket{le="+Inf"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\n---\n%s", want, body)
		}
	}
}
