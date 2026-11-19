package server_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/mcp"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/server"
)

// ---- Cedar enforce mode (fake engine: what Cedar says is the test's choice) ----

type decideFn = func(policy.Request, policy.Snapshot) (string, string, error)

func say(verdict, rule string) decideFn {
	return func(policy.Request, policy.Snapshot) (string, string, error) { return verdict, rule, nil }
}

func enforce(e *env, fn decideFn) {
	e.srv.Cedar, e.srv.CedarEnforce = stubShadow{fn}, true
}

func TestCedarEnforceDeniesWhatGoAllows(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	enforce(e, say("deny", "scope_model"))
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 403 || errCode(jsonBody(t, b)) != "scope_model" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if e.fake.CallCount() != 0 {
		t.Fatal("a call Cedar denied reached upstream")
	}
	ds := e.decisions(resp)
	if len(ds) != 1 || ds[0].Verdict != policy.Deny || ds[0].Rule != "scope_model" ||
		ds[0].CedarVerdict != "deny" || ds[0].CedarRule != "scope_model" ||
		ds[0].GoVerdict != policy.Allow || ds[0].GoRule != "allow" {
		t.Fatalf("rows: %+v", ds)
	}
	if !strings.Contains(e.logs.String(), "cedar enforce mismatch") {
		t.Fatalf("no mismatch warning:\n%s", e.logs.String())
	}
}

func TestCedarEnforceAllowsWhatGoDenies(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.setScope(func(sc *policy.Scope) { sc.Models = []string{"nothing-matches"} })
	enforce(e, say("allow", "allow"))
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("Cedar's allow must count: %d %s", resp.StatusCode, b)
	}
	ds := e.decisions(resp)
	if len(ds) != 1 || ds[0].Verdict != policy.Allow || ds[0].CedarVerdict != "allow" ||
		ds[0].GoVerdict != policy.Deny || ds[0].GoRule != "scope_model" {
		t.Fatalf("rows: %+v", ds)
	}
}

func TestCedarEnforceAgreementKeepsGoReason(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.setScope(func(sc *policy.Scope) { sc.Models = []string{"nothing-matches"} })
	enforce(e, say("deny", "scope_model"))
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	ds := e.decisions(resp)
	if len(ds) != 1 || !strings.Contains(ds[0].Reason, "outside this agent's scope") {
		t.Fatalf("rows: %+v", ds)
	}
	if strings.Contains(e.logs.String(), "mismatch") {
		t.Fatalf("unexpected mismatch:\n%s", e.logs.String())
	}
}

func TestCedarEnforceRequireApprovalFlowsToApprovalPath(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	enforce(e, say("require_approval", "approval_threshold"))
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": "k1"})
	if resp.StatusCode != 202 || jsonBody(t, b)["approval_id"] == nil {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if e.fake.CallCount() != 0 {
		t.Fatal("reached upstream before approval")
	}
}

func TestCedarEnforceFailsClosed(t *testing.T) {
	for name, fn := range map[string]decideFn{
		"error":   func(policy.Request, policy.Snapshot) (string, string, error) { return "", "", errors.New("boom") },
		"panic":   func(policy.Request, policy.Snapshot) (string, string, error) { panic("boom") },
		"garbage": say("maybe", "x"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 10*usd, nil)
			enforce(e, fn)
			resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
			if resp.StatusCode != 403 || errCode(jsonBody(t, b)) != server.RuleCedarError {
				t.Fatalf("%d %s", resp.StatusCode, b)
			}
			if e.fake.CallCount() != 0 {
				t.Fatal("fell back to the Go decision")
			}
			ds := e.decisions(resp)
			if len(ds) != 1 || ds[0].Verdict != policy.Deny || ds[0].Rule != server.RuleCedarError ||
				ds[0].CedarVerdict != "error" || ds[0].GoVerdict != policy.Allow {
				t.Fatalf("rows: %+v", ds)
			}
			if !strings.Contains(e.logs.String(), "level=ERROR") || !strings.Contains(e.logs.String(), "cedar enforce") {
				t.Fatalf("no error logged:\n%s", e.logs.String())
			}
		})
	}
}

