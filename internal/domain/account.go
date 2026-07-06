package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Account is the ledger's unit of balance. Money is always int64 cents:
// no floats anywhere near balances.
type Account struct {
	ID           string    `json:"id"`
	Number       string    `json:"number"`
	Holder       string    `json:"holder"`
	BalanceCents int64     `json:"balance_cents"`
	CreatedAt    time.Time `json:"created_at"`
}

// NewAccount validates input and builds an Account with a fresh UUID.
func NewAccount(number, holder string, initialBalanceCents int64, now time.Time) (Account, error) {
	number = strings.TrimSpace(number)
	holder = strings.TrimSpace(holder)
	if number == "" || holder == "" {
		return Account{}, ErrInvalidPayload
	}
	if initialBalanceCents < 0 {
		return Account{}, ErrInvalidAmount
	}
	return Account{
		ID:           uuid.NewString(),
		Number:       number,
		Holder:       holder,
		BalanceCents: initialBalanceCents,
		CreatedAt:    now.UTC(),
	}, nil
}
