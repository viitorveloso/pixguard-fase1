package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ctxKey int

const (
	ctxKeyPattern ctxKey = iota
	ctxKeySubject
)

// patternHolder lets inner handlers report the matched route pattern back to
// the outer observe middleware (Go 1.22 has r.PathValue but not r.Pattern,
// which only landed in 1.23 — this keeps the module on 1.22).
type patternHolder struct{ pattern string }

// route wraps a handler and records its pattern for metrics/log labels, so
// cardinality stays bounded ("/api/accounts/{id}", never the raw path).
func route(pattern string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if holder, ok := r.Context().Value(ctxKeyPattern).(*patternHolder); ok {
			holder.pattern = pattern
		}
		h(w, r)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// observe assigns/propagates X-Request-ID, measures latency, records metrics
// and emits one structured log line per request.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)

		holder := &patternHolder{pattern: "unmatched"}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyPattern, holder))

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		elapsed := time.Since(start)
		s.metrics.ObserveHTTP(r.Method, holder.pattern, rec.status, elapsed)
		s.log.Info("request",
			slog.String("request_id", requestID),
			slog.String("method", r.Method),
			slog.String("route", holder.pattern),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
		)
	})
}

// recoverer turns panics into 500s instead of dropped connections.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", slog.Any("panic", rec), slog.String("path", r.URL.Path))
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// auth enforces Bearer JWT on /api/* routes.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pixledger"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed Authorization header")
			return
		}
		claims, err := s.codec.Verify(strings.TrimPrefix(authz, prefix))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pixledger", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKeySubject, claims.Sub)))
	}
}
