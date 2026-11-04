package policy_test

import (
	"sync"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

func newPolicy() *policy.Policy {
	return policy.New(policy.Config{
		Agents: map[string]policy.AgentRule{"a": {Account: "acct-1", Budget: 100}},
		Payees: []string{"0xAbC0000000000000000000000000000000000001"},
	})
}

func req(agent, payee string, amt money.Nanos) policy.Request {
	return policy.Request{AccountID: "acct-1", AgentID: agent, Payee: payee, Amount: amt, Network: "eip155:1337"}
}

const good = "0xabc0000000000000000000000000000000000001" // case differs from config on purpose

func TestCheck(t *testing.T) {
	p := newPolicy()
	if a, d := p.Check(req("a", good, 60)); d != nil || !a.Valid() {
		t.Fatalf("first: %v", d)
	}
	if _, d := p.Check(req("a", good, 60)); d == nil || d.Reason != "over_budget" {
		t.Fatalf("second should exceed the budget: %v", d)
	}
	if _, d := p.Check(req("a", "0xdead000000000000000000000000000000000002", 1)); d == nil || d.Reason != "payee_not_allowed" {
		t.Fatalf("payee: %v", d)
	}
	if _, d := p.Check(req("nobody", good, 1)); d == nil || d.Reason != "unknown_agent" {
		t.Fatalf("agent: %v", d)
	}
	if _, d := p.Check(req("a", good, 0)); d == nil || d.Reason != "invalid_request" {
		t.Fatalf("amount: %v", d)
	}
}

func TestDeniedIsNotAllowed(t *testing.T) {
	a, d := newPolicy().Check(req("a", good, 1000))
	if d == nil || a.Valid() {
		t.Fatal("a denial must carry no proof")
	}
	if (policy.Allowed{}).Valid() {
		t.Fatal("the zero Allowed must not be a proof")
	}
}

func TestRefundRestoresBudget(t *testing.T) {
	p := newPolicy()
	a, _ := p.Check(req("a", good, 100))
	if _, d := p.Check(req("a", good, 1)); d == nil {
		t.Fatal("budget should be used up")
	}
	p.Refund(a)
	if _, d := p.Check(req("a", good, 100)); d != nil {
		t.Fatalf("refund did not restore: %v", d)
	}
}

// Concurrent checks cannot overspend a budget that fits one payment.
func TestConcurrentChecksRespectBudget(t *testing.T) {
	p := newPolicy()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, d := p.Check(req("a", good, 100)); d == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d payments approved, want 1", ok)
	}
}

func TestAgentBelongsToOneAccount(t *testing.T) {
	p := newPolicy()
	r := req("a", good, 1)
	r.AccountID = "acct-2"
	if _, d := p.Check(r); d == nil || d.Reason != "unknown_agent" {
		t.Fatalf("another account spent as the agent: %v", d)
	}
	// A rule with no account never matches anyone.
	p2 := policy.New(policy.Config{Agents: map[string]policy.AgentRule{"a": {Budget: 100}}, Payees: []string{good}})
	if _, d := p2.Check(r); d == nil {
		t.Fatal("rule without an account must deny")
	}
	r.AccountID = ""
	if _, d := p2.Check(r); d == nil {
		t.Fatal("empty account must deny")
	}
}
