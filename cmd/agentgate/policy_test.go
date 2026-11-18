package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

// cli runs one subcommand against a fresh test database.
type cli struct {
	t    *testing.T
	acct string
	st   *agents.Store
	q    *approval.Queue
	led  *ledger.Postgres
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	pool := dbtest.New(t)
	cc := pool.Config().ConnConfig
	t.Setenv("DATABASE_URL", fmt.Sprintf("host=%s port=%d user=%s dbname=%s", cc.Host, cc.Port, cc.User, cc.Database))
	t.Setenv("AG_CONFIG", "")
	c := &cli{t: t, st: agents.NewStore(pool), led: ledger.New(pool)}
	c.q = &approval.Queue{Store: approval.NewStore(pool), Ledger: c.led}
	var err error
	if c.acct, err = c.led.CreateAccount(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.led.Deposit(context.Background(), c.acct, 1_000_000_000, "dep", ""); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *cli) run(args ...string) (string, error) {
	c.t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	if code != 0 {
		return out.String(), fmt.Errorf("exit %d: %s", code, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func TestAgentCreateListRevoke(t *testing.T) {
	c := newCLI(t)
	ctx := context.Background()
	out, err := c.run("agent", "create", "--account", c.acct, "--name", "bot", "--models", "claude-*,gpt-4o", "--tools", "echo",
		"--payees", "0x70997970C51812dc3A010C7d01b50e0d17dc79C8", "--daily", "5", "--task", "1", "--velocity", "0.5", "--approval-over", "0.25")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimSpace(out)
	a, err := c.st.Authenticate(ctx, secret)
	if err != nil || a.Name != "bot" || a.AccountID != c.acct {
		t.Fatalf("%+v %v", a, err)
	}
	sc := a.Scope
	if len(sc.Models) != 2 || sc.Models[0] != "claude-*" || sc.Tools[0] != "echo" || len(sc.Payees) != 1 ||
		sc.DailyBudget != 5_000_000_000 || sc.TaskBudget != 1_000_000_000 || sc.VelocityPerMinute != 500_000_000 || sc.ApprovalThreshold != 250_000_000 {
		t.Fatalf("scope %+v", sc)
	}
	list, err := c.run("agent", "list")
	if err != nil || !strings.Contains(list, "bot") || !strings.Contains(list, "active") || strings.Contains(list, secret) {
		t.Fatalf("list %q %v", list, err)
	}
	if _, err := c.run("agent", "create", "--account", c.acct, "--name", "bot"); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := c.run("agent", "create", "--account", c.acct, "--name", "bad", "--daily", "lots"); err == nil {
		t.Fatal("bad amount accepted")
	}
	if _, err := c.run("agent", "revoke", "bot"); err != nil {
		t.Fatal(err)
	}
	if a, _ := c.st.Authenticate(ctx, secret); a.Status != policy.StatusRevoked {
		t.Fatalf("status %s", a.Status)
	}
	if _, err := c.run("agent", "revoke", "nobody"); err == nil {
		t.Fatal("revoked a ghost")
	}
}

func TestKillAndResume(t *testing.T) {
	c := newCLI(t)
	ctx := context.Background()
	_, a, _ := c.st.Create(ctx, c.acct, "k", policy.DefaultScope())
	if out, err := c.run("kill", "--agent", "k"); err != nil || !strings.Contains(out, "killed") {
		t.Fatalf("%q %v", out, err)
	}
	if got, _ := c.st.Get(ctx, a.ID); got.Status != policy.StatusKilled {
		t.Fatalf("status %s", got.Status)
	}
	if _, err := c.run("kill", "--agent", "k", "--resume"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.st.Get(ctx, a.ID); got.Status != policy.StatusActive {
		t.Fatalf("status %s", got.Status)
	}
	if _, err := c.run("kill"); err == nil {
		t.Fatal("kill without --agent")
	}
}

func TestApproveCLI(t *testing.T) {
	c := newCLI(t)
	ctx := context.Background()
	_, a, _ := c.st.Create(ctx, c.acct, "k", policy.DefaultScope())
	queue := func(key string) approval.Approval {
		h, err := c.led.Reserve(ctx, ledger.ReserveReq{AccountID: c.acct, Amount: 5, IdemKey: key, Kind: ledger.KindModel, TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		ap, _, err := c.q.Create(ctx, approval.Approval{AgentID: a.ID, AccountID: c.acct, IdemKey: key, RequestID: "r", Kind: "model",
			Subject: "m", Amount: 5, Fingerprint: "fp", HoldID: h.ID}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return ap
	}
	one, two := queue("a"), queue("b")

	out, err := c.run("approve", "list")
	if err != nil || !strings.Contains(out, one.ID) || !strings.Contains(out, two.ID) || !strings.Contains(out, "k") {
		t.Fatalf("%q %v", out, err)
	}
	if out, err = c.run("approve", "allow", one.ID, "--by", "alice"); err != nil || !strings.Contains(out, "alice") {
		t.Fatalf("%q %v", out, err)
	}
	if got, _ := c.q.Get(ctx, one.ID); got.Status != approval.Approved || got.DecidedBy != "alice" {
		t.Fatalf("%+v", got)
	}
	if _, err := c.run("approve", "deny", two.ID, "--by", "bob"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.q.Get(ctx, two.ID); got.Status != approval.Denied || got.DecidedBy != "bob" {
		t.Fatalf("%+v", got)
	}
	if b, _ := c.led.Balance(ctx, c.acct); b.Held != 5 { // deny released its hold, allow kept its own
		t.Fatalf("held %d", b.Held)
	}
	if out, _ = c.run("approve", "list"); strings.Contains(out, one.ID) || strings.Contains(out, two.ID) {
		t.Fatalf("decided entries still pending: %q", out)
	}
	if out, _ = c.run("approve", "list", "--status", "all"); !strings.Contains(out, one.ID) || !strings.Contains(out, two.ID) {
		t.Fatalf("%q", out)
	}
	if _, err := c.run("approve", "allow", one.ID, "--by", "alice"); err == nil {
		t.Fatal("approved twice")
	}
	if _, err := c.run("approve", "allow"); err == nil {
		t.Fatal("allow without an id")
	}
	t.Setenv("AG_APPROVER", "carol")
	three := queue("c")
	if _, err := c.run("approve", "allow", three.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.q.Get(ctx, three.ID); got.DecidedBy != "carol" {
		t.Fatalf("approver %q", got.DecidedBy)
	}
}

func TestAgentSpiffeAndAPIKey(t *testing.T) {
	c := newCLI(t)
	if _, err := c.run("agent", "create", "--account", c.acct, "--name", "bot"); err != nil {
		t.Fatal(err)
	}
	const id = "spiffe://example.org/bot"
	if out, err := c.run("agent", "set-spiffe", "bot", id); err != nil || !strings.Contains(out, id) {
		t.Fatalf("set-spiffe: %q %v", out, err)
	}
	if out, _ := c.run("agent", "list"); !strings.Contains(out, id) {
		t.Fatalf("list lacks the id: %s", out)
	}
	if _, err := c.run("agent", "set-spiffe", "bot", "https://nope"); err == nil {
		t.Fatal("invalid id accepted")
	}
	if out, err := c.run("agent", "api-key", "bot", "off"); err != nil || !strings.Contains(out, "off") {
		t.Fatalf("api-key off: %q %v", out, err)
	}
	if _, err := c.run("agent", "api-key", "bot", "maybe"); err == nil {
		t.Fatal("bad api-key value accepted")
	}
	if _, err := c.run("agent", "api-key", "bot", "on"); err != nil {
		t.Fatal(err)
	}
	if out, err := c.run("agent", "set-spiffe", "bot", "--clear"); err != nil || !strings.Contains(out, "cleared") {
		t.Fatalf("--clear: %q %v", out, err)
	}
	if _, err := c.run("agent", "set-spiffe", "bot", id); err != nil {
		t.Fatal(err)
	}
	if out, err := c.run("agent", "set-spiffe", "bot", ""); err != nil || !strings.Contains(out, "cleared") {
		t.Fatalf(`"" : %q %v`, out, err)
	}
	if _, err := c.run("agent", "set-spiffe", "nobody", id); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func TestParseLifetime(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "0": 0, "never": 0, "90d": 90 * 24 * time.Hour, "24h": 24 * time.Hour, "30m": 30 * time.Minute} {
		if got, err := parseLifetime(in); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"x", "-1h", "d", "1.5d", "-2d"} {
		if _, err := parseLifetime(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestAgentKeyExpiryAndRotateCLI(t *testing.T) {
	c := newCLI(t)
	ctx := context.Background()
	key, err := c.run("agent", "create", "--account", c.acct, "--name", "bot", "--key-ttl", "90d")
	if err != nil {
		t.Fatal(err)
	}
	key = strings.TrimSpace(key)
	a, err := c.st.Get(ctx, "bot")
	if err != nil || a.KeyExpiresAt == nil {
		t.Fatalf("create --key-ttl: %v %+v", err, a)
	}
	if d := time.Until(*a.KeyExpiresAt); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Fatalf("expiry in %v, want about 90d", d)
	}
	if _, err := c.run("agent", "create", "--account", c.acct, "--name", "bad", "--key-ttl", "soon"); err == nil {
		t.Fatal("bad --key-ttl accepted")
	}
	if _, err := c.run("agent", "create", "--account", c.acct, "--name", "plain"); err != nil {
		t.Fatal(err)
	}
	out, _ := c.run("agent", "list")
	if !strings.Contains(out, "KEY EXPIRES") || !strings.Contains(out, a.KeyExpiresAt.Local().Format("2006-01-02")) || !strings.Contains(out, "never") {
		t.Fatalf("list: %s", out)
	}

	// key-expiry: set, clear, expire now-ish.
	if out, err := c.run("agent", "key-expiry", "bot", "never"); err != nil || !strings.Contains(out, "never expires") {
		t.Fatalf("key-expiry never: %q %v", out, err)
	}
	if _, err := c.run("agent", "key-expiry", "bot"); err == nil {
		t.Fatal("key-expiry without a value accepted")
	}
	if _, err := c.run("agent", "key-expiry", "bot", "-5h"); err == nil {
		t.Fatal("negative key-expiry accepted")
	}
	if out, err := c.run("agent", "key-expiry", "bot", "12h"); err != nil || !strings.Contains(out, "expires") {
		t.Fatalf("key-expiry 12h: %q %v", out, err)
	}
	if _, err := c.st.Authenticate(ctx, key); err != nil {
		t.Fatalf("key unchanged by key-expiry: %v", err)
	}

	// rotate-key: new secret printed, old one lives through the grace period.
	nk, err := c.run("agent", "rotate-key", "bot", "--grace", "2h", "--key-ttl", "30d")
	if err != nil {
		t.Fatal(err)
	}
	nk = strings.TrimSpace(nk)
	if !strings.HasPrefix(nk, "ag_") || nk == key {
		t.Fatalf("new key %q", nk)
	}
	if _, err := c.st.Authenticate(ctx, nk); err != nil {
		t.Fatal(err)
	}
	if got, err := c.st.Authenticate(ctx, key); err != nil || !got.UsedPrevKey {
		t.Fatalf("old key in grace: %v %+v", err, got)
	}
	nk2, err := c.run("agent", "rotate-key", "bot", "--grace", "0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.Authenticate(ctx, key); err == nil {
		t.Fatal("old key still works after rotating twice")
	}
	if _, err := c.st.Authenticate(ctx, nk); err == nil {
		t.Fatal("--grace 0: the replaced key still works")
	}
	if _, err := c.st.Authenticate(ctx, strings.TrimSpace(nk2)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.run("agent", "rotate-key", "bot", "--grace", "later"); err == nil {
		t.Fatal("bad --grace accepted")
	}
	if _, err := c.run("agent", "rotate-key", "nobody"); err == nil {
		t.Fatal("rotated an unknown agent")
	}
	if _, err := c.run("agent", "rotate-key"); err == nil {
		t.Fatal("rotate-key without an agent accepted")
	}

	// An expired key shows as expired in the list.
	if _, err := c.st.SetKeyExpiry(ctx, "bot", time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if out, _ := c.run("agent", "list"); !strings.Contains(out, "expired") {
		t.Fatalf("list lacks expired: %s", out)
	}
}
