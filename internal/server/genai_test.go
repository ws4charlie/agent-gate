package server_test

import (
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/screen"
)

// rootSpan returns the request span: the one with no parent.
func rootSpan(t *testing.T, e *env, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range e.spans.GetSpans() {
		if !s.Parent.IsValid() && s.Name == name {
			return s.Snapshot()
		}
	}
	t.Fatalf("no root span %q", name)
	return nil
}

func attrs(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func wantAttrs(t *testing.T, got map[string]attribute.Value, want map[string]any) {
	t.Helper()
	for k, w := range want {
		v, ok := got[k]
		if !ok {
			t.Errorf("attribute %s missing", k)
			continue
		}
		if v.AsInterface() != w {
			t.Errorf("attribute %s = %v, want %v", k, v.AsInterface(), w)
		}
	}
}

func noContent(t *testing.T, e *env, needles ...string) {
	t.Helper()
	for _, s := range e.spans.GetSpans() {
		for _, kv := range s.Attributes {
			for _, n := range needles {
				if strings.Contains(kv.Value.Emit(), n) {
					t.Errorf("span %q attribute %s carries content %q", s.Name, kv.Key, n)
				}
			}
		}
		for _, ev := range s.Events {
			for _, n := range needles {
				if strings.Contains(ev.Name, n) {
					t.Errorf("span %q event carries content %q", s.Name, n)
				}
			}
		}
	}
}

func TestGenAIModelCallSpan(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	body := `{"model":"oa/m","max_tokens":200,"messages":[{"role":"user","content":"SECRET-PROMPT-TEXT"}]}`
	resp, _ := e.do("/v1/chat/completions", body, nil)
	row := e.auditRow(resp)

	s := rootSpan(t, e, "chat oa/m")
	if s.SpanKind() != trace.SpanKindClient {
		t.Fatalf("kind %v", s.SpanKind())
	}
	if s.Status().Code == codes.Error {
		t.Fatalf("status %v", s.Status())
	}
	got := attrs(s)
	wantAttrs(t, got, map[string]any{
		"gen_ai.operation.name":      "chat",
		"gen_ai.provider.name":       "fakeoa",
		"gen_ai.request.model":       "oa/m",
		"gen_ai.usage.input_tokens":  row.Usage.InputTokens,
		"gen_ai.usage.output_tokens": row.Usage.OutputTokens,
		"gen_ai.agent.id":            row.AgentID,
		"agentgate.agent.id":         row.AgentID,
		"agentgate.account.id":       e.acct,
		"agentgate.request.id":       row.RequestID,
		"agentgate.request.status":   "ok",
		"agentgate.cost.nanos":       int64(row.Cost),
		"agentgate.cost.source":      row.CostSource,
		"agentgate.decision":         string(policy.Allow),
	})
	if row.Usage.InputTokens == 0 || row.Usage.OutputTokens == 0 {
		t.Fatalf("fake upstream reported no tokens: %+v", row.Usage)
	}
	for _, k := range []string{"gen_ai.agent.name", "agentgate.run.id", "agentgate.rule"} {
		if _, ok := got[k]; !ok {
			t.Errorf("attribute %s missing", k)
		}
	}
	if _, ok := got["error.type"]; ok {
		t.Error("error.type on a successful call")
	}
	if id, ok := got["gen_ai.response.id"]; ok && id.AsString() == "" {
		t.Error("empty gen_ai.response.id")
	}
	for _, old := range []string{"model", "provider", "tokens.input", "tokens.output", "cost.nanos", "agent.id", "account.id", "request.id", "request.status"} {
		if _, ok := got[old]; ok {
			t.Errorf("old attribute %q still set", old)
		}
	}
	noContent(t, e, "SECRET-PROMPT-TEXT", "messages")
}

func TestGenAIRefusedModelCallSpan(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.setScope(func(s *policy.Scope) { s.Models = []string{"an/*"} })
	resp, _ := e.do("/v1/chat/completions", `{"model":"oa/m","messages":[{"role":"user","content":"SECRET-PROMPT-TEXT"}]}`, nil)
	e.auditRow(resp)

	s := rootSpan(t, e, "chat oa/m")
	if s.Status().Code != codes.Error {
		t.Fatalf("status %v", s.Status())
	}
	wantAttrs(t, attrs(s), map[string]any{
		"error.type":               "policy_denied",
		"agentgate.request.status": "policy_denied",
		"agentgate.decision":       string(policy.Deny),
		"agentgate.rule":           policy.RuleScopeModel,
		"gen_ai.operation.name":    "chat",
	})
	noContent(t, e, "SECRET-PROMPT-TEXT")
}

func TestGenAIToolCallSpan(t *testing.T) {
	m := newMCPEnv(t, 10*usd, nil)
	resp, _ := m.call("echo", map[string]any{"text": "SECRET-TOOL-ARGUMENT"}, nil)
	m.auditRow(resp)

	s := rootSpan(t, m.env, "execute_tool echo")
	if s.SpanKind() != trace.SpanKindClient || s.Status().Code == codes.Error {
		t.Fatalf("kind %v status %v", s.SpanKind(), s.Status())
	}
	wantAttrs(t, attrs(s), map[string]any{
		"gen_ai.operation.name":    "execute_tool",
		"gen_ai.tool.name":         "echo",
		"gen_ai.provider.name":     "fake",
		"agentgate.request.status": "ok",
		"agentgate.account.id":     m.acct,
	})
	noContent(t, m.env, "SECRET-TOOL-ARGUMENT")
}

func TestGenAIRefusedToolCallSpan(t *testing.T) {
	m := newMCPEnv(t, 10*usd, nil)
	m.scopeTools("fake/other")
	resp, _ := m.call("echo", map[string]any{"text": "SECRET-TOOL-ARGUMENT"}, nil)
	m.auditRow(resp)
	s := rootSpan(t, m.env, "execute_tool echo")
	if s.Status().Code != codes.Error {
		t.Fatalf("status %v", s.Status())
	}
	wantAttrs(t, attrs(s), map[string]any{"error.type": "policy_denied", "agentgate.rule": policy.RuleScopeTool})
	noContent(t, m.env, "SECRET-TOOL-ARGUMENT")
}

func TestGenAIOtherMCPMethodKeepsPlainName(t *testing.T) {
	m := newMCPEnv(t, 10*usd, nil)
	resp, _ := m.rpc("tools/list", nil, nil)
	m.auditRow(resp)
	s := rootSpan(t, m.env, "mcp tools/list")
	got := attrs(s)
	if _, ok := got["gen_ai.operation.name"]; ok {
		t.Error("gen_ai attributes on tools/list")
	}
	wantAttrs(t, got, map[string]any{"agentgate.request.status": "ok"})
}

func TestGenAIResponseModel(t *testing.T) {
	for _, c := range []struct{ name, path, model, body string }{
		{"openai-json", "/v1/chat/completions", "oa/m", chatBody("oa/m", false)},
		{"openai-stream", "/v1/chat/completions", "oa/m", chatBody("oa/m", true)},
		{"anthropic-json", "/v1/messages", "an/m", msgBody("an/m", false)},
		{"anthropic-stream", "/v1/messages", "an/m", msgBody("an/m", true)},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, 10*usd, nil)
			resp, _ := e.do(c.path, c.body, nil)
			e.auditRow(resp)
			got := attrs(rootSpan(t, e, "chat "+c.model))
			rm, ok := got["gen_ai.response.model"]
			if !ok || rm.AsString() == "" {
				t.Fatalf("gen_ai.response.model = %v (set %v)", rm, ok)
			}
		})
	}
}

