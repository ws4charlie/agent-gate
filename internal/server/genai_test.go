package server_test

import (
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/ws4charlie/agent-gate/internal/policy"
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
