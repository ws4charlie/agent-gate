// Package cedarpolicy runs AgentGate's runtime policy as Cedar policies kept
// in git, beside the Go engine (policy.Evaluate). The caller runs both and
// records both. In shadow mode the Go decision counts; with the gateway's
// --cedar-enforce the Cedar verdict counts and an error here is a deny. The
// engine itself only answers; the choice is made in internal/server.
package cedarpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	cedar "github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"

	"github.com/ws4charlie/agent-gate/internal/policy"
)

// RuleNoPermit is the rule reported when Cedar denies without any forbid
// firing: no permit matched.
const RuleNoPermit = "cedar_no_permit"

// Verdict recorded when Cedar itself failed.
const VerdictError = "error"

// order is the Go engine's rule order. When several forbids fire, the
// earliest one here is the one Go would have reported.
var order = []string{
	policy.RuleKilled, policy.RuleRevoked, policy.RuleInvalid,
	policy.RuleScopeModel, policy.RuleScopeTool, policy.RuleScopePayee,
	policy.RuleRegistryUnapproved,
	policy.RuleBudgetDaily, policy.RuleBudgetTask, policy.RuleVelocity,
	policy.RuleApprovalThreshold,
}

type ruleInfo struct {
	rank    int
	rule    string
	verdict string
}

// Engine evaluates the Cedar policies. Safe for concurrent use.
type Engine struct {
	ps    *cedar.PolicySet
	rules map[cedar.PolicyID]ruleInfo // forbids only
}

// Load reads every *.cedar file in dir into one policy set. Each forbid must
// carry @rule("<id>") with an id from the Go engine's rule order.
func Load(dir string) (*Engine, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.cedar"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("cedar: no *.cedar files in %s", dir)
	}
	ps := cedar.NewPolicySet()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		set, err := cedar.NewPolicySetFromBytes(f, b)
		if err != nil {
			return nil, fmt.Errorf("cedar: %w", err)
		}
		for id, p := range set.All() {
			ps.Add(cedar.PolicyID(fmt.Sprintf("%s#%s", filepath.Base(f), id)), p)
		}
	}
	e := &Engine{ps: ps, rules: map[cedar.PolicyID]ruleInfo{}}
	rank := map[string]int{}
	for i, r := range order {
		rank[r] = i
	}
	permits := 0
	for id, p := range ps.All() {
		if p.Effect() == cedar.Permit {
			permits++
			continue
		}
		ann := p.Annotations()
		rule := string(ann["rule"])
		rk, ok := rank[rule]
		if !ok {
			return nil, fmt.Errorf("cedar: forbid %s has no known @rule (got %q)", id, rule)
		}
		verdict := string(policy.Deny)
		if v, ok := ann["verdict"]; ok {
			if string(v) != string(policy.RequireApproval) {
				return nil, fmt.Errorf("cedar: forbid %s has unknown @verdict %q", id, v)
			}
			verdict = string(v)
		}
		e.rules[id] = ruleInfo{rank: rk, rule: rule, verdict: verdict}
	}
	if permits == 0 {
		return nil, fmt.Errorf("cedar: no permit policy in %s", dir)
	}
	return e, nil
}

// actionFor maps a request kind to its Cedar action. An unknown kind gets an
// action no permit mentions; the invalid_request forbid reports it.
func actionFor(k policy.Kind) string {
	switch k {
	case policy.KindModel:
		return "call_model"
	case policy.KindTool:
		return "call_tool"
	case policy.KindPayment:
		return "pay"
	}
	return "invalid"
}

func long(n int64) types.Long { return types.Long(n) }