func TestGenAISecretGateSpans(t *testing.T) {
	m := newMCPEnv(t, 10*usd, map[string]money.Nanos{"paid": 1_000_000})
	args := map[string]any{"text": "debug " + testKey}
	resp, b := m.call("paid", args, nil)
	_, risk := heldResult(t, b)
	m.auditRow(resp)
	code := risk["to_send_anyway"].(map[string]any)["value"].(string)
	held := attrs(rootSpan(t, m.env, "execute_tool paid"))
	wantAttrs(t, held, map[string]any{"agentgate.decision": "hold", "agentgate.rule": "secret_held", "agentgate.request.status": "secret_held"})
	noContent(t, m.env, testKey, "AbCdEfGh", code, "debug")

	m2 := newMCPEnv(t, 10*usd, map[string]money.Nanos{"paid": 1_000_000})
	resp, b = m2.call("paid", args, nil)
	_, risk = heldResult(t, b)
	m2.auditRow(resp)
	code = risk["to_send_anyway"].(map[string]any)["value"].(string)
	args["_agentgate_confirm"] = code
	resp, _ = m2.call("paid", args, nil)
	m2.auditRow(resp)
	var got map[string]attribute.Value
	for _, s := range m2.spans.GetSpans() {
		if !s.Parent.IsValid() && s.Name == "execute_tool paid" {
			if a := attrs(s.Snapshot()); a["agentgate.request.status"].AsString() == "ok" {
				got = a
			}
		}
	}
	if got == nil {
		t.Fatal("no span for the confirmed call")
	}
	wantAttrs(t, got, map[string]any{"agentgate.decision": "allow", "agentgate.rule": "secret_held"})
	noContent(t, m2.env, testKey, "AbCdEfGh", code)

	m3 := newMCPEnv(t, 10*usd, map[string]money.Nanos{"paid": 1_000_000})
	m3.setScope(func(s *policy.Scope) { s.Secrets = policy.SecretsBlock })
	resp, _ = m3.call("paid", map[string]any{"text": "https://admin:hunter2pass@db.example.com"}, nil)
	m3.auditRow(resp)
	wantAttrs(t, attrs(rootSpan(t, m3.env, "execute_tool paid")), map[string]any{"agentgate.decision": "deny", "agentgate.rule": "secret_blocked"})
	noContent(t, m3.env, "hunter2", "db.example.com")
}

