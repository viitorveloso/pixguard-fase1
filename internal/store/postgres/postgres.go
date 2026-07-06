// Package postgres implements the service.Store against PostgreSQL using
// database/sql + lib/pq only. All financial guarantees (atomic idempotency,
// ordered row locks, balance invariants) live inside ExecuteTransfer's single
// transaction in store.go.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"time"

	_ "github.com/lib/pq"
)

// Open connects with sane pool limits and retries the initial ping so the API
// container can start alongside a Postgres that is still booting.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(40)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		lastErr = db.PingContext(pingCtx)
		cancel()
		if lastErr == nil {
			return db, nil
		}
		select {
		case <-ctx.Done():
			db.Close()
			return nil, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	db.Close()
	return nil, fmt.Errorf("postgres unreachable after retries: %w", lastErr)
}

// migrationLockID is an arbitrary app-wide advisory lock key so concurrent
// instances never race applying migrations.
const migrationLockID = 723451

// Migrate applies every *.sql file from fsys in lexical order, each inside its
// own transaction, tracking applied versions in schema_migrations. A session
// advisory lock (held on a dedicated connection) serializes deployers.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("migrate: advisory lock: %w", err)
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID)

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("migrate: ensure schema_migrations: %w", err)
	}

	entries, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return fmt.Errorf("migrate: glob: %w", err)
	}
	sort.Strings(entries)

	for _, name := range entries {
		var exists bool
		if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", name).Scan(&exists); err != nil {
			return fmt.Errorf("migrate: check %s: %w", name, err)
		}
		if exists {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migrate: begin %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate: apply %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate: record %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate: commit %s: %w", name, err)
		}
	}
	return nil
}
