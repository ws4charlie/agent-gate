package cedarpolicy_test

import (
	"testing"

	"github.com/ws4charlie/agent-gate/internal/cedarpolicy"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

const dir = "../../policies/cedar"

func load(t *testing.T) *cedarpolicy.Engine {
	t.Helper()
	e, err := cedarpolicy.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func report(t *testing.T, diffs []cedarpolicy.Diff) {
	t.Helper()
	for i, d := range diffs {
		if i == 10 {
			t.Errorf("... and %d more", len(diffs)-10)
			break
		}
		t.Error(d)
	}
}

// The table of internal/policy/policy_test.go: Cedar and Go give the same
// verdict and rule.
func TestEquivalenceTable(t *testing.T) {
	cases := cedarpolicy.TableCases()
	if len(cases) < 40 {
		t.Fatalf("table shrank to %d cases", len(cases))
	}
	report(t, cedarpolicy.Check(load(t), cases))
}

// 5,000+ generated cases from a fixed seed: zero differences.
func TestEquivalenceProperty(t *testing.T) {
	cases := cedarpolicy.GeneratedCases(20261117, 6000)
	report(t, cedarpolicy.Check(load(t), cases))

	// The generator must really reach every outcome and rule.
	seen := map[string]int{}
	kinds := map[policy.Kind]bool{}
	statuses := map[policy.Status]bool{}
	for _, c := range cases {
		d := policy.Evaluate(c.Req, c.Snap)
		seen[d.Rule]++
		kinds[c.Req.Kind] = true
		statuses[c.Snap.Status] = true
	}
	for _, r := range []string{policy.RuleAllow, policy.RuleKilled, policy.RuleRevoked, policy.RuleInvalid,
		policy.RuleScopeModel, policy.RuleScopeTool, policy.RuleScopePayee, policy.RuleBudgetDaily,
		policy.RuleBudgetTask, policy.RuleVelocity, policy.RuleApprovalThreshold} {
		if seen[r] < 5 {
			t.Errorf("generator reached rule %s only %d times", r, seen[r])
		}
	}
	if len(kinds) < 5 || len(statuses) < 5 {
		t.Errorf("generator coverage: %d kinds, %d statuses", len(kinds), len(statuses))
	}
}

func TestNoPermitAndErrors(t *testing.T) {
	e := load(t)
	// A snapshot of another agent cannot be expressed.
	if _, _, err := e.Decide(policy.Request{AgentID: "a"}, policy.Snapshot{AgentID: "b"}); err == nil {
		t.Fatal("expected an error for a foreign snapshot")
	}
}

func TestLoadRejectsBadPolicies(t *testing.T) {
	if _, err := cedarpolicy.Load(t.TempDir()); err == nil {
		t.Fatal("empty dir should fail")
	}
}
