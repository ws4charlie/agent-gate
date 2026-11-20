package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
)

func newStore(t *testing.T) (*Store, func(kind, model, agent string)) {
	t.Helper()
	pool := dbtest.New(t)
	n := 0
	audit := func(kind, model, agent string) {
		t.Helper()
		n++
		_, err := pool.Exec(context.Background(), `INSERT INTO audit_events (request_id, agent_id, kind, model, status, at)
			VALUES ($1, $2, $3, $4, 'ok', now())`, "req"+string(rune('a'+n)), agent, kind, model)
		if err != nil {
			t.Fatal(err)
		}
	}
	return NewStore(pool), audit
}

func TestRegistryCRUD(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	e, err := s.Upsert(ctx, Entry{Kind: "model", Name: "gpt-4o", Owner: "ml-team", Purpose: "chat"})
	if err != nil || e.Status != "proposed" {
		t.Fatalf("%+v %v", e, err)
	}
	// Updating without a status keeps the status; with one, changes it.
	if _, err := s.SetStatus(ctx, "model", "gpt-4o", "approved", "t"); err != nil {
		t.Fatal(err)
	}
	e, err = s.Upsert(ctx, Entry{Kind: "model", Name: "gpt-4o", Owner: "platform", Purpose: "chat v2", Notes: "n"})
	if err != nil || e.Status != "approved" || e.Owner != "platform" || e.Notes != "n" {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := s.Upsert(ctx, Entry{Kind: "tool", Name: "fs/read", Owner: "o", Purpose: "p", Status: "blocked"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "tool", "fs/read")
	if err != nil || got.Status != "blocked" {
		t.Fatalf("%+v %v", got, err)
	}
	all, _ := s.List(ctx, "")
	models, _ := s.List(ctx, "model")
	if len(all) != 2 || len(models) != 1 {
		t.Fatalf("%d %d", len(all), len(models))
	}
	if err := s.Delete(ctx, "tool", "fs/read", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "tool", "fs/read"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "tool", "fs/read", "t"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.SetStatus(ctx, "tool", "nope", "approved", "t"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestRegistryChecks(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.Upsert(ctx, Entry{Kind: "dataset", Name: "x", Owner: "o", Purpose: "p"}); !errors.Is(err, ErrBadKind) {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, Entry{Kind: "model", Name: "x", Owner: "o", Purpose: "p", Status: "great"}); !errors.Is(err, ErrBadStatus) {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, Entry{Kind: "model", Name: "x", Purpose: "p"}); !errors.Is(err, ErrBadEntry) {
		t.Fatal(err)
	}
	// The database refuses a bad kind or status even without the Go checks.
	if _, err := s.pool.Exec(ctx, `INSERT INTO registry_entries (kind, name, owner, purpose) VALUES ('x','a','o','p')`); err == nil {
		t.Fatal("bad kind accepted by the table")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO registry_entries (kind, name, owner, purpose, status) VALUES ('model','a','o','p','x')`); err == nil {
		t.Fatal("bad status accepted by the table")
	}
}

func TestUnregistered(t *testing.T) {
	s, audit := newStore(t)
	ctx := context.Background()
	audit("model", "gpt-4o", "ag_1")
	audit("model", "gpt-4o", "ag_1")
	audit("mcp", "fs/read", "ag_1")
	for _, m := range []string{"initialize", "ping", "tools/list", "resources/read", "notifications/initialized"} {
		audit("mcp", m, "ag_1")
	}
	audit("mcp", "", "ag_1")
	audit("payment", "seller/search", "ag_1")
	audit("payment", "", "ag_1")   // a payment with no tool name: not listed as a tool
	audit("model", "claude-x", "") // no agent: not listed as an agent
	since := time.Now().Add(-time.Hour)

	got, err := s.Unregistered(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"model/gpt-4o": 2, "model/claude-x": 1, "tool/fs/read": 1, "tool/seller/search": 1, "agent/ag_1": 11}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for _, u := range got {
		if want[u.Kind+"/"+u.Name] != u.Calls || u.Status != "" || u.LastSeen.IsZero() {
			t.Errorf("%+v", u)
		}
	}
	// Outside the window nothing shows.
	if got, _ := s.Unregistered(ctx, time.Now().Add(time.Hour)); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	// Registering removes an item from the list.
	for _, e := range []Entry{{Kind: "model", Name: "gpt-4o"}, {Kind: "tool", Name: "fs/read"}, {Kind: "agent", Name: "ag_1"}} {
		e.Owner, e.Purpose = "o", "p"
		if _, err := s.Upsert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = s.Unregistered(ctx, since)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestInUseButNotApproved(t *testing.T) {
	s, audit := newStore(t)
	ctx := context.Background()
	audit("model", "old-model", "ag_1")
	audit("model", "fine-model", "ag_1")
	audit("mcp", "evil/tool", "ag_1")
	for _, e := range []Entry{
		{Kind: "model", Name: "old-model", Status: "deprecated"},
		{Kind: "model", Name: "fine-model", Status: "approved"},
		{Kind: "tool", Name: "evil/tool", Status: "blocked"},
		{Kind: "tool", Name: "unused/tool", Status: "blocked"},
	} {
		e.Owner, e.Purpose = "o", "p"
		if _, err := s.Upsert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.InUseButNotApproved(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	seen := map[string]string{}
	for _, u := range got {
		seen[u.Name] = u.Status
	}
	if seen["old-model"] != "deprecated" || seen["evil/tool"] != "blocked" {
		t.Fatalf("%+v", seen)
	}
}

func TestHistoryRiskTierAndVersion(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	e, err := s.Upsert(ctx, Entry{Kind: "model", Name: "m1", Owner: "o", Purpose: "p", RiskTier: "high", Version: "2026-01", Actor: "alice"})
	if err != nil || e.RiskTier != "high" || e.Version != "2026-01" || e.Status != "proposed" {
		t.Fatalf("%+v %v", e, err)
	}
	// An update without tier/version/status keeps them and writes no history row.
	e, _ = s.Upsert(ctx, Entry{Kind: "model", Name: "m1", Owner: "o2", Purpose: "p"})
	if e.RiskTier != "high" || e.Version != "2026-01" || e.Status != "proposed" {
		t.Fatalf("%+v", e)
	}
	if _, err := s.Upsert(ctx, Entry{Kind: "model", Name: "m1", Owner: "o", Purpose: "p", RiskTier: "extreme"}); !errors.Is(err, ErrBadTier) {
		t.Fatal(err)
	}
	s.SetStatus(ctx, "model", "m1", "approved", "bob")
	s.SetStatus(ctx, "model", "m1", "approved", "bob") // no change, no row
	if err := s.Delete(ctx, "model", "m1", "carol"); err != nil {
		t.Fatal(err)
	}
	h, err := s.History(ctx, "model", "m1", 0)
	if err != nil || len(h) != 3 {
		t.Fatalf("%+v %v", h, err)
	}
	if h[0].Actor != "carol" || h[0].From != "approved" || h[0].To != "" ||
		h[1].Actor != "bob" || h[1].From != "proposed" || h[1].To != "approved" ||
		h[2].Actor != "alice" || h[2].From != "" || h[2].To != "proposed" {
		t.Fatalf("%+v", h)
	}
}

func TestSeedAndToolStatuses(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	s.Upsert(ctx, Entry{Kind: "model", Name: "kept", Owner: "o", Purpose: "p", Status: "blocked"})
	items := []SeedItem{{"model", "kept"}, {"model", "new"}, {"tool", "fs/read"}, {"tool", "fs/write"}, {"tool", "other/x"}}
	dry, err := s.Seed(ctx, items, "t", true)
	if err != nil || len(dry) != 4 {
		t.Fatalf("%v %v", dry, err)
	}
	if st, _ := s.Status(ctx, "model", "new"); st != "" {
		t.Fatal("dry run wrote")
	}
	added, _ := s.Seed(ctx, items, "t", false)
	again, _ := s.Seed(ctx, items, "t", false)
	if len(added) != 4 || len(again) != 0 {
		t.Fatalf("%v %v", added, again)
	}
	if st, _ := s.Status(ctx, "model", "kept"); st != "blocked" {
		t.Fatalf("existing entry changed: %s", st)
	}
	m, _ := s.ToolStatuses(ctx, "fs")
	if len(m) != 2 || m["read"] != "approved" || m["write"] != "approved" {
		t.Fatalf("%v", m)
	}
	if h, _ := s.History(ctx, "", "", 0); len(h) != 5 { // 1 create + 4 seeded
		t.Fatalf("%d rows", len(h))
	}
}
