package domain

import (
	"regexp"
	"testing"
	"time"
)

func TestNewEndToEndIDFormat(t *testing.T) {
	now := time.Date(2026, 7, 6, 14, 30, 0, 0, time.UTC)
	id := NewEndToEndID("12345678", now)

	if len(id) != 32 {
		t.Fatalf("want 32 chars, got %d (%q)", len(id), id)
	}
	pattern := regexp.MustCompile(`^E\d{8}\d{12}[A-Za-z0-9]{11}$`)
	if !pattern.MatchString(id) {
		t.Fatalf("id %q does not match Bacen format", id)
	}
	if id[1:9] != "12345678" {
		t.Fatalf("ISPB segment wrong: %q", id[1:9])
	}
	if id[9:21] != "202607061430" {
		t.Fatalf("timestamp segment wrong: %q", id[9:21])
	}
}

func TestNewEndToEndIDUniqueness(t *testing.T) {
	now := time.Now()
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewEndToEndID("00000000", now)
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate end-to-end id generated: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewAccountValidation(t *testing.T) {
	now := time.Now()

	if _, err := NewAccount("", "Lucas", 100, now); err != ErrInvalidPayload {
		t.Fatalf("empty number: want ErrInvalidPayload, got %v", err)
	}
	if _, err := NewAccount("0001", "   ", 100, now); err != ErrInvalidPayload {
		t.Fatalf("blank holder: want ErrInvalidPayload, got %v", err)
	}
	if _, err := NewAccount("0001", "Lucas", -1, now); err != ErrInvalidAmount {
		t.Fatalf("negative balance: want ErrInvalidAmount, got %v", err)
	}

	account, err := NewAccount("  0001 ", " Lucas ", 5000, now)
	if err != nil {
		t.Fatalf("valid account: unexpected error %v", err)
	}
	if account.ID == "" {
		t.Fatal("expected generated ID")
	}
	if account.Number != "0001" || account.Holder != "Lucas" {
		t.Fatalf("expected trimmed fields, got %q / %q", account.Number, account.Holder)
	}
	if account.BalanceCents != 5000 {
		t.Fatalf("want balance 5000, got %d", account.BalanceCents)
	}
}
