// Package jwt implements a minimal HS256 codec with the standard library
// only. Scope is deliberately small: sign and verify tokens for this API.
// Header alg is checked explicitly (alg=none and friends are rejected) and
// signatures are compared in constant time.
package jwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrExpiredToken = errors.New("token expired")
)

type Codec struct {
	secret []byte
}

func NewCodec(secret string) *Codec { return &Codec{secret: []byte(secret)} }

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Claims is the minimal claim set this API needs.
type Claims struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

func (c *Codec) Sign(sub string, ttl time.Duration) (string, error) {
	now := time.Now()
	head, err := json.Marshal(header{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(Claims{Sub: sub, Iat: now.Unix(), Exp: now.Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(head) + "." + enc.EncodeToString(body)
	return signingInput + "." + enc.EncodeToString(c.sign(signingInput)), nil
}

func (c *Codec) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}
	enc := base64.RawURLEncoding

	headBytes, err := enc.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var head header
	if err := json.Unmarshal(headBytes, &head); err != nil || head.Alg != "HS256" {
		// Rejecting anything but HS256 kills alg-confusion attacks (alg=none).
		return Claims{}, ErrInvalidToken
	}

	expected := c.sign(parts[0] + "." + parts[1])
	got, err := enc.DecodeString(parts[2])
	if err != nil || !hmac.Equal(expected, got) {
		return Claims{}, ErrInvalidToken
	}

	claimBytes, err := enc.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var claims Claims
	if err := json.Unmarshal(claimBytes, &claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if claims.Exp == 0 {
		return Claims{}, ErrInvalidToken
	}
	if time.Now().Unix() >= claims.Exp {
		return Claims{}, ErrExpiredToken
	}
	return claims, nil
}

func (c *Codec) sign(input string) []byte {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(input))
	return mac.Sum(nil)
}
