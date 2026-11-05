package server_test

import (
	"encoding/json"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

// The test gateway's default limit is 100. The hold assumes a bounded output, so the upstream must be held to the same
// bound. Otherwise one request can cost more than its estimate and carry an
// agent past its daily budget.
func TestUpstreamGetsTheOutputLimitTheHoldAssumed(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want float64
	}{
		"missing":      {`{"model":"oa/m","messages":[{"role":"user","content":"hi"}]}`, 100},
		"zero":         {`{"model":"oa/m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`, 100},
		"client's own": {`{"model":"oa/m","max_tokens":7,"messages":[{"role":"user","content":"hi"}]}`, 7},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 10*usd, nil)
			e.fake.SetDefault(fakeupstream.Script{InputTokens: 1, OutputTokens: 1, CostUSD: "0.000001"})
			if resp, b := e.do("/v1/chat/completions", tc.body, nil); resp.StatusCode != 200 {
				t.Fatalf("%d %s", resp.StatusCode, b)
			}
			var sent map[string]any
			if err := json.Unmarshal(e.fake.Calls()[0].Body, &sent); err != nil {
				t.Fatal(err)
			}
			if sent["max_tokens"] != tc.want {
				t.Fatalf("upstream max_tokens = %v, want %v", sent["max_tokens"], tc.want)
			}
		})
	}
}

// With both fields set, the hold must cover the larger, because the gateway
// cannot know which one the provider obeys.
func TestEstimateUsesTheLargerOutputLimit(t *testing.T) {
	small := `{"model":"oa/m","max_tokens":1,"max_completion_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	both := `{"model":"oa/m","max_tokens":90000,"max_completion_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	e := newEnv(t, 100*usd, nil)
	e.setScope(func(s *policy.Scope) { s.DailyBudget = estimate(small) * 2 })
	e.fake.SetDefault(fakeupstream.Script{InputTokens: 1, OutputTokens: 1, CostUSD: "0.000001"})
	resp, b := e.do("/v1/chat/completions", both, nil)
	if resp.StatusCode != 429 || errCode(jsonBody(t, b)) != policy.RuleBudgetDaily {
		t.Fatalf("a large max_tokens hid behind a small max_completion_tokens: %d %s", resp.StatusCode, b)
	}
}
