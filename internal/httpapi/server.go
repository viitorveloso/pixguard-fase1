// Package httpapi wires the HTTP surface: routing (Go 1.22 method+wildcard
// patterns), JWT auth on /api/*, structured request logging, Prometheus
// metrics and panic recovery.
package httpapi

import (
	"log/slog"
	"net/http"

	"pixledger/internal/jwt"
	"pixledger/internal/metrics"
	"pixledger/internal/service"
)

type Server struct {
	ledger  *service.Ledger
	codec   *jwt.Codec
	metrics *metrics.Registry
	log     *slog.Logger
	handler http.Handler
}

func New(ledger *service.Ledger, codec *jwt.Codec, reg *metrics.Registry, log *slog.Logger) *Server {
	s := &Server{ledger: ledger, codec: codec, metrics: reg, log: log}

	mux := http.NewServeMux()

	// Operational endpoints: unauthenticated by design (probes/scrapers).
	mux.HandleFunc("GET /healthz", route("/healthz", s.handleHealthz))
	mux.HandleFunc("GET /readyz", route("/readyz", s.handleReadyz))
	mux.Handle("GET /metrics", route("/metrics", func(w http.ResponseWriter, r *http.Request) {
		reg.Handler().ServeHTTP(w, r)
	}))

	// Business endpoints: Bearer JWT required.
	mux.HandleFunc("POST /api/accounts", route("/api/accounts", s.auth(s.handleCreateAccount)))
	mux.HandleFunc("GET /api/accounts/{id}", route("/api/accounts/{id}", s.auth(s.handleGetAccount)))
	mux.HandleFunc("POST /api/pix", route("/api/pix", s.auth(s.handlePix)))

	s.handler = s.recoverer(s.observe(mux))
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }
