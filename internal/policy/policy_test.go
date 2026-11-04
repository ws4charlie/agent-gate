package policy_test

import (
	"testing"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

const payee = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"

// open is a scope with every check switched on and every limit comfortably
// above the base request, so each case below breaks exactly the rules it names.
func open() policy.Scope {
	return policy.Scope{
		Models: []string{"claude-*", "gpt-4o"}, Tools: []string{"echo"}, Payees: []string{payee},
		DailyBudget: 1000, TaskBudget: 500, VelocityPerMinute: 300, ApprovalThreshold: 200,
	}
}

func snap() policy.Snapshot {
	return policy.Snapshot{AgentID: "a", Status: policy.StatusActive, Scope: open()}
}

func model(amt money.Nanos) policy.Request {
	return policy.Request{AgentID: "a", Kind: policy.KindModel, Model: "claude-x", Amount: amt}
}

func pay(amt money.Nanos) policy.Request {
	return policy.Request{AgentID: "a", Kind: policy.KindPayment, Tool: "echo", Payee: payee, Amount: amt, HoldID: "h1", Network: "n"}
}

// Every rule on its own, and the allow path.
func TestEvaluateEachRule(t *testing.T) {
	cases := []struct {
		name string
		req  policy.Request
		mut  func(*policy.Snapshot)
		want policy.Verdict
		rule string
	}{
		{"allow model", model(100), nil, policy.Allow, policy.RuleAllow},
		{"allow payment", pay(100), nil, policy.Allow, policy.RuleAllow},
		{"killed", model(100), func(s *policy.Snapshot) { s.Status = policy.StatusKilled }, policy.Deny, policy.RuleKilled},
		{"revoked", model(100), func(s *policy.Snapshot) { s.Status = policy.StatusRevoked }, policy.Deny, policy.RuleRevoked},
		{"unknown status fails closed", model(100), func(s *policy.Snapshot) { s.Status = "weird" }, policy.Deny, policy.RuleRevoked},
		{"model prefix glob", func() policy.Request { r := model(100); r.Model = "claude-opus"; return r }(), nil, policy.Allow, policy.RuleAllow},
		{"model exact", func() policy.Request { r := model(100); r.Model = "gpt-4o"; return r }(), nil, policy.Allow, policy.RuleAllow},
		{"model out of scope", func() policy.Request { r := model(100); r.Model = "gpt-5"; return r }(), nil, policy.Deny, policy.RuleScopeModel},
		{"empty model list denies", model(100), func(s *policy.Snapshot) { s.Scope.Models = nil }, policy.Deny, policy.RuleScopeModel},
		{"star allows any model", func() policy.Request { r := model(100); r.Model = "anything"; return r }(),
			func(s *policy.Snapshot) { s.Scope.Models = []string{"*"} }, policy.Allow, policy.RuleAllow},
		{"tool out of scope", func() policy.Request { r := pay(100); r.Tool = "premium"; return r }(), nil, policy.Deny, policy.RuleScopeTool},
		{"payee out of scope", func() policy.Request { r := pay(100); r.Payee = "0x00000000000000000000000000000000000000AA"; return r }(), nil, policy.Deny, policy.RuleScopePayee},
		{"payee case-insensitive", func() policy.Request { r := pay(100); r.Payee = "0x70997970c51812dc3a010c7d01b50e0d17dc79c8"; return r }(), nil, policy.Allow, policy.RuleAllow},
		{"no payees means no payments", pay(100), func(s *policy.Snapshot) { s.Scope.Payees = nil }, policy.Deny, policy.RuleScopePayee},
		{"daily budget", model(100), func(s *policy.Snapshot) { s.DailySpent = 901 }, policy.Deny, policy.RuleBudgetDaily},
		{"daily budget exactly full", model(100), func(s *policy.Snapshot) { s.DailySpent = 900 }, policy.Allow, policy.RuleAllow},
		{"task budget", model(100), func(s *policy.Snapshot) { s.TaskSpent = 401 }, policy.Deny, policy.RuleBudgetTask},
		{"velocity", model(100), func(s *policy.Snapshot) { s.VelocitySpent = 201 }, policy.Deny, policy.RuleVelocity},
		{"velocity exactly full", model(100), func(s *policy.Snapshot) { s.VelocitySpent = 200 }, policy.Allow, policy.RuleAllow},
		{"over threshold", model(201), nil, policy.RequireApproval, policy.RuleApprovalThreshold},
		{"at threshold needs nothing", model(200), nil, policy.Allow, policy.RuleAllow},
		{"zero limits are unlimited", model(100000), func(s *policy.Snapshot) { s.Scope = policy.DefaultScope() }, policy.Allow, policy.RuleAllow},
		{"default scope has no payees", pay(100), func(s *policy.Snapshot) { s.Scope = policy.DefaultScope() }, policy.Deny, policy.RuleScopePayee},
		{"zero amount", model(0), nil, policy.Deny, policy.RuleInvalid},
		{"killed beats invalid", model(0), func(s *policy.Snapshot) { s.Status = policy.StatusKilled }, policy.Deny, policy.RuleKilled},
		{"negative amount", model(-5), nil, policy.Deny, policy.RuleInvalid},
		{"no agent", policy.Request{Kind: policy.KindModel, Model: "claude-x", Amount: 1}, nil, policy.Deny, policy.RuleUnknownAgent},
		{"unknown kind", policy.Request{AgentID: "a", Kind: "x", Amount: 1}, nil, policy.Deny, policy.RuleInvalid},
		{"snapshot of another agent", model(100), func(s *policy.Snapshot) { s.AgentID = "b" }, policy.Deny, policy.RuleUnknownAgent},
		{"approved skips only the threshold", model(201), nil, policy.RequireApproval, policy.RuleApprovalThreshold},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := snap()
			if c.mut != nil {
				c.mut(&s)
			}
			d := policy.Evaluate(c.req, s)
			if d.Verdict != c.want || d.Rule != c.rule {
				t.Fatalf("got %s/%s (%s), want %s/%s", d.Verdict, d.Rule, d.Reason, c.want, c.rule)
			}
			if d.Reason == "" {
				t.Fatal("decision without a reason")
			}
			_, ok := d.Allowed()
			if ok != (c.want == policy.Allow) {
				t.Fatalf("Allowed() = %v for %s", ok, c.want)
			}
		})
	}
}

