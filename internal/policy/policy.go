// Package policy is the decision engine of "custody for agents": one pure
// function, Evaluate, that sits in front of model calls, tool calls and
// payments.
//
// Evaluate does no I/O. The caller (the server) loads a Snapshot of the
// agent's scope and counters, asks for a Decision, and records it. Pure means
// every rule and every collision between rules is a table test, and the same
// answer comes out whatever the database is doing.
//
// Rule order is fixed. The first rule that fires decides, so a request that
// breaks two rules always reports the earlier one:
//
//  1. kill / revoke   agent_killed, agent_revoked
//  2. scope allowlist scope_model, scope_tool, scope_payee
//  3. budget          budget_daily, budget_task
//  4. velocity        velocity
//  5. approval        approval_threshold (RequireApproval, not Deny)
//
// Why this order: a dead credential must never learn anything else about the
// policy; "you may not do that at all" (scope) outranks "not right now"
// (budget, velocity); and a human is only asked about requests that are
// otherwise allowed, so an approver never sees a request that was going to be
// refused anyway.
//
// The snapshot counters are a read, so Evaluate alone cannot enforce a budget
// against concurrent requests (I5 for budgets). Enforcement is the conditional
// UPDATE in agents.Store.Charge; Evaluate gives the precise rule id and a
// cheap early refusal.
//
// I10 (screen before sign): Allowed has unexported fields, so code outside this
// package cannot build one. The only source of an Allowed is a Decision from
// Evaluate. The wallet's signing path takes an Allowed and spends it with
// Use, so "signing without screening", and screening once and signing twice,
// do not compile or do not run.
package policy

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// Kind is what the request wants to do.
type Kind string

const (
	KindModel   Kind = "model"
	KindTool    Kind = "tool"
	KindPayment Kind = "payment"
)

// Status of an agent credential.
type Status string

const (
	StatusActive  Status = "active"
	StatusKilled  Status = "killed"  // kill switch; reversible with `kill --resume`
	StatusRevoked Status = "revoked" // permanent
)

// Scope is what one agent credential may do. A zero limit means "no limit";
// an empty allowlist means "nothing allowed" (default-deny), so a payee list
// must be written down before an agent can pay anyone.
type Scope struct {
	Models []string `json:"models"` // exact names or a trailing * ("claude-*", "*")
	Tools  []string `json:"tools"`
	Payees []string `json:"payees"` // 0x addresses, compared case-insensitively

	DailyBudget       money.Nanos `json:"daily_budget_nanos"`        // per UTC day
	TaskBudget        money.Nanos `json:"task_budget_nanos"`         // per X-AgentGate-Task
	VelocityPerMinute money.Nanos `json:"velocity_per_minute_nanos"` // sliding 60s window
	ApprovalThreshold money.Nanos `json:"approval_threshold_nanos"`  // a single request above this needs a human

	// Secrets says what happens when a tool call carries a key or password:
	// SecretsConfirm (the default, "") holds it and tells the agent, which may
	// resend it with a confirm code; SecretsBlock refuses it outright.
	Secrets string `json:"secrets,omitempty"`
}

// Values of Scope.Secrets.
const (
	SecretsConfirm = "confirm"
	SecretsBlock   = "block"
)

// SecretMode is Secrets with the default filled in.
func (s Scope) SecretMode() string {
	if s.Secrets == SecretsBlock {
		return SecretsBlock
	}
	return SecretsConfirm
}

// DefaultScope is what a migrated T2 API key gets: any model, any tool, no
// limits, no payees (so no payments until someone lists a payee).
func DefaultScope() Scope { return Scope{Models: []string{"*"}, Tools: []string{"*"}} }

// Request is one thing an agent wants to do.
type Request struct {
	AgentID string
	Kind    Kind
	Model   string // KindModel
	Tool    string // KindTool, KindPayment
	Payee   string // KindPayment: 0x address
	Network string // KindPayment
	Amount  money.Nanos
	TaskID  string // "" is the shared default task

	// HoldID is the ledger hold that funds a payment. It binds the Allowed
	// to that hold (single use, I10).
	HoldID string
	// Approved is set by the caller only after a human approved exactly this
	// request (approval store, fingerprint checked). It skips rule 5 and
	// nothing else: an approval never lifts a scope, budget or velocity limit.
	Approved bool
}

// Snapshot is the read-only state Evaluate needs.
type Snapshot struct {
	AgentID       string
	Status        Status
	Scope         Scope
	DailySpent    money.Nanos // today (UTC), held plus settled
	TaskSpent     money.Nanos // for Request.TaskID
	VelocitySpent money.Nanos // last 60 seconds
}

// Verdict is the outcome class.
type Verdict string

const (
	Allow           Verdict = "allow"
	Deny            Verdict = "deny"
	RequireApproval Verdict = "require_approval"
)

// Rule ids. They are stable: they appear in audit rows and API errors.
const (
	RuleInvalid           = "invalid_request"
	RuleUnknownAgent      = "unknown_agent"
	RuleKilled            = "agent_killed"
	RuleRevoked           = "agent_revoked"
	RuleScopeModel        = "scope_model"
	RuleScopeTool         = "scope_tool"
	RuleScopePayee        = "scope_payee"
	RuleBudgetDaily       = "budget_daily"
	RuleBudgetTask        = "budget_task"
	RuleVelocity          = "velocity"
	RuleApprovalThreshold = "approval_threshold"
	RuleAllow             = "allow"
)

