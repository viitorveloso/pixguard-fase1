package domain

import "errors"

// Sentinel errors shared across layers. The HTTP layer maps each one to a
// specific status code and machine-readable error code (see httpapi/respond.go).
var (
	ErrAccountNotFound       = errors.New("account not found")
	ErrInsufficientFunds     = errors.New("insufficient funds")
	ErrInvalidAmount         = errors.New("amount must be a positive integer of cents")
	ErrSameAccount           = errors.New("source and destination accounts must differ")
	ErrInvalidPayload        = errors.New("invalid payload")
	ErrMissingIdempotencyKey = errors.New("missing X-Idempotency-Key header")
	ErrIdempotencyConflict   = errors.New("idempotency key already used with a different payload")
	ErrNumberTaken           = errors.New("account number already taken")
)
