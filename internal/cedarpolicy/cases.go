package cedarpolicy

import (
	"fmt"
	"math/rand"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

// Case is one (request, snapshot) pair to run through both engines.
type Case struct {
	Name string
	Req  policy.Request
	Snap policy.Snapshot
}

// Diff is a case where Cedar and the Go engine disagree (or Cedar failed).
type Diff struct {
	Case      Case
	GoVerdict string
	GoRule    string
	Verdict   string
	Rule      string
	Err       error
}

func (d Diff) String() string {
	if d.Err != nil {
		return fmt.Sprintf("%s: cedar error: %v (go %s/%s)", d.Case.Name, d.Err, d.GoVerdict, d.GoRule)
	}
	return fmt.Sprintf("%s: go %s/%s, cedar %s/%s", d.Case.Name, d.GoVerdict, d.GoRule, d.Verdict, d.Rule)
}

// Check runs every case through policy.Evaluate and the engine and returns the
// differences.
func Check(e *Engine, cases []Case) []Diff {
	var out []Diff
	for _, c := range cases {
		g := policy.Evaluate(c.Req, c.Snap)
		v, r, err := e.Decide(c.Req, c.Snap)
		if err != nil || v != string(g.Verdict) || r != g.Rule {
			out = append(out, Diff{Case: c, GoVerdict: string(g.Verdict), GoRule: g.Rule, Verdict: v, Rule: r, Err: err})
		}
	}
	return out
}

// BuiltinCases is the table of internal/policy/policy_test.go (copied, so the
// CLI can run it without test code) followed by n generated cases from a fixed
// seed.
func BuiltinCases(n int) []Case {
	return append(TableCases(), GeneratedCases(1, n)...)
}

const payee = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"

func openScope() policy.Scope {
	return policy.Scope{
		Models: []string{"claude-*", "gpt-4o"}, Tools: []string{"echo"}, Payees: []string{payee},
		DailyBudget: 1000, TaskBudget: 500, VelocityPerMinute: 300, ApprovalThreshold: 200,
	}
}

func baseSnap() policy.Snapshot {
	return policy.Snapshot{AgentID: "a", Status: policy.StatusActive, Scope: openScope()}
}

func modelReq(amt money.Nanos) policy.Request {
	return policy.Request{AgentID: "a", Kind: policy.KindModel, Model: "claude-x", Amount: amt}
}

func payReq(amt money.Nanos) policy.Request {
	return policy.Request{AgentID: "a", Kind: policy.KindPayment, Tool: "echo", Payee: payee, Amount: amt, HoldID: "h1", Network: "n"}
}

// TableCases mirrors the cases of TestEvaluateEachRule, plus the approved
// variants and the pairwise rule-order collisions. The "unknown agent" cases
// are left out: a snapshot of another agent is not expressible as a Cedar
// request (Decide returns an error for it).
func TableCases() []Case {
	type mut = func(*policy.Request, *policy.Snapshot)
	var cs []Case
	add := func(name string, r policy.Request, m func(*policy.Snapshot)) {
		s := baseSnap()
		if m != nil {
			m(&s)
		}
		cs = append(cs, Case{name, r, s})
	}
	withModel := func(name string) policy.Request { r := modelReq(100); r.Model = name; return r }
	withPay := func(f func(*policy.Request)) policy.Request { r := payReq(100); f(&r); return r }

	add("allow model", modelReq(100), nil)
	add("allow payment", payReq(100), nil)
	add("killed", modelReq(100), func(s *policy.Snapshot) { s.Status = policy.StatusKilled })
	add("revoked", modelReq(100), func(s *policy.Snapshot) { s.Status = policy.StatusRevoked })
	add("unknown status fails closed", modelReq(100), func(s *policy.Snapshot) { s.Status = "weird" })
	add("model prefix glob", withModel("claude-opus"), nil)
	add("model exact", withModel("gpt-4o"), nil)
	add("model out of scope", withModel("gpt-5"), nil)
	add("empty model list denies", modelReq(100), func(s *policy.Snapshot) { s.Scope.Models = nil })
	add("star allows any model", withModel("anything"), func(s *policy.Snapshot) { s.Scope.Models = []string{"*"} })
	add("tool out of scope", withPay(func(r *policy.Request) { r.Tool = "premium" }), nil)
	add("payee out of scope", withPay(func(r *policy.Request) { r.Payee = "0x00000000000000000000000000000000000000AA" }), nil)
	add("payee case-insensitive", withPay(func(r *policy.Request) { r.Payee = "0x70997970c51812dc3a010c7d01b50e0d17dc79c8" }), nil)
	add("no payees means no payments", payReq(100), func(s *policy.Snapshot) { s.Scope.Payees = nil })
	add("daily budget", modelReq(100), func(s *policy.Snapshot) { s.DailySpent = 901 })
	add("daily budget exactly full", modelReq(100), func(s *policy.Snapshot) { s.DailySpent = 900 })
	add("task budget", modelReq(100), func(s *policy.Snapshot) { s.TaskSpent = 401 })
	add("velocity", modelReq(100), func(s *policy.Snapshot) { s.VelocitySpent = 201 })
	add("velocity exactly full", modelReq(100), func(s *policy.Snapshot) { s.VelocitySpent = 200 })
	add("over threshold", modelReq(201), nil)
	add("at threshold needs nothing", modelReq(200), nil)
	add("zero limits are unlimited", modelReq(100000), func(s *policy.Snapshot) { s.Scope = policy.DefaultScope() })
	add("default scope has no payees", payReq(100), func(s *policy.Snapshot) { s.Scope = policy.DefaultScope() })
	add("zero amount", modelReq(0), nil)
	add("killed beats invalid", modelReq(0), func(s *policy.Snapshot) { s.Status = policy.StatusKilled })
	add("negative amount", modelReq(-5), nil)
	add("unknown kind", policy.Request{AgentID: "a", Kind: "x", Amount: 1}, nil)
	add("approved over threshold", func() policy.Request { r := modelReq(201); r.Approved = true; return r }(), nil)
	add("approved does not lift scope", func() policy.Request { r := withModel("gpt-5"); r.Amount = 201; r.Approved = true; return r }(), nil)
	add("approved does not lift daily", func() policy.Request { r := modelReq(210); r.Approved = true; return r }(), func(s *policy.Snapshot) { s.DailySpent = 900 })

	// Every pair of rules that both fire: the earlier one must win.
	chain := []mut{
		func(_ *policy.Request, s *policy.Snapshot) { s.Status = policy.StatusKilled },
		func(r *policy.Request, _ *policy.Snapshot) { r.Model = "gpt-5" },
		func(_ *policy.Request, s *policy.Snapshot) { s.DailySpent = 950 },
		func(_ *policy.Request, s *policy.Snapshot) { s.TaskSpent = 450 },
		func(_ *policy.Request, s *policy.Snapshot) { s.VelocitySpent = 250 },
		func(r *policy.Request, _ *policy.Snapshot) { r.Amount = 210 },
	}
	for i := range chain {
		for j := i + 1; j < len(chain); j++ {
			r, s := modelReq(100), baseSnap()
			chain[i](&r, &s)
			chain[j](&r, &s)
			cs = append(cs, Case{fmt.Sprintf("collision %d+%d", i, j), r, s})
		}
	}
	r, s := modelReq(100), baseSnap()
	for _, f := range chain {
		f(&r, &s)
	}
	cs = append(cs, Case{"all rules broken", r, s})
	r, s = payReq(100), baseSnap()
	r.Tool, r.Payee = "premium", "0x00000000000000000000000000000000000000AA"
	cs = append(cs, Case{"tool and payee both wrong", r, s})
	return cs
}

// GeneratedCases returns n pseudo-random cases from a fixed seed. They cover
// every status, kind and allowlist shape, zero and non-zero limits, and
// amounts and spend chosen to land at, just below and just above each limit.
func GeneratedCases(seed int64, n int) []Case {
	rng := rand.New(rand.NewSource(seed))
	pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
	lists := [][]string{nil, {"*"}, {"claude-*", "gpt-4o"}, {"echo"}, {"gpt-4o"}, {"cl*"}, {"claude-x", "*"}, {payee}, {"0X70997970C51812DC3A010C7D01B50E0D17DC79C8"}, {"0x7099*"}}
	names := []string{"", "claude-x", "claude-opus", "gpt-4o", "gpt-5", "echo", "premium", "cl", "c", "*", payee, "0x70997970c51812dc3a010c7d01b50e0d17dc79c8", "0x00000000000000000000000000000000000000AA"}
	limits := []money.Nanos{0, 0, 1, 100, 1000}
	statuses := []policy.Status{policy.StatusActive, policy.StatusActive, policy.StatusActive, policy.StatusKilled, policy.StatusRevoked, "weird", ""}
	kinds := []policy.Kind{policy.KindModel, policy.KindTool, policy.KindPayment, policy.KindModel, policy.KindTool, policy.KindPayment, "x", ""}
	// near returns a value at, just below or just above l (or something small).
	near := func(l money.Nanos) money.Nanos {
		v := l + money.Nanos(rng.Intn(3)-1)
		if rng.Intn(5) == 0 || v < 0 {
			v = money.Nanos(rng.Intn(5))
		}
		return v
	}
	list := func() []string { return lists[rng.Intn(len(lists))] }
	out := make([]Case, 0, n)
	for i := 0; i < n; i++ {
		sc := policy.Scope{Models: list(), Tools: list(), Payees: list(),
			DailyBudget: limits[rng.Intn(len(limits))], TaskBudget: limits[rng.Intn(len(limits))],
			VelocityPerMinute: limits[rng.Intn(len(limits))], ApprovalThreshold: limits[rng.Intn(len(limits))]}
		agent := pick("a", "a", "a", "")
		s := policy.Snapshot{AgentID: agent, Status: statuses[rng.Intn(len(statuses))], Scope: sc}
		r := policy.Request{AgentID: agent, Kind: kinds[rng.Intn(len(kinds))],
			Model: names[rng.Intn(len(names))], Tool: names[rng.Intn(len(names))], Payee: names[rng.Intn(len(names))],
			Approved: rng.Intn(3) == 0}
		// The amount sits near one of the limits (or at 0, -1, 1); spend is
		// chosen so that spent+amount lands at, just below or just above a limit.
		switch rng.Intn(4) {
		case 0:
			r.Amount = near(sc.ApprovalThreshold)
		case 1:
			r.Amount = near(sc.DailyBudget) / 2
		case 2:
			r.Amount = money.Nanos(rng.Intn(3) - 1)
		default:
			r.Amount = money.Nanos(rng.Intn(150) + 1)
		}
		fit := func(l money.Nanos) money.Nanos {
			if rng.Intn(4) == 0 {
				return money.Nanos(rng.Intn(1200))
			}
			return max(0, near(l)-r.Amount)
		}
		s.DailySpent, s.TaskSpent, s.VelocitySpent = fit(sc.DailyBudget), fit(sc.TaskBudget), fit(sc.VelocityPerMinute)
		out = append(out, Case{Name: fmt.Sprintf("gen-%d", i), Req: r, Snap: s})
	}
	return out
}
