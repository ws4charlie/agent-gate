package main

import (
	"context"
	"os"
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