// Decide evaluates one request and maps the Cedar result back to the Go
// engine's vocabulary. Any Cedar failure, or a panic, comes back as err.
//
// Request and snapshot are translated like this: the agent is the principal
// with its status and limits as attributes (0 = no limit); the kind is the
// action; the model or tool is the resource; amounts and spend go in the
// context. Two things are computed in Go and handed over as facts:
//
//   - model_in_scope, tool_in_scope, payee_in_scope: wildcard allowlists
//     ("claude-*") are not set membership, so policy.MatchAny answers them.
//   - registry_on, registry_approved: the AI Registry lookup is done by the
//     caller (Snapshot.Registry); Cedar only reads the result.
//   - daily_total, task_total, velocity_total: spent + amount, summed with
//     the same int64 arithmetic as policy.Evaluate. Cedar arithmetic fails on
//     overflow, and a failing policy would be skipped silently.
func (e *Engine) Decide(r policy.Request, s policy.Snapshot) (verdict, rule string, err error) {
	defer func() {
		if p := recover(); p != nil {
			verdict, rule, err = "", "", fmt.Errorf("cedar: panic: %v", p)
		}
	}()
	if s.AgentID != r.AgentID {
		return "", "", fmt.Errorf("cedar: snapshot belongs to another agent")
	}
	principal := cedar.NewEntityUID("Agent", cedar.String(r.AgentID))
	sc := s.Scope
	entities := cedar.EntityMap{principal: {
		UID: principal,
		Attributes: cedar.NewRecord(cedar.RecordMap{
			"status":              cedar.String(s.Status),
			"daily_budget":        long(int64(sc.DailyBudget)),
			"task_budget":         long(int64(sc.TaskBudget)),
			"velocity_per_minute": long(int64(sc.VelocityPerMinute)),
			"approval_threshold":  long(int64(sc.ApprovalThreshold)),
		}),
	}}
	resType, resID := cedar.EntityType("Tool"), r.Tool
	if r.Kind == policy.KindModel {
		resType, resID = "Model", r.Model
	}
	amt := int64(r.Amount)
	valid := r.AgentID != "" && r.Amount > 0 &&
		(r.Kind == policy.KindModel || r.Kind == policy.KindTool || r.Kind == policy.KindPayment)
	req := cedar.Request{
		Principal: principal,
		Action:    cedar.NewEntityUID("Action", cedar.String(actionFor(r.Kind))),
		Resource:  cedar.NewEntityUID(resType, cedar.String(resID)),
		Context: cedar.NewRecord(cedar.RecordMap{
			"amount":         long(amt),
			"daily_total":    long(int64(s.DailySpent) + amt),
			"task_total":     long(int64(s.TaskSpent) + amt),
			"velocity_total": long(int64(s.VelocitySpent) + amt),
			"approved":       cedar.Boolean(r.Approved),
			"valid":          cedar.Boolean(valid),
			"model_in_scope": cedar.Boolean(policy.MatchAny(sc.Models, r.Model, false)),
			"tool_in_scope":  cedar.Boolean(policy.MatchAny(sc.Tools, r.Tool, false)),
			"payee_in_scope": cedar.Boolean(policy.MatchAny(sc.Payees, r.Payee, true)),
			// The registry state of the model or tool, looked up by the caller.
			"registry_on":       cedar.Boolean(s.Registry.On),
			"registry_approved": cedar.Boolean(s.Registry.Passes()),
		}),
	}
	dec, diag := cedar.Authorize(e.ps, entities, req)
	if len(diag.Errors) > 0 {
		// A policy that errors is skipped by Cedar; for an equivalence check
		// that is a failure, not a verdict.
		return "", "", fmt.Errorf("cedar: policy %s: %s", diag.Errors[0].PolicyID, diag.Errors[0].Message)
	}
	if dec == cedar.Allow {
		return string(policy.Allow), policy.RuleAllow, nil
	}
	best, found := ruleInfo{rank: len(order)}, false
	for _, reason := range diag.Reasons {
		ri, ok := e.rules[reason.PolicyID]
		if ok && ri.rank < best.rank {
			best, found = ri, true
		}
	}
	if !found {
		return string(policy.Deny), RuleNoPermit, nil
	}
	return best.verdict, best.rule, nil
}
