package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"pixledger/internal/config"
	"pixledger/internal/httpapi"
	"pixledger/internal/jwt"
	"pixledger/internal/metrics"
	"pixledger/internal/outbox"
	"pixledger/internal/service"
	"pixledger/internal/store/postgres"
	"pixledger/migrations"
)

func main() {
	cfg := config.Load()

	level := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if cfg.JWTSecret == config.DefaultJWTSecret {
		log.Warn("using default JWT secret — set JWT_SECRET in production")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database connection failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer db.Close()

	if err := postgres.Migrate(ctx, db, migrations.FS); err != nil {
		log.Error("migrations failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log.Info("migrations applied")

	store := postgres.NewStore(db)
	ledger := service.NewLedger(store, cfg.ISPB, nil)
	reg := metrics.NewRegistry()
	codec := jwt.NewCodec(cfg.JWTSecret)
	api := httpapi.New(ledger, codec, reg, log)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	var wg sync.WaitGroup
	if cfg.WebhookURL != "" {
		worker := &outbox.Worker{
			DB:          db,
			WebhookURL:  cfg.WebhookURL,
			Interval:    cfg.OutboxPollInterval,
			BaseBackoff: cfg.OutboxBaseBackoff,
			MaxBackoff:  cfg.OutboxMaxBackoff,
			Log:         log,
			Metrics:     reg,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker.Run(ctx)
		}()
		log.Info("outbox worker started", slog.String("webhook_url", cfg.WebhookURL))
	} else {
		log.Info("WEBHOOK_URL not set — outbox worker disabled, events still recorded for audit")
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", slog.String("addr", server.Addr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		log.Error("server error", slog.String("error", err.Error()))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", slog.String("error", err.Error()))
	}
	stop()
	wg.Wait()
	log.Info("bye")
}