func TestCedarEnforceWithoutEngineFailsClosed(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.srv.CedarEnforce = true
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 403 || errCode(jsonBody(t, b)) != server.RuleCedarError {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestCedarShadowKeepsGoVerdictColumnsEmpty(t *testing.T) {
	e := newEnv(t, 10*usd, func(s *server.Server) { s.Cedar = stubShadow{say("deny", "scope_model")} })
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	ds := e.decisions(resp)
	if resp.StatusCode != 200 || len(ds) != 1 || ds[0].GoVerdict != "" || ds[0].GoRule != "" || ds[0].CedarVerdict != "deny" {
		t.Fatalf("%d rows: %+v", resp.StatusCode, ds)
	}
}

// ---- shortcuts ----

func TestCedarEnforceRefuseInactive(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	if _, err := e.pool.Exec(t.Context(), `UPDATE agents SET status='killed' WHERE account_id=$1`, e.acct); err != nil {
		t.Fatal(err)
	}
	enforce(e, say("deny", "cedar_says_no"))
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 403 || errCode(jsonBody(t, b)) != "cedar_says_no" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	ds := e.decisions(resp)
	if len(ds) != 1 || ds[0].Rule != "cedar_says_no" || ds[0].GoRule != policy.RuleKilled {
		t.Fatalf("rows: %+v", ds)
	}
	// Cedar failing here is a deny too.
	enforce(e, func(policy.Request, policy.Snapshot) (string, string, error) { return "", "", errors.New("boom") })
	resp, b = e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 403 || errCode(jsonBody(t, b)) != server.RuleCedarError {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestCedarShadowRefuseInactiveRecordsCedar(t *testing.T) {
	e := newEnv(t, 10*usd, func(s *server.Server) { s.Cedar = cedarEngine(t) })
	if _, err := e.pool.Exec(t.Context(), `UPDATE agents SET status='killed' WHERE account_id=$1`, e.acct); err != nil {
		t.Fatal(err)
	}
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	ds := e.decisions(resp)
	if resp.StatusCode != 403 || len(ds) != 1 || ds[0].Rule != policy.RuleKilled || ds[0].CedarVerdict != "deny" || ds[0].CedarRule != policy.RuleKilled {
		t.Fatalf("%d rows: %+v", resp.StatusCode, ds)
	}
}

func TestCedarEnforceFreeToolCall(t *testing.T) {
	m := newMCPEnv(t, 10*usd, nil)
	enforce(m.env, say("deny", "scope_tool"))
	resp, b := m.call("echo", map[string]any{"text": "hi"}, nil)
	code, data := rpcErr(t, b)
	if resp.StatusCode != 200 || code != mcp.CodePolicyDenied || data["rule"] != "scope_tool" || m.up.ToolCalls() != 0 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	ds := m.decisions(resp)
	if len(ds) != 1 || ds[0].Verdict != policy.Deny || ds[0].CedarVerdict != "deny" || ds[0].GoVerdict != policy.Allow {
		t.Fatalf("rows: %+v", ds)
	}
	// Cedar allowing a tool the Go engine would refuse lets it through.
	m.scopeTools("fake/other")
	enforce(m.env, say("allow", "allow"))
	resp, b = m.call("echo", map[string]any{"text": "hi"}, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"hi"`) || m.up.ToolCalls() != 1 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// Cedar failing: deny.
	enforce(m.env, func(policy.Request, policy.Snapshot) (string, string, error) { return "", "", errors.New("boom") })
	resp, b = m.call("echo", map[string]any{"text": "hi"}, nil)
	_, data = rpcErr(t, b)
	if data["rule"] != server.RuleCedarError || m.up.ToolCalls() != 1 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestCedarShadowFreeToolRecordsCedar(t *testing.T) {
	m := newMCPEnv(t, 10*usd, nil)
	m.srv.Cedar = cedarEngine(t)
	resp, _ := m.call("echo", map[string]any{"text": "hi"}, nil)
	ds := m.decisions(resp)
	if resp.StatusCode != 200 || len(ds) != 1 || ds[0].CedarVerdict != "allow" || ds[0].CedarRule != "allow" {
		t.Fatalf("rows: %+v", ds)
	}
}

func listedTools(t *testing.T, b []byte) string {
	t.Helper()
	var out struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range out.Result.Tools {
		names = append(names, tl.Name)
	}
	return strings.Join(names, ",")
}

func TestCedarEnforceToolsList(t *testing.T) {
	m := newMCPEnv(t, 10*usd, nil)
	// Cedar hides echo although the Go engine would list everything.
	enforce(m.env, func(r policy.Request, _ policy.Snapshot) (string, string, error) {
		if r.Tool == "fake/echo" {
			return "deny", "scope_tool", nil
		}
		return "allow", "allow", nil
	})
	_, b := m.rpc("tools/list", nil, nil)
	if got := listedTools(t, b); got == "" || strings.Contains(got, "echo") {
		t.Fatalf("listed %q", got)
	}
	// A Cedar error hides every tool.
	enforce(m.env, func(policy.Request, policy.Snapshot) (string, string, error) { return "", "", errors.New("boom") })
	_, b = m.rpc("tools/list", nil, nil)
	if got := listedTools(t, b); got != "" {
		t.Fatalf("listed %q after a Cedar error", got)
	}
	// Nothing is logged per tool.
	var n int
	if err := m.pool.QueryRow(t.Context(), `SELECT count(*) FROM policy_decisions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d decision rows %v", n, err)
	}
}
