package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"pixledger/internal/domain"
)

type errorBody struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	TransactionID string `json:"transaction_id,omitempty"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload != nil {
		json.NewEncoder(w).Encode(payload)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// respondError maps domain sentinels to the contract's status/code table.
// Anything unmapped is a 500 with a generic message (details go to logs only).
func (s *Server) respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrMissingIdempotencyKey):
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", err.Error())
	case errors.Is(err, domain.ErrInvalidPayload):
		writeError(w, http.StatusBadRequest, "invalid_payload", err.Error())
	case errors.Is(err, domain.ErrInsufficientFunds):
		writeError(w, http.StatusPaymentRequired, "insufficient_funds", err.Error())
	case errors.Is(err, domain.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
	case errors.Is(err, domain.ErrNumberTaken):
		writeError(w, http.StatusConflict, "number_taken", err.Error())
	case errors.Is(err, domain.ErrInvalidAmount):
		writeError(w, http.StatusUnprocessableEntity, "invalid_amount", err.Error())
	case errors.Is(err, domain.ErrSameAccount):
		writeError(w, http.StatusUnprocessableEntity, "same_account", err.Error())
	case errors.Is(err, domain.ErrAccountNotFound):
		writeError(w, http.StatusNotFound, "account_not_found", err.Error())
	default:
		s.log.Error("internal error", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
