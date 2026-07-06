// tokengen mints HS256 dev tokens for hitting the API locally:
//
//	go run ./cmd/tokengen -sub lucas -ttl 24h
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"pixledger/internal/jwt"
)

func main() {
	sub := flag.String("sub", "dev", "token subject")
	ttl := flag.Duration("ttl", 24*time.Hour, "token lifetime")
	secret := flag.String("secret", "", "HS256 secret (default: JWT_SECRET env or dev default)")
	flag.Parse()

	key := *secret
	if key == "" {
		key = os.Getenv("JWT_SECRET")
	}
	if key == "" {
		key = "dev-secret-change-me"
	}

	token, err := jwt.NewCodec(key).Sign(*sub, *ttl)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(token)
}
