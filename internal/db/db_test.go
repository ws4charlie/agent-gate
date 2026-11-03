package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ws4charlie/agent-gate/internal/db"
	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
)

func TestMigrationsApplyOnFreshDB(t *testing.T) {
	pool := dbtest.New(t) // already migrated once
	ctx := context.Background()
	for _, tbl := range []string{"accounts", "holds", "ledger_entries", "schema_migrations"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, tbl).Scan(&ok); err != nil || !ok {
			t.Errorf("table %s missing (err %v)", tbl, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '0001_ledger'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("0001 not recorded: n=%d err=%v", n, err)
	}
	applied, err := db.Migrate(ctx, pool)
	if err != nil || len(applied) != 0 {
		t.Fatalf("second Migrate should be a no-op: %v %v", applied, err)
	}
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// I1: the DB, not only Go code, refuses held > balance.
func TestCheckRejectsHeldAboveBalance_I1(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (name, balance_nanos) VALUES ('a', 100) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `UPDATE accounts SET held_nanos = 101 WHERE id = $1`, id)
	if pgCode(err) != "23514" { // check_violation
		t.Fatalf("held > balance: want check_violation, got %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE accounts SET held_nanos = 100 WHERE id = $1`, id); err != nil {
		t.Fatalf("held == balance must be allowed: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE accounts SET held_nanos = -1 WHERE id = $1`, id); pgCode(err) != "23514" {
		t.Fatalf("negative held: want check_violation, got %v", err)
	}
}

func TestLedgerEntriesAppendOnly(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (name) VALUES ('a') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_entries (account_id, kind, amount_nanos, balance_after, held_after, idem_key)
		VALUES ($1, 'deposit', 5, 5, 0, 'k1')`, id); err != nil {
		t.Fatalf("insert must work: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE ledger_entries SET memo = 'x'`,
		`DELETE FROM ledger_entries`,
		`TRUNCATE ledger_entries`,
	} {
		if _, err := pool.Exec(ctx, stmt); pgCode(err) != "23001" { // restrict_violation
			t.Errorf("%s: want restrict_violation, got %v", stmt, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("row must survive: n=%d err=%v", n, err)
	}
}

func TestEntryIdempotencyUnique_I4(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	var id string
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (name) VALUES ('a') RETURNING id`).Scan(&id)
	ins := `INSERT INTO ledger_entries (account_id, kind, amount_nanos, balance_after, held_after, idem_key) VALUES ($1, $2, 1, 1, 0, 'k')`
	if _, err := pool.Exec(ctx, ins, id, "deposit"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, ins, id, "deposit"); pgCode(err) != "23505" {
		t.Fatalf("dup (account,key,kind): want unique_violation, got %v", err)
	}
	if _, err := pool.Exec(ctx, ins, id, "hold"); err != nil {
		t.Fatalf("same key, different kind is allowed: %v", err)
	}
}
