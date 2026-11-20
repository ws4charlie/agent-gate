package server_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/cedarpolicy"
	"github.com/ws4charlie/agent-gate/internal/mcp"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/registry"
	"github.com/ws4charlie/agent-gate/internal/server"
)

// registryOn turns the registry check on with the real Cedar policies in
// enforce mode, so every case below proves both engines.
func registryOn(t *testing.T, s *server.Server) {
	t.Helper()
	eng, err := cedarpolicy.Load("../../policies/cedar")
	if err != nil {
		t.Fatal(err)
	}
	s.Cedar, s.CedarEnforce = eng, true
	s.RegistryEnforce = true
	s.AIRegistry = registry.NewStore(s.Pool)
}

func (e *env) setRegistry(kind, name, status string) {
	e.t.Helper()
	st := registry.NewStore(e.pool)
	if _, err := st.Upsert(context.Background(), registry.Entry{Kind: kind, Name: name, Owner: "o", Purpose: "p", Status: status}); err != nil {
		e.t.Fatal(err)
	}
}

// The proof: a new model is refused until it is approved, with Cedar deciding
// and the Go engine agreeing.
func TestRegistryEnforceModel(t *testing.T) {
	e := newEnv(t, 10*usd, func(s *server.Server) { registryOn(t, s) })
	ask := func() (int, string, []byte) {
		resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
		return resp.StatusCode, errCode(jsonBody(t, b)), b
	}
	check := func(want string, status string) {
		t.Helper()
		resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
		ds := e.decisions(resp)
		if len(ds) != 1 || ds[0].Rule != want || ds[0].GoRule != want || ds[0].CedarRule != want {
			t.Fatalf("%s: rows %+v (%s)", status, ds, b)
		}
		if strings.Contains(e.logs.String(), "mismatch") {
			t.Fatalf("Cedar and Go disagree:\n%s", e.logs.String())
		}
	}
	// No entry at all.
	if code, rule, b := ask(); code != 403 || rule != policy.RuleRegistryUnapproved {
		t.Fatalf("no entry: %d %s", code, b)
	}
	check(policy.RuleRegistryUnapproved, "no entry")
	// proposed, deprecated, blocked: all refused.
	for _, st := range []string{"proposed", "deprecated", "blocked"} {
		e.setRegistry("model", "oa/m", st)
		if code, rule, b := ask(); code != 403 || rule != policy.RuleRegistryUnapproved {
			t.Fatalf("%s: %d %s", st, code, b)
		}
	}
	if e.fake.CallCount() != 0 {
		t.Fatal("an unapproved model reached upstream")
	}
	check(policy.RuleRegistryUnapproved, "blocked")
	// Approved: the same request passes.
	if _, err := registry.NewStore(e.pool).SetStatus(context.Background(), "model", "oa/m", "approved", "test", ""); err != nil {
		t.Fatal(err)
	}
	if code, _, b := ask(); code != 200 {
		t.Fatalf("approved: %d %s", code, b)
	}
	check(policy.RuleAllow, "approved")
	if e.fake.CallCount() != 2 { // the ask and the check above
		t.Fatalf("upstream calls: %d", e.fake.CallCount())
	}
}

// Not on the agent's allowlist stays scope_model, whatever the registry says.
func TestRegistryEnforceScopeComesFirst(t *testing.T) {
	e := newEnv(t, 10*usd, func(s *server.Server) { registryOn(t, s) })
	e.setScope(func(sc *policy.Scope) { sc.Models = []string{"other"} })
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 403 || errCode(jsonBody(t, b)) != policy.RuleScopeModel {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

// With the check off nothing is looked up and an unregistered model passes.
func TestRegistryCheckOffChangesNothing(t *testing.T) {
	e := newEnv(t, 10*usd, func(s *server.Server) {
		eng, err := cedarpolicy.Load("../../policies/cedar")
		if err != nil {
			t.Fatal(err)
		}
		s.Cedar, s.CedarEnforce = eng, true
	})
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

// A tool: tools/call is refused and tools/list hides it until it is approved.
func TestRegistryEnforceTool(t *testing.T) {
	m := newMCPEnv(t, 10*usd, nil)
	registryOn(t, m.srv)

	resp, b := m.rpc("tools/list", nil, nil)
	if resp.StatusCode != 200 || listedTools(t, b) != "" {
		t.Fatalf("nothing is registered, nothing may be listed: %d %s", resp.StatusCode, b)
	}
	m.setRegistry("tool", "fake/echo", "proposed")
	m.setRegistry("tool", "fake/stream", "approved")
	_, b = m.rpc("tools/list", nil, nil)
	if got := listedTools(t, b); got != "stream" {
		t.Fatalf("listed %q, want only the approved tool", got)
	}

	resp, b = m.call("echo", map[string]any{"text": "hi"}, nil)
	code, data := rpcErr(t, b)
	if code != mcp.CodePolicyDenied || data["rule"] != policy.RuleRegistryUnapproved {
		t.Fatalf("proposed tool: %s", b)
	}
	ds := m.decisions(resp)
	if len(ds) != 1 || ds[0].Rule != policy.RuleRegistryUnapproved || ds[0].CedarRule != policy.RuleRegistryUnapproved ||
		ds[0].GoRule != policy.RuleRegistryUnapproved {
		t.Fatalf("rows %+v", ds)
	}
	if m.up.ToolCalls() != 0 {
		t.Fatal("an unapproved tool reached upstream")
	}

	if _, err := registry.NewStore(m.pool).SetStatus(context.Background(), "tool", "fake/echo", "approved", "test", ""); err != nil {
		t.Fatal(err)
	}
	resp, b = m.call("echo", map[string]any{"text": "hi"}, nil)
	if code, _ := rpcErr(t, b); resp.StatusCode != 200 || code != 0 {
		t.Fatalf("approved tool: %d %s", resp.StatusCode, b)
	}
	_, b = m.rpc("tools/list", nil, nil)
	if got := listedTools(t, b); got != "echo,stream" {
		t.Fatalf("listed %q", got)
	}
	if strings.Contains(m.logs.String(), "mismatch") {
		t.Fatalf("Cedar and Go disagree:\n%s", m.logs.String())
	}
}

// A paid tool: the payment path is gated by the same rule, before any money moves.
func TestRegistryEnforcePricedTool(t *testing.T) {
	m := newMCPEnv(t, 10*usd, map[string]money.Nanos{"slow": 1_000_000})
	registryOn(t, m.srv)
	resp, b := m.call("slow", map[string]any{"ms": 1}, nil)
	if _, data := rpcErr(t, b); data["rule"] != policy.RuleRegistryUnapproved {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	m.noActiveHolds()
	if m.up.ToolCalls() != 0 {
		t.Fatal("reached upstream")
	}
}
