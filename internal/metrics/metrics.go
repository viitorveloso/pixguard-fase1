// Package metrics is a zero-dependency registry that renders the Prometheus
// text exposition format (version 0.0.4). It covers exactly what this service
// needs — counters with a few labels and one latency histogram — without
// pulling the full client library.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}

type Registry struct {
	mu           sync.Mutex
	httpRequests map[string]int64 // "method|route|status"
	transfers    map[string]int64 // result
	outbox       map[string]int64 // result
	bucketCounts []int64
	durationSum  float64
	durationN    int64
}

func NewRegistry() *Registry {
	return &Registry{
		httpRequests: make(map[string]int64),
		transfers:    make(map[string]int64),
		outbox:       make(map[string]int64),
		bucketCounts: make([]int64, len(durationBuckets)),
	}
}

// ObserveHTTP records one request in the counter and the latency histogram.
func (r *Registry) ObserveHTTP(method, route string, status int, elapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.httpRequests[fmt.Sprintf("%s|%s|%d", method, route, status)]++
	secs := elapsed.Seconds()
	for i, upper := range durationBuckets {
		if secs <= upper {
			r.bucketCounts[i]++
		}
	}
	r.durationSum += secs
	r.durationN++
}

// IncTransfer counts business outcomes: completed | failed_insufficient_funds.
func (r *Registry) IncTransfer(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transfers[result]++
}

// IncOutbox counts webhook delivery outcomes: delivered | retried.
func (r *Registry) IncOutbox(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outbox[result]++
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, r.render())
	})
}

func (r *Registry) render() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder

	b.WriteString("# HELP ledger_http_requests_total Total HTTP requests.\n")
	b.WriteString("# TYPE ledger_http_requests_total counter\n")
	for _, key := range sortedKeys(r.httpRequests) {
		parts := strings.SplitN(key, "|", 3)
		fmt.Fprintf(&b, "ledger_http_requests_total{method=%q,route=%q,status=%q} %d\n",
			parts[0], parts[1], parts[2], r.httpRequests[key])
	}

	b.WriteString("# HELP ledger_transfers_total Transfer outcomes.\n")
	b.WriteString("# TYPE ledger_transfers_total counter\n")
	for _, key := range sortedKeys(r.transfers) {
		fmt.Fprintf(&b, "ledger_transfers_total{result=%q} %d\n", key, r.transfers[key])
	}

	b.WriteString("# HELP ledger_outbox_deliveries_total Outbox webhook delivery outcomes.\n")
	b.WriteString("# TYPE ledger_outbox_deliveries_total counter\n")
	for _, key := range sortedKeys(r.outbox) {
		fmt.Fprintf(&b, "ledger_outbox_deliveries_total{result=%q} %d\n", key, r.outbox[key])
	}

	b.WriteString("# HELP ledger_http_request_duration_seconds HTTP request latency.\n")
	b.WriteString("# TYPE ledger_http_request_duration_seconds histogram\n")
	for i, upper := range durationBuckets {
		fmt.Fprintf(&b, "ledger_http_request_duration_seconds_bucket{le=\"%g\"} %d\n", upper, r.bucketCounts[i])
	}
	fmt.Fprintf(&b, "ledger_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", r.durationN)
	fmt.Fprintf(&b, "ledger_http_request_duration_seconds_sum %g\n", r.durationSum)
	fmt.Fprintf(&b, "ledger_http_request_duration_seconds_count %d\n", r.durationN)

	return b.String()
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
