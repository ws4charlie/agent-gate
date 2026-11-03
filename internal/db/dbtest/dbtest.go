// Package dbtest gives each test its own freshly migrated Postgres database.
package dbtest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/db"
)

// New creates a randomly named database on the server in DATABASE_URL, runs the
// migrations, and drops it on cleanup, so tests can run in parallel.
// If DATABASE_URL is unset or unreachable the test is skipped, unless
// AG_REQUIRE_DB=1 (CI and hand verification), where it fails.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	require := os.Getenv("AG_REQUIRE_DB") == "1"
	unavailable := func(format string, args ...any) {
		t.Helper()
		if require {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		unavailable("DATABASE_URL not set")
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		unavailable("bad DATABASE_URL: %v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	admin, err := pgx.ConnectConfig(cctx, cfg)
	if err != nil {
		unavailable("database unreachable: %v", err)
	}
	defer admin.Close(ctx)

	name := "agentgate_t_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})

	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
