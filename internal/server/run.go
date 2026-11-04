package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/run"
)

const (
	// HeaderRun names the run a request belongs to. It is created when absent
	// and always returned on the response, so a client can pass it on.
	HeaderRun = "X-AgentGate-Run"

	// Rule ids of the run gate. They live here, not in package policy: a run is
	// not part of the agent's scope, and Evaluate stays a pure function of the
	// agent's snapshot.
	RuleRunKilled    = "run_killed"
	RuleRunForbidden = "run_forbidden"
)

// beginRun attaches the request to its run and records its step. It runs right
// after authentication and rule 1, before anything is reserved. A killed run is
// refused here (I12: the next request after a kill). It returns false when a
// response was written.
//
// The step is written before the refusal so that a refused attempt is visible
// in the run, with its audit status.
func (c *call) beginRun(kind string) bool {
	id := c.r.Header.Get(HeaderRun)
	if id == "" {
		id = run.NewID()
	}
	r, err := c.s.Runs.Open(c.ctx, id, c.agent.ID, c.agent.AccountID)
	switch {
	case errors.Is(err, run.ErrBadID):
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", err.Error(), nil)
		return false
	case errors.Is(err, run.ErrForbidden):
		// Not the caller's run. Say nothing about whether it exists.
		d := policy.Decision{Verdict: policy.Deny, Rule: RuleRunForbidden, Reason: "run belongs to another agent"}
		c.logDecision(policy.Request{AgentID: c.agent.ID, Kind: policy.Kind(kind)}, d, "")
		c.denyResponse(d)
		return false
	case err != nil:
		c.s.Log.Error("run open", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return false
	}
	c.runID, c.ev.RunID = r.ID, r.ID
	c.w.Header().Set(HeaderRun, r.ID)
	c.span.SetAttributes(runAttr(r.ID))
	if err := c.s.Runs.Begin(c.ctx, r.ID, kind, c.ev.RequestID, c.start); err != nil {
		c.s.Log.Error("run step", "request_id", c.ev.RequestID, "err", err) // bookkeeping: never blocks the request
	}
	if r.Status == run.Killed {
		d := policy.Decision{Verdict: policy.Deny, Rule: RuleRunKilled, Reason: "run was killed"}
		c.logDecision(policy.Request{AgentID: c.agent.ID, Kind: policy.Kind(kind)}, d, "")
		c.denyResponse(d)
		return false
	}
	return true
}

// endStep closes the run step with the final audit status. It runs from
// finish, detached from the client, like the audit write.
func (c *call) endStep(status string) {
	if c.runID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if err := c.s.Runs.End(ctx, c.ev.RequestID, c.ev.Model, c.ext.HoldID, status, c.ev.Cost, time.Now()); err != nil {
		c.s.Log.Error("run step end", "request_id", c.ev.RequestID, "err", err)
	}
}

// holdKind is the ledger kind of what this request reserves.
func (c *call) holdKind() ledger.Kind {
	if c.rpc { // only tools/call reserves on the MCP route
		return ledger.KindTool
	}
	return ledger.KindModel
}

func runAttr(id string) attribute.KeyValue { return attribute.String("run.id", id) }

// killRule names why a call in flight was cancelled: its run was killed, or
// else its agent. Both cancel through the same path (I12); the rule id tells
// the caller which one to look at.
func (c *call) killRule() string {
	if c.runID != "" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 2*time.Second)
		defer cancel()
		if r, err := c.s.Runs.Get(ctx, c.runID); err == nil && r.Status == run.Killed {
			return RuleRunKilled
		}
	}
	return policy.RuleKilled
}
