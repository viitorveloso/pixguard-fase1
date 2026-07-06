// Package config loads all settings from environment variables with sane
// local-dev defaults (twelve-factor style; docker-compose and CI override).
package config

import (
	"os"
	"time"
)

const DefaultJWTSecret = "dev-secret-change-me"

type Config struct {
	Port               string
	DatabaseURL        string
	JWTSecret          string
	ISPB               string
	WebhookURL         string
	OutboxPollInterval time.Duration
	OutboxBaseBackoff  time.Duration
	OutboxMaxBackoff   time.Duration
	ShutdownTimeout    time.Duration
	LogLevel           string
}

func Load() Config {
	return Config{
		Port:               getenv("PORT", "8080"),
		DatabaseURL:        getenv("DATABASE_URL", "postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable"),
		JWTSecret:          getenv("JWT_SECRET", DefaultJWTSecret),
		ISPB:               getenv("ISPB", "00000000"),
		WebhookURL:         os.Getenv("WEBHOOK_URL"),
		OutboxPollInterval: getduration("OUTBOX_POLL_INTERVAL", 2*time.Second),
		OutboxBaseBackoff:  getduration("OUTBOX_BASE_BACKOFF", 1*time.Second),
		OutboxMaxBackoff:   getduration("OUTBOX_MAX_BACKOFF", 60*time.Second),
		ShutdownTimeout:    getduration("SHUTDOWN_TIMEOUT", 10*time.Second),
		LogLevel:           getenv("LOG_LEVEL", "info"),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getduration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
