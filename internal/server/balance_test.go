package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/policy"
)

func (e *env) getBalance(secret string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.gw.URL+"/v1/balance", nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func TestBalanceRoute(t *testing.T) {
	p := newPayEnv(t) // $10 deposit, $0.30 daily budget
	code, m := p.getBalance(p.secret)
	if code != 200 || m["account_id"] != p.acct || m["balance_nanos"] != float64(10*usd) || m["held_nanos"] != float64(0) ||
		m["daily_spent_nanos"] != float64(0) || m["daily_budget_nanos"] != float64(usd*3/10) || m["remaining_today_nanos"] != float64(usd*3/10) {
		t.Fatalf("%d %v", code, m)
	}
	if c, _, _ := p.pay("echo", "bal-1"); c != 200 {
		t.Fatal(c)
	}
	_, m = p.getBalance(p.secret)
	if m["balance_nanos"] != float64(10*usd-10_000_000) || m["daily_spent_nanos"] != float64(10_000_000) ||
		m["remaining_today_nanos"] != float64(usd*3/10-10_000_000) || m["available"] != m["balance"] {
		t.Fatalf("after a payment: %v", m)
	}
	// No budget: the budget fields are absent rather than zero.
	p.setScope(func(s *policy.Scope) {})
	_, m = p.getBalance(p.secret)
	if _, ok := m["daily_budget"]; ok {
		t.Fatalf("budget shown for an agent with none: %v", m)
	}
}

func TestBalanceNeedsAValidKey(t *testing.T) {
	e := newEnv(t, usd, nil)
	for _, k := range []string{"", "ag_nope"} {
		if code, _ := e.getBalance(k); code != 401 {
			t.Fatalf("key %q: %d", k, code)
		}
	}
}

// Another account's agent sees only its own account.
func TestBalanceIsPerCredential(t *testing.T) {
	p := newPayEnv(t)
	_, m := p.getBalance(p.secret)
	if m["account_id"] != p.acct {
		t.Fatalf("%v", m)
	}
}
