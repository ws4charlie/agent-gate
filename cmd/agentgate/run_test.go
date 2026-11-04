package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/db"
	"github.com/ws4charlie/agent-gate/internal/policy"
	runs "github.com/ws4charlie/agent-gate/internal/run"
)

func TestRunListShowKill(t *testing.T) {
	c := newCLI(t)
	ctx := context.Background()
	_, a, err := c.st.Create(ctx, c.acct, "bot", policy.DefaultScope())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, os.Getenv("DATABASE_URL")) // newCLI points it at the test database
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	st := runs.New(pool)
	if _, err := st.Open(ctx, "job-9", a.ID, c.acct); err != nil {
		t.Fatal(err)
	}
	if err := st.Begin(ctx, "job-9", "tool", "req-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.End(ctx, "req-1", "fake/echo", "", "ok", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err := c.run("run", "list")
	if err != nil || !strings.Contains(out, "job-9") || !strings.Contains(out, "active") {
		t.Fatalf("list: %q %v", out, err)
	}
	out, err = c.run("run", "show", "job-9")
	if err != nil || !strings.Contains(out, "fake/echo") || !strings.Contains(out, "tool") {
		t.Fatalf("show: %q %v", out, err)
	}
	if out, err = c.run("run", "kill", "job-9"); err != nil || !strings.Contains(out, "killed run job-9") {
		t.Fatalf("kill: %q %v", out, err)
	}
	if r, _ := st.Get(ctx, "job-9"); r.Status != runs.Killed {
		t.Fatalf("status %s", r.Status)
	}
	if _, err = c.run("run", "kill", "nope"); err == nil {
		t.Fatal("killing an unknown run must fail")
	}
	if _, err = c.run("run"); err == nil {
		t.Fatal("run needs a subcommand")
	}
}
