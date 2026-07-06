package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"pixledger/internal/domain"
	"pixledger/internal/service"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// decodeJSON enforces a strict contract: bounded body, unknown fields
// rejected, exactly one JSON object.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return domain.ErrInvalidPayload
	}
	if dec.More() {
		return domain.ErrInvalidPayload
	}
	// Drain any trailing whitespace; anything else was caught by More().
	io.Copy(io.Discard, r.Body)
	return nil
}

type createAccountRequest struct {
	Number              string `json:"number"`
	Holder              string `json:"holder"`
	InitialBalanceCents int64  `json:"initial_balance_cents"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.respondError(w, err)
		return
	}
	account, err := s.ledger.CreateAccount(r.Context(), service.CreateAccountInput{
		Number:              req.Number,
		Holder:              req.Holder,
		InitialBalanceCents: req.InitialBalanceCents,
	})
	if err != nil {
		s.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, account)
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	account, err := s.ledger.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		s.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

type pixRequest struct {
	SourceAccountID string `json:"source_account_id"`
	DestAccountID   string `json:"dest_account_id"`
	AmountCents     int64  `json:"amount_cents"`
	Description     string `json:"description"`
}

type transferResponse struct {
	TransactionID   string    `json:"transaction_id"`
	EndToEndID      string    `json:"end_to_end_id"`
	Status          string    `json:"status"`
	SourceAccountID string    `json:"source_account_id"`
	DestAccountID   string    `json:"dest_account_id"`
	AmountCents     int64     `json:"amount_cents"`
	Description     string    `json:"description"`
	CreatedAt       time.Time `json:"created_at"`
}

const replayHeader = "Idempotent-Replayed"

func (s *Server) handlePix(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("X-Idempotency-Key")
	if key == "" {
		s.respondError(w, domain.ErrMissingIdempotencyKey)
		return
	}

	var req pixRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.respondError(w, err)
		return
	}

	outcome, err := s.ledger.Transfer(r.Context(), service.TransferInput{
		IdempotencyKey:  key,
		SourceAccountID: req.SourceAccountID,
		DestAccountID:   req.DestAccountID,
		AmountCents:     req.AmountCents,
		Description:     req.Description,
	})
	if err != nil {
		s.respondError(w, err)
		return
	}

	if outcome.Replayed {
		w.Header().Set(replayHeader, "true")
	}

	transfer := outcome.Transfer

	// Business failure committed by the store: 402 with the recorded
	// transaction so clients can reconcile, replayed deterministically.
	if transfer.Status == domain.StatusFailed {
		if !outcome.Replayed {
			s.metrics.IncTransfer("failed_insufficient_funds")
		}
		writeJSON(w, http.StatusPaymentRequired, errorEnvelope{Error: errorBody{
			Code:          "insufficient_funds",
			Message:       domain.ErrInsufficientFunds.Error(),
			TransactionID: transfer.ID,
		}})
		return
	}

	status := http.StatusCreated
	if outcome.Replayed {
		status = http.StatusOK
	} else {
		s.metrics.IncTransfer("completed")
	}
	writeJSON(w, status, transferResponse{
		TransactionID:   transfer.ID,
		EndToEndID:      transfer.EndToEndID,
		Status:          string(transfer.Status),
		SourceAccountID: transfer.SourceAccountID,
		DestAccountID:   transfer.DestAccountID,
		AmountCents:     transfer.AmountCents,
		Description:     transfer.Description,
		CreatedAt:       transfer.CreatedAt,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.ledger.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