// The fixed order: for every pair of rules that both fire, the earlier wins.
func TestEvaluateRuleOrderCollisions(t *testing.T) {
	// Each breaker mutates a request/snapshot so that one rule fires.
	type breaker struct {
		rule string
		do   func(*policy.Request, *policy.Snapshot)
	}
	chain := []breaker{
		{policy.RuleKilled, func(_ *policy.Request, s *policy.Snapshot) { s.Status = policy.StatusKilled }},
		{policy.RuleScopeModel, func(r *policy.Request, _ *policy.Snapshot) { r.Model = "gpt-5" }},
		{policy.RuleBudgetDaily, func(_ *policy.Request, s *policy.Snapshot) { s.DailySpent = 950 }},
		{policy.RuleBudgetTask, func(_ *policy.Request, s *policy.Snapshot) { s.TaskSpent = 450 }},
		{policy.RuleVelocity, func(_ *policy.Request, s *policy.Snapshot) { s.VelocitySpent = 250 }},
		{policy.RuleApprovalThreshold, func(r *policy.Request, _ *policy.Snapshot) { r.Amount = 210 }},
	}
	for i := range chain {
		for j := i + 1; j < len(chain); j++ {
			r, s := model(100), snap()
			chain[i].do(&r, &s)
			chain[j].do(&r, &s)
			if d := policy.Evaluate(r, s); d.Rule != chain[i].rule {
				t.Errorf("%s + %s: got %s, want the earlier rule %s", chain[i].rule, chain[j].rule, d.Rule, chain[i].rule)
			}
		}
	}
	// All of them at once: the first one.
	r, s := model(100), snap()
	for _, b := range chain {
		b.do(&r, &s)
	}
	if d := policy.Evaluate(r, s); d.Rule != policy.RuleKilled {
		t.Fatalf("all rules broken: got %s", d.Rule)
	}
	// Revoked beats scope as well, and scope tool/payee order: tool first.
	r, s = pay(100), snap()
	r.Tool, r.Payee = "premium", "0x00000000000000000000000000000000000000AA"
	if d := policy.Evaluate(r, s); d.Rule != policy.RuleScopeTool {
		t.Fatalf("tool and payee both wrong: got %s", d.Rule)
	}
}