func TestGenAIScreenedResultSpan(t *testing.T) {
	m := newMCPEnv(t, 10*usd, map[string]money.Nanos{"inject": 1_000_000})
	resp, _ := m.call("inject", map[string]any{"text": "SECRET-TOOL-ARGUMENT"}, nil)
	m.auditRow(resp)
	got := attrs(rootSpan(t, m.env, "execute_tool inject"))
	wantAttrs(t, got, map[string]any{"agentgate.decision": "block", "agentgate.rule": screen.RuleInstructionOverride, "agentgate.request.status": "screened"})
	noContent(t, m.env, "SECRET-TOOL-ARGUMENT", "evil.example", "Ignore all previous")
}

func TestGenAIPaymentSpans(t *testing.T) {
	p := newPayMCPEnv(t)
	resp, _ := p.call("sp-1")
	p.auditRow(resp)
	got := attrs(rootSpan(t, p.env, "execute_tool lookup"))
	wantAttrs(t, got, map[string]any{"agentgate.decision": "allow", "agentgate.rule": policy.RuleAllow, "agentgate.payment.amount_nanos": int64(10_000_000)})
	if n, ok := got["agentgate.payment.network"]; !ok || n.AsString() == "" {
		t.Error("agentgate.payment.network missing")
	}
	noContent(t, p.env, p.payTo.Hex(), "paid answer")

	q := newPayMCPEnv(t)
	q.setScope(func(sc *policy.Scope) { sc.Payees = []string{q.payTo.Hex()}; sc.ApprovalThreshold = 5_000_000 })
	resp, _ = q.call("sp-2")
	q.auditRow(resp)
	wantAttrs(t, attrs(rootSpan(t, q.env, "execute_tool lookup")), map[string]any{
		"agentgate.decision": string(policy.RequireApproval), "agentgate.rule": policy.RuleApprovalThreshold})
}
