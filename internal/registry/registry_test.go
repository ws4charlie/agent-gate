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
	if _, err := s.SetStatus(ctx, "model", "gpt-4o", "approved"); err != nil {
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
	if err := s.Delete(ctx, "tool", "fs/read"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "tool", "fs/read"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "tool", "fs/read"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.SetStatus(ctx, "tool", "nope", "approved"); !errors.Is(err, ErrNotFound) {
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
	audit("model", "claude-x", "") // no agent: not listed as an agent
	since := time.Now().Add(-time.Hour)

	got, err := s.Unregistered(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"model/gpt-4o": 2, "model/claude-x": 1, "tool/fs/read": 1, "tool/seller/search": 1, "agent/ag_1": 10}
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
