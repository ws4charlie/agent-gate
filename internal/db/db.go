// Package db opens the Postgres pool and applies embedded migrations.
package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/migrations"
)

// Open connects and pings.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// migrateLock is an arbitrary constant: two processes migrating at once queue
// on it instead of racing on schema_migrations.
const migrateLock = 0x61675f6d // "ag_m"

// Migrate applies every migrations/NNNN_*.sql not yet recorded in
// schema_migrations, in filename order, each in its own transaction.
// It returns the versions applied by this call.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: migrate: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLock); err != nil {
		return nil, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLock)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}

	var applied []string
	for _, f := range files {
		version := strings.TrimSuffix(f, ".sql")
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
			return applied, err
		}
		if done {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, f)
		if err != nil {
			return applied, err
		}
		if err := applyOne(ctx, conn, version, string(body)); err != nil {
			return applied, fmt.Errorf("db: migration %s: %w", version, err)
		}
		applied = append(applied, version)
	}
	return applied, nil
}

func applyOne(ctx context.Context, conn *pgxpool.Conn, version, body string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Simple protocol so one file may hold many statements.
	if _, err := tx.Exec(ctx, body, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