// Decision is the result of Evaluate.
type Decision struct {
	Verdict Verdict
	Rule    string
	Reason  string
	allowed Allowed
}

// Allowed returns the proof of approval, only for an Allow decision.
func (d Decision) Allowed() (Allowed, bool) { return d.allowed, d.Verdict == Allow && d.allowed.ok }

// Evaluate decides one request. Pure: no I/O, no clock, no randomness.
func Evaluate(r Request, s Snapshot) Decision {
	deny := func(rule, format string, a ...any) Decision {
		return Decision{Verdict: Deny, Rule: rule, Reason: fmt.Sprintf(format, a...)}
	}
	if s.AgentID != r.AgentID {
		return deny(RuleUnknownAgent, "snapshot belongs to another agent")
	}

	// 1. kill / revoke. First of all, malformed requests included: a dead
	// credential learns nothing but that it is dead.
	switch s.Status {
	case StatusActive:
	case StatusKilled:
		return deny(RuleKilled, "agent was killed")
	case StatusRevoked:
		return deny(RuleRevoked, "credential was revoked")
	default:
		return deny(RuleRevoked, "credential is not active") // unknown status fails closed
	}
	if r.AgentID == "" || r.Amount <= 0 || (r.Kind != KindModel && r.Kind != KindTool && r.Kind != KindPayment) {
		return deny(RuleInvalid, "agent, kind and a positive amount are required")
	}

	// 2. scope allowlist
	switch r.Kind {
	case KindModel:
		if !matchAny(s.Scope.Models, r.Model, false) {
			return deny(RuleScopeModel, "model %q is outside this agent's scope", r.Model)
		}
	case KindTool, KindPayment:
		if !matchAny(s.Scope.Tools, r.Tool, false) {
			return deny(RuleScopeTool, "tool %q is outside this agent's scope", r.Tool)
		}
	}
	if r.Kind == KindPayment && !matchAny(s.Scope.Payees, r.Payee, true) {
		return deny(RuleScopePayee, "payee %s is not on this agent's allowlist", r.Payee)
	}

	// 3. budget: daily, then per task
	if l := s.Scope.DailyBudget; l > 0 && s.DailySpent+r.Amount > l {
		return deny(RuleBudgetDaily, "%d + %d nanos exceeds the daily budget %d", s.DailySpent, r.Amount, l)
	}
	if l := s.Scope.TaskBudget; l > 0 && s.TaskSpent+r.Amount > l {
		return deny(RuleBudgetTask, "%d + %d nanos exceeds the task budget %d", s.TaskSpent, r.Amount, l)
	}

	// 4. velocity
	if l := s.Scope.VelocityPerMinute; l > 0 && s.VelocitySpent+r.Amount > l {
		return deny(RuleVelocity, "%d + %d nanos in the last minute exceeds %d per minute", s.VelocitySpent, r.Amount, l)
	}

	// 5. approval threshold
	if t := s.Scope.ApprovalThreshold; t > 0 && r.Amount > t && !r.Approved {
		return Decision{Verdict: RequireApproval, Rule: RuleApprovalThreshold,
			Reason: fmt.Sprintf("%d nanos is above the approval threshold %d", r.Amount, t)}
	}

	return Decision{Verdict: Allow, Rule: RuleAllow, Reason: "within scope and limits",
		allowed: Allowed{req: r, ok: true, used: new(atomic.Bool)}}
}

// matchAny reports whether name matches one pattern: exact, "*", or a prefix
// followed by a trailing "*".
func matchAny(patterns []string, name string, fold bool) bool {
	if name == "" {
		return false
	}
	if fold {
		name = strings.ToLower(name)
	}
	for _, p := range patterns {
		if fold {
			p = strings.ToLower(p)
		}
		if p == name || p == "*" {
			return true
		}
		if pre, ok := strings.CutSuffix(p, "*"); ok && strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

// ErrNotApproved and friends are what Use returns.
var (
	ErrNotApproved = errors.New("policy: not approved")
	ErrWrongHold   = errors.New("policy: approval belongs to a different hold")
	ErrAlreadyUsed = errors.New("policy: approval was already used")
)

// Allowed is proof that Evaluate approved a Request. The zero value is not a
// proof: Valid reports false for it. Copies share one used flag, so copying an
// Allowed does not give a second use.
type Allowed struct {
	req  Request
	ok   bool
	used *atomic.Bool
}

func (a Allowed) Request() Request { return a.req }
func (a Allowed) Valid() bool      { return a.ok && a.used != nil }

// Use spends the approval for one signature, tied to its hold id (I10). It
// fails if the Allowed is the zero value, was granted for another hold, or was
// already used.
func (a Allowed) Use(holdID string) error {
	if !a.Valid() {
		return ErrNotApproved
	}
	if holdID == "" || a.req.HoldID != holdID {
		return ErrWrongHold
	}
	if !a.used.CompareAndSwap(false, true) {
		return ErrAlreadyUsed
	}
	return nil
}