// An approval lifts the threshold and nothing else.
func TestApprovedSkipsOnlyTheThreshold(t *testing.T) {
	r, s := model(210), snap()
	r.Approved = true
	if d := policy.Evaluate(r, s); d.Verdict != policy.Allow {
		t.Fatalf("approved over-threshold request: %s/%s", d.Verdict, d.Rule)
	}
	for name, mut := range map[string]func(*policy.Snapshot){
		"killed":   func(s *policy.Snapshot) { s.Status = policy.StatusKilled },
		"scope":    func(s *policy.Snapshot) { s.Scope.Models = []string{"nope"} },
		"daily":    func(s *policy.Snapshot) { s.DailySpent = 900 },
		"task":     func(s *policy.Snapshot) { s.TaskSpent = 400 },
		"velocity": func(s *policy.Snapshot) { s.VelocitySpent = 200 },
	} {
		s := snap()
		mut(&s)
		r := model(210)
		r.Approved = true
		if d := policy.Evaluate(r, s); d.Verdict != policy.Deny {
			t.Errorf("approval bypassed %s: %s/%s", name, d.Verdict, d.Rule)
		}
	}
}

func TestEvaluateIsPure(t *testing.T) {
	r, s := model(100), snap()
	a, b := policy.Evaluate(r, s), policy.Evaluate(r, s)
	if a.Verdict != b.Verdict || a.Rule != b.Rule || a.Reason != b.Reason {
		t.Fatal("same input, different decision")
	}
}

// I10: the zero Allowed is no proof, an Allowed is tied to one hold and is
// single use, and copies share the single use.
func TestAllowedSingleUseTiedToHold(t *testing.T) {
	if (policy.Allowed{}).Valid() {
		t.Fatal("zero Allowed is valid")
	}
	if err := (policy.Allowed{}).Use("h1"); err == nil {
		t.Fatal("zero Allowed was usable")
	}
	d := policy.Evaluate(pay(100), snap())
	a, ok := d.Allowed()
	if !ok || !a.Valid() {
		t.Fatal("no Allowed from an Allow")
	}
	if a.Request().Amount != 100 || a.Request().HoldID != "h1" {
		t.Fatalf("request not carried: %+v", a.Request())
	}
	if err := a.Use("other"); err != policy.ErrWrongHold {
		t.Fatalf("other hold: %v", err)
	}
	copyOf := a
	if err := a.Use("h1"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := a.Use("h1"); err != policy.ErrAlreadyUsed {
		t.Fatalf("second use: %v", err)
	}
	if err := copyOf.Use("h1"); err != policy.ErrAlreadyUsed {
		t.Fatalf("copy reused the approval: %v", err)
	}
	// A request with no hold id can never be used to sign.
	noHold := pay(100)
	noHold.HoldID = ""
	d = policy.Evaluate(noHold, snap())
	a, _ = d.Allowed()
	if err := a.Use(""); err == nil {
		t.Fatal("Allowed without a hold id was usable")
	}
	// Denials and approval requests carry no Allowed.
	if _, ok := policy.Evaluate(model(500), snap()).Allowed(); ok {
		t.Fatal("RequireApproval produced an Allowed")
	}
}
