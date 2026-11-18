package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/registry"
)

func TestRegistryAPI(t *testing.T) {
	s := newTestServer(t, nil)
	ctx := context.Background()
	acct, err := ledger.New(s.pool).CreateAccount(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	_, ag, err := s.agents.Create(ctx, acct, "scout", policy.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][3]string{{"model", "gpt-4o", ag.ID}, {"model", "old-model", ag.ID}, {"mcp", "fs/read", ag.ID}, {"mcp", "tools/list", ag.ID}} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO audit_events (request_id, agent_id, kind, model, status, at) VALUES ($1,$2,$3,$4,'ok',now())`,
			"r-"+r[1], r[2], r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	st := registry.NewStore(s.pool)
	for _, e := range []registry.Entry{{Kind: "model", Name: "gpt-4o", Status: "approved"}, {Kind: "model", Name: "old-model", Status: "deprecated"}} {
		e.Owner, e.Purpose = "ml", "chat"
		if _, err := st.Upsert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, apiReq(s, "GET", "/api/registry", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got struct {
		Entries []struct {
			Kind, Name, Owner, Status string
			InUse                     *struct{ Calls int64 } `json:"in_use"`
		}
		Unregistered []struct {
			Kind, Name string
			AgentName  string `json:"agent_name"`
			Calls      int64
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 || got.Entries[0].Name != "gpt-4o" || got.Entries[0].InUse != nil ||
		got.Entries[1].Name != "old-model" || got.Entries[1].InUse == nil || got.Entries[1].InUse.Calls != 1 {
		t.Fatalf("entries %+v", got.Entries)
	}
	seen := map[string]string{}
	for _, u := range got.Unregistered {
		seen[u.Kind+"/"+u.Name] = u.AgentName
	}
	if len(seen) != 2 || seen["tool/fs/read"] != "" {
		t.Fatalf("unregistered %+v", got.Unregistered)
	}
	if name, ok := seen["agent/"+ag.ID]; !ok || name != "scout" {
		t.Fatalf("agent row %+v", seen)
	}

	// It sits behind the console key like the rest of the API.
	r := apiReq(s, "GET", "/api/registry", "")
	r.Header.Del("X-Console-Key")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", w.Code)
	}
}
