// Package outbox implements the transactional outbox consumer. Events are
// written in the SAME transaction as the transfer (store.go), so the API
// commit never waits on the network. This worker polls pending rows with
// FOR UPDATE SKIP LOCKED (safe with multiple replicas) and POSTs them to the
// configured webhook with exponential backoff + jitter. Delivery is
// at-least-once; consumers deduplicate by end_to_end_id / event id.
package outbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"time"

	"pixledger/internal/metrics"
)

type Worker struct {
	DB          *sql.DB
	WebhookURL  string
	Interval    time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	BatchSize   int
	Client      *http.Client
	Log         *slog.Logger
	Metrics     *metrics.Registry
}

type event struct {
	ID        string
	Type      string
	Payload   []byte
	Attempts  int
	CreatedAt time.Time
}

type envelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
	Attempt   int             `json:"attempt"`
}

// Run polls until ctx is cancelled. Errors are logged, never fatal — the
// outbox must survive webhook flakiness by definition.
func (w *Worker) Run(ctx context.Context) {
	if w.BatchSize <= 0 {
		w.BatchSize = 20
	}
	if w.Client == nil {
		w.Client = &http.Client{Timeout: 5 * time.Second}
	}
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.processBatch(ctx); err != nil {
				w.Log.Error("outbox batch failed", slog.String("error", err.Error()))
			}
		}
	}
}

func (w *Worker) processBatch(ctx context.Context) error {
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, type, payload, attempts, created_at
		   FROM events
		  WHERE delivered_at IS NULL AND next_attempt_at <= now()
		  ORDER BY created_at
		  LIMIT $1
		  FOR UPDATE SKIP LOCKED`, w.BatchSize)
	if err != nil {
		return fmt.Errorf("select pending: %w", err)
	}
	var batch []event
	for rows.Next() {
		var ev event
		if err := rows.Scan(&ev.ID, &ev.Type, &ev.Payload, &ev.Attempts, &ev.CreatedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	if len(batch) == 0 {
		return tx.Commit()
	}

	for _, ev := range batch {
		if w.deliver(ctx, ev) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE events SET delivered_at = now(), attempts = attempts + 1 WHERE id = $1`, ev.ID); err != nil {
				return fmt.Errorf("mark delivered: %w", err)
			}
			w.Metrics.IncOutbox("delivered")
			w.Log.Info("event delivered", slog.String("event_id", ev.ID), slog.Int("attempt", ev.Attempts+1))
		} else {
			backoff := w.backoffFor(ev.Attempts + 1)
			if _, err := tx.ExecContext(ctx,
				`UPDATE events SET attempts = attempts + 1, next_attempt_at = now() + make_interval(secs => $2) WHERE id = $1`,
				ev.ID, backoff.Seconds()); err != nil {
				return fmt.Errorf("schedule retry: %w", err)
			}
			w.Metrics.IncOutbox("retried")
			w.Log.Warn("event delivery failed, scheduled retry",
				slog.String("event_id", ev.ID),
				slog.Int("attempt", ev.Attempts+1),
				slog.Duration("backoff", backoff))
		}
	}
	return tx.Commit()
}

func (w *Worker) deliver(ctx context.Context, ev event) bool {
	body, err := json.Marshal(envelope{
		ID:        ev.ID,
		Type:      ev.Type,
		CreatedAt: ev.CreatedAt,
		Payload:   ev.Payload,
		Attempt:   ev.Attempts + 1,
	})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// backoffFor grows base * 2^(attempt-1), capped, with ±20% jitter to avoid
// thundering herds when a webhook recovers.
func (w *Worker) backoffFor(attempt int) time.Duration {
	backoff := float64(w.BaseBackoff) * math.Pow(2, float64(attempt-1))
	if capped := float64(w.MaxBackoff); backoff > capped {
		backoff = capped
	}
	jitter := 0.8 + rand.Float64()*0.4
	return time.Duration(backoff * jitter)
}
