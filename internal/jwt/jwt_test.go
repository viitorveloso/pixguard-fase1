package jwt

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundtrip(t *testing.T) {
	codec := NewCodec("secret-a")
	token, err := codec.Sign("lucas", time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := codec.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Sub != "lucas" {
		t.Fatalf("want sub lucas, got %q", claims.Sub)
	}
}

func TestExpiredToken(t *testing.T) {
	codec := NewCodec("secret-a")
	token, err := codec.Sign("lucas", -time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := codec.Verify(token); err != ErrExpiredToken {
		t.Fatalf("want ErrExpiredToken, got %v", err)
	}
}

func TestTamperedPayload(t *testing.T) {
	codec := NewCodec("secret-a")
	token, _ := codec.Sign("lucas", time.Hour)
	parts := strings.Split(token, ".")
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin","iat":1,"exp":99999999999}`))
	if _, err := codec.Verify(parts[0] + "." + forged + "." + parts[2]); err != ErrInvalidToken {
		t.Fatalf("want ErrInvalidToken for tampered payload, got %v", err)
	}
}

func TestWrongSecret(t *testing.T) {
	token, _ := NewCodec("secret-a").Sign("lucas", time.Hour)
	if _, err := NewCodec("secret-b").Verify(token); err != ErrInvalidToken {
		t.Fatalf("want ErrInvalidToken for wrong secret, got %v", err)
	}
}

func TestAlgNoneRejected(t *testing.T) {
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := enc.EncodeToString([]byte(`{"sub":"admin","iat":1,"exp":99999999999}`))
	// Classic alg=none attack: empty or arbitrary signature must never pass.
	for _, sig := range []string{"", enc.EncodeToString([]byte("x"))} {
		if _, err := NewCodec("secret-a").Verify(head + "." + body + "." + sig); err != ErrInvalidToken {
			t.Fatalf("alg=none accepted (sig=%q): got %v", sig, err)
		}
	}
}

func TestMalformedTokens(t *testing.T) {
	codec := NewCodec("secret-a")
	for _, token := range []string{"", "a", "a.b", "a.b.c.d", "not-base64.!.also"} {
		if _, err := codec.Verify(token); err != ErrInvalidToken {
			t.Fatalf("token %q: want ErrInvalidToken, got %v", token, err)
		}
	}
}

func TestMissingExpRejected(t *testing.T) {
	enc := base64.RawURLEncoding
	codec := NewCodec("secret-a")
	head := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := enc.EncodeToString([]byte(`{"sub":"lucas","iat":1}`))
	signingInput := head + "." + body
	token := signingInput + "." + enc.EncodeToString(codec.sign(signingInput))
	if _, err := codec.Verify(token); err != ErrInvalidToken {
		t.Fatalf("token without exp must be rejected, got %v", err)
	}
}
