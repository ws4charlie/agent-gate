// Package policy is the thin first cut of "custody for agents": a per-agent
// budget and a payee allowlist, read from config. T4 replaces the rules with
// the real engine but keeps the shape below.
//
// I10 (screen before sign): Allowed has unexported fields, so code outside this
// package cannot build one — not with Allowed{}, not with a struct literal that
// sets fields. The only source of an Allowed is Policy.Check. The wallet's only
// signing entry point takes an Allowed, so "signing without screening" does not
// compile.
package policy

import (
	"fmt"
	"strings"
	"sync"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// Request is one payment someone wants to make.
type Request struct {
	AccountID string // the authenticated account asking; must own the agent
	AgentID   string
	Payee     string // 0x address
	Amount    money.Nanos
	Network   string
}

// Allowed is proof that Check approved a Request. Zero value is not a proof:
// Valid reports false for it.
type Allowed struct {
	req Request
	ok  bool
}

func (a Allowed) Request() Request { return a.req }
func (a Allowed) Valid() bool      { return a.ok }

// Denied is a refusal with a stable reason code.
type Denied struct {
	Reason string // payee_not_allowed | over_budget | unknown_agent | invalid_request
	Detail string
}

func (d *Denied) Error() string { return d.Reason + ": " + d.Detail }

// AgentRule is one agent's limits.
type AgentRule struct {
	// Account is the one gateway account allowed to spend as this agent. The
	// agent id comes from the request body, so without this binding any key
	// could spend under the agent with the biggest budget.
	Account string `json:"account_id"`
	// Budget is the most this agent may spend in total, in Nanos. Hardcoded
	// from config for now; T4 makes it a windowed, persisted rule.
	Budget money.Nanos `json:"budget_nanos"`
}

// Config is the JSON shape.
type Config struct {
	Agents map[string]AgentRule `json:"agents"`
	Payees []string             `json:"payees"` // allowlist of 0x addresses
}

type Policy struct {
	cfg    Config
	payees map[string]bool
	mu     sync.Mutex
	spent  map[string]money.Nanos
}

func New(c Config) *Policy {
	p := &Policy{cfg: c, payees: map[string]bool{}, spent: map[string]money.Nanos{}}
	for _, a := range c.Payees {
		p.payees[strings.ToLower(a)] = true
	}
	return p
}

// Check screens a payment. On approval it counts the amount against the
// agent's budget immediately (so two concurrent payments cannot both fit a
// budget that holds one); Refund undoes that if the payment then fails.
func (p *Policy) Check(r Request) (Allowed, *Denied) {
	if r.AgentID == "" || r.Payee == "" || r.Amount <= 0 {
		return Allowed{}, &Denied{"invalid_request", "agent, payee and a positive amount are required"}
	}
	rule, ok := p.cfg.Agents[r.AgentID]
	if !ok || rule.Account == "" || rule.Account != r.AccountID {
		// Same answer for "no such agent" and "not yours": do not reveal which agents exist.
		return Allowed{}, &Denied{"unknown_agent", fmt.Sprintf("agent %q has no payment rule for this account", r.AgentID)}
	}
	if !p.payees[strings.ToLower(r.Payee)] {
		return Allowed{}, &Denied{"payee_not_allowed", fmt.Sprintf("payee %s is not on the allowlist", r.Payee)}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spent[r.AgentID]+r.Amount > rule.Budget {
		return Allowed{}, &Denied{"over_budget", fmt.Sprintf("%d + %d nanos exceeds budget %d", p.spent[r.AgentID], r.Amount, rule.Budget)}
	}
	p.spent[r.AgentID] += r.Amount
	return Allowed{req: r, ok: true}, nil
}

// Refund returns an approved amount to the agent's budget (payment failed).
func (p *Policy) Refund(a Allowed) {
	if !a.ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spent[a.req.AgentID] -= a.req.Amount
}
