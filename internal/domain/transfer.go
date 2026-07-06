package domain

import (
	"crypto/rand"
	"fmt"
	"time"
)

// TransferStatus is persisted as-is; failed transfers are first-class records,
// not discarded attempts (a business decision such as insufficient funds must
// replay deterministically under the same idempotency key).
type TransferStatus string

const (
	StatusCompleted TransferStatus = "completed"
	StatusFailed    TransferStatus = "failed"
)

// Transfer is a Pix transfer between two internal accounts.
type Transfer struct {
	ID              string         `json:"transaction_id"`
	SourceAccountID string         `json:"source_account_id"`
	DestAccountID   string         `json:"dest_account_id"`
	AmountCents     int64          `json:"amount_cents"`
	Description     string         `json:"description"`
	Status          TransferStatus `json:"status"`
	FailureReason   string         `json:"failure_reason,omitempty"`
	EndToEndID      string         `json:"end_to_end_id"`
	CreatedAt       time.Time      `json:"created_at"`
}

const e2eAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// NewEndToEndID builds a Bacen-style EndToEndId:
// "E" + ISPB (8 digits) + yyyyMMddHHmm + 11 alphanumeric chars = 32 chars.
// The random suffix uses crypto/rand — collisions across instances would be a
// financial incident, math/rand is not acceptable here.
func NewEndToEndID(ispb string, now time.Time) string {
	buf := make([]byte, 11)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing means the process cannot safely mint IDs.
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	for i, b := range buf {
		buf[i] = e2eAlphabet[int(b)%len(e2eAlphabet)]
	}
	return "E" + ispb + now.UTC().Format("200601021504") + string(buf)
}
