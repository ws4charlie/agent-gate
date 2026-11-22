package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRegistryCLI(t *testing.T) {
	c := newCLI(t)
	if _, err := c.run("registry", "add", "model", "gpt-4o", "--owner", "ml-team", "--purpose", "chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.run("registry", "add", "model", "gpt-4o"); err == nil {
		t.Fatal("add without owner accepted")
	}
	if _, err := c.run("registry", "add", "dataset", "x", "--owner", "o", "--purpose", "p"); err == nil {
		t.Fatal("bad kind accepted")
	}
	out, err := c.run("registry", "list")
	if err != nil || !strings.Contains(out, "gpt-4o") || !strings.Contains(out, "proposed") || !strings.Contains(out, "ml-team") {
		t.Fatalf("%q %v", out, err)
	}
	if out, _ = c.run("registry", "list", "--kind", "tool"); strings.Contains(out, "gpt-4o") {
		t.Fatalf("kind filter: %q", out)
	}
	if _, err := c.run("registry", "status", "model", "gpt-4o", "deprecated"); err != nil {
		t.Fatal(err)
	}
	if out, _ = c.run("registry", "list"); !strings.Contains(out, "deprecated") {
		t.Fatalf("%q", out)
	}
	// An update by add keeps the status.
	if _, err := c.run("registry", "add", "model", "gpt-4o", "--owner", "platform", "--purpose", "chat"); err != nil {
		t.Fatal(err)
	}
	if out, _ = c.run("registry", "list"); !strings.Contains(out, "platform") || !strings.Contains(out, "deprecated") {
		t.Fatalf("%q", out)
	}

	// Audit rows: the deprecated model and an unlisted tool are in use.
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for i, r := range [][2]string{{"model", "gpt-4o"}, {"mcp", "fs/read"}} {
		if _, err := pool.Exec(context.Background(), `INSERT INTO audit_events (request_id, kind, model, status, at) VALUES ($1, $2, $3, 'ok', now())`,
			"r"+string(rune('a'+i)), r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	out, err = c.run("registry", "gaps", "--since", "7d")
	if err != nil || !strings.Contains(out, "fs/read") || !strings.Contains(out, "deprecated") || !strings.Contains(out, "gpt-4o") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := c.run("registry", "remove", "model", "gpt-4o"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.run("registry", "remove", "model", "gpt-4o"); err == nil {
		t.Fatal("second remove accepted")
	}
	if _, err := c.run("registry", "gaps", "--since", "soon"); err == nil {
		t.Fatal("bad since accepted")
	}
}

func TestRegistryHistorySeedAndFields(t *testing.T) {
	c := newCLI(t)
	if _, err := c.run("registry", "add", "model", "m1", "--owner", "o", "--purpose", "p", "--risk", "high", "--version", "v1", "--actor", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.run("registry", "add", "model", "m1", "--owner", "o", "--purpose", "p", "--risk", "extreme"); err == nil {
		t.Fatal("bad risk accepted")
	}
	out, _ := c.run("registry", "list")
	if !strings.Contains(out, "high") || !strings.Contains(out, "v1") {
		t.Fatalf("%q", out)
	}
	if _, err := c.run("registry", "status", "model", "m1", "approved", "--actor", "bob"); err != nil {
		t.Fatal(err)
	}
	out, err := c.run("registry", "history", "model", "m1")
	if err != nil || !strings.Contains(out, "alice") || !strings.Contains(out, "bob") || !strings.Contains(out, "(new)") {
		t.Fatalf("%q %v", out, err)
	}
	if out, _ = c.run("registry", "history"); !strings.Contains(out, "m1") {
		t.Fatalf("%q", out)
	}
	// seed: an active agent with an exact model, a wildcard and a tool; a price table with one model.
	if _, err := c.run("agent", "create", "--account", c.acct, "--name", "bot", "--models", "seed-model,claude-*", "--tools", "srv/t1,srv/*"); err != nil {
		t.Fatal(err)
	}
	prices := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(prices, []byte(`{"models":{"priced-model":{"input":1,"output":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = c.run("registry", "seed", "--prices", prices, "--dry-run")
	if err != nil || !strings.Contains(out, "would add 3") || !strings.Contains(out, "priced-model") || !strings.Contains(out, "seed-model") ||
		!strings.Contains(out, "srv/t1") || !strings.Contains(out, "not seeded: wildcard model claude-*") || !strings.Contains(out, "not seeded: wildcard tool srv/*") {
		t.Fatalf("%q %v", out, err)
	}
	if out, _ = c.run("registry", "list"); strings.Contains(out, "seed-model") {
		t.Fatal("dry run wrote")
	}
	if out, err = c.run("registry", "seed", "--prices", prices); err != nil || !strings.Contains(out, "added 3") {
		t.Fatalf("%q %v", out, err)
	}
	if out, _ = c.run("registry", "seed", "--prices", prices); !strings.Contains(out, "added 0") {
		t.Fatalf("not idempotent: %q", out)
	}
	if out, _ = c.run("registry", "list"); !strings.Contains(out, "seeded from live config") || !strings.Contains(out, "m1") {
		t.Fatalf("%q", out)
	}
}

// Flags may come before, between or after the positional words, and a word a
// command does not take is an error instead of being dropped.
func TestRegistryArgsAnyOrder(t *testing.T) {
	c := newCLI(t)
	if _, err := c.run("registry", "add", "--owner", "o", "--purpose", "p", "model", "m1"); err != nil {
		t.Fatalf("flags first: %v", err)
	}
	if _, err := c.run("registry", "add", "model", "--owner", "o", "m2", "--purpose", "p"); err != nil {
		t.Fatalf("flags between: %v", err)
	}
	if _, err := c.run("registry", "status", "--notes", "why", "model", "m1", "approved", "--actor", "t"); err != nil {
		t.Fatalf("status flags around: %v", err)
	}
	out, err := c.run("registry", "history", "model", "m1")
	if err != nil || !strings.Contains(out, "why") || !strings.Contains(out, "approved") {
		t.Fatalf("history: %q %v", out, err)
	}
	// After "--" a word starting with "-" is a positional word, not a flag.
	if _, err := c.run("registry", "add", "--owner", "o", "--purpose", "p", "--", "model", "-dash"); err != nil {
		t.Fatalf("after --: %v", err)
	}
	if out, _ := c.run("registry", "list"); !strings.Contains(out, "-dash") {
		t.Fatalf("after --: %q", out)
	}
	if _, err := c.run("registry", "list", "--kind", "model", "extra"); err == nil {
		t.Fatal("list took a stray word")
	}
	if _, err := c.run("registry", "status", "model", "m1", "approved", "oops"); err == nil {
		t.Fatal("status took a fourth word")
	}
}
