package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

// Audit statuses written by the policy gate.
const (
	StatusApprovalRequired = "approval_required" // queued for a human; the hold stays
	StatusApprovalDenied   = "approval_denied"
	StatusApprovalExpired  = "approval_expired"
	StatusApprovalMismatch = "approval_mismatch"
	StatusKilled           = "killed" // the kill switch cancelled the call in flight (I12)
)

const (
	// HeaderTask names the task a request belongs to, for the per-task budget.
	HeaderTask      = "X-AgentGate-Task"
	maxTaskIDLength = 128
	killPoll        = 250 * time.Millisecond // I12 asks for 1s; polling at a quarter of it
)

// ---- shared helpers for the model and payment flows ----

// taskID reads X-AgentGate-Task. Empty means the shared default task.
func (c *call) taskID() (string, bool) {
	t := strings.TrimSpace(c.r.Header.Get(HeaderTask))
	if len(t) > maxTaskIDLength {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", HeaderTask+" too long", nil)
		return "", false
	}
	return t, true
}

// idemKey returns the client's Idempotency-Key, or a generated one.
func (c *call) idemKey() (string, bool) {
	idem := strings.TrimSpace(c.r.Header.Get("Idempotency-Key"))
	if len(idem) > 255 {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "Idempotency-Key too long", nil)
		return "", false
	}
	if idem == "" {
		idem = "gen-" + c.ev.RequestID
	}
	return idem, true
}

func bodyHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// refuseInactive is rule 1 applied right after authentication, before the
// body is even read: a killed or revoked credential is refused on the next
// request whatever that request looks like (I12).
func (c *call) refuseInactive(kind policy.Kind) bool {
	a := c.agent
	if a.Status == policy.StatusActive {
		return false
	}
	d := policy.Evaluate(policy.Request{AgentID: a.ID, Kind: kind, Amount: 1},
		policy.Snapshot{AgentID: a.ID, Status: a.Status, Scope: a.Scope})
	c.logDecision(policy.Request{AgentID: a.ID, Kind: kind}, d, "")
	c.denyResponse(d)
	return true
}

// logDecision writes the decision row (allow included), detached from the
// client so a disconnect cannot drop it.
func (c *call) logDecision(req policy.Request, d policy.Decision, approvalID string) {
	subject := req.Model
	if req.Kind != policy.KindModel {
		subject = req.Tool
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	err := c.s.Agents.LogDecision(ctx, agents.Decision{RequestID: c.ev.RequestID, AgentID: c.agent.ID, AccountID: c.agent.AccountID,
		Kind: string(req.Kind), Subject: subject, Payee: req.Payee, TaskID: req.TaskID, Amount: req.Amount,
		Verdict: d.Verdict, Rule: d.Rule, Reason: d.Reason, ApprovalID: approvalID})
	if err != nil {
		c.s.Log.Error("decision log", "request_id", c.ev.RequestID, "err", err)
	}
}

// evaluate loads the snapshot, runs the pure engine, and logs the decision.
// ok=false means a response was already written (internal error).
func (c *call) evaluate(req policy.Request, approvalID string) (policy.Decision, bool) {
	snap, err := c.s.Agents.Snapshot(c.ctx, c.agent.ID, req.TaskID)
	if err != nil {
		c.s.Log.Error("policy snapshot", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return policy.Decision{}, false
	}
	c.snap = snap
	d := policy.Evaluate(req, snap)
	c.logDecision(req, d, approvalID)
	return d, true
}

// denyResponse refuses the request with the rule id. Budget and velocity are
// "not now" (429); everything else is "no" (403).
func (c *call) denyResponse(d policy.Decision) {
	status := http.StatusForbidden
	switch d.Rule {
	case policy.RuleBudgetDaily, policy.RuleBudgetTask, policy.RuleVelocity:
		status = http.StatusTooManyRequests
	case policy.RuleInvalid:
		status = http.StatusBadRequest
	}
	c.span.AddEvent("policy " + d.Rule)
	c.fail(status, StatusPolicyDenied, d.Rule, d.Reason,
		map[string]any{"type": "policy_denied", "rule": d.Rule, "request_id": c.ev.RequestID})
}

// charge counts an admitted request against the agent's windows, after its
// hold exists. It is the enforcing step: the snapshot the engine read may be
// stale, the conditional statements in Charge are not. A refusal here is a lost
// race; the hold is released and the request is refused with the rule that
// ran out.
func (c *call) charge(h ledger.Hold, req policy.Request) bool {
	rule, err := c.s.Agents.Charge(c.ctx, agents.ChargeReq{HoldID: h.ID, AgentID: c.agent.ID, TaskID: req.TaskID,
		Amount: req.Amount, Scope: c.snap.Scope})
	switch {
	case err != nil:
		c.s.Log.Error("charge", "request_id", c.ev.RequestID, "err", err)
		c.release(h, "charge failed")
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return false
	case rule != "":
		d := policy.Decision{Verdict: policy.Deny, Rule: rule, Reason: "limit reached while this request was being admitted"}
		c.logDecision(req, d, "")
		c.release(h, "policy: "+rule)
		c.denyResponse(d)
		return false
	}
	return true
}

// closeCharge corrects the budget windows to what the hold really cost.
func (c *call) closeCharge(holdID string, actual money.Nanos) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if err := c.s.Agents.CloseCharge(ctx, holdID, actual); err != nil {
		c.s.Log.Error("close charge", "request_id", c.ev.RequestID, "hold", holdID, "err", err) // reconciler closes it
	}
}

// ---- approvals ----

// checkApproval looks for a queued approval for this (agent, Idempotency-Key).
//
//	none                 -> proceed as a fresh request
//	different fingerprint-> 422: an approval covers one exact request
//	pending              -> 202 again with the same approval id
//	denied / expired     -> refused
//	consumed             -> 409, a replay is one execution
//	approved             -> spent now (once); the request proceeds with Approved set
//
// stop=true means a response was written.
func (c *call) checkApproval(idem, fp string) (approved bool, approvalID string, stop bool) {
	s := c.s
	a, found, err := s.Approvals.Find(c.ctx, c.agent.ID, idem)
	if err != nil {
		s.Log.Error("approval lookup", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return false, "", true
	}
	if !found {
		return false, "", false
	}
	c.ext.HoldID = a.HoldID
	extra := map[string]any{"approval_id": a.ID, "request_id": c.ev.RequestID}
	if a.Fingerprint != fp {
		// Same key, different model, payee, amount, task or body: the approval
		// does not carry over.
		c.fail(http.StatusUnprocessableEntity, StatusApprovalMismatch, "approval_mismatch",
			"this Idempotency-Key was approved for a different request", extra)
		return false, a.ID, true
	}
	// Past its time? Make the stored state say so (and free the hold) first.
	if (a.Status == approval.Pending || a.Status == approval.Approved) && !time.Now().Before(a.ExpiresAt) {
		_, _ = s.Approvals.Sweep(c.ctx)
		a.Status = approval.Expired
	}
	switch a.Status {
	case approval.Pending:
		c.respondApprovalRequired(a)
		return false, a.ID, true
	case approval.Denied:
		c.fail(http.StatusForbidden, StatusApprovalDenied, "approval_denied", "a human denied this request", extra)
		return false, a.ID, true
	case approval.Expired:
		c.fail(http.StatusGone, StatusApprovalExpired, "approval_expired", "the approval expired; send a new request", extra)
		return false, a.ID, true
	case approval.Consumed:
		extra["original_request_id"] = a.RequestID
		c.fail(http.StatusConflict, audit.StatusDuplicate, "duplicate_request", "this approved request already ran", extra)
		return false, a.ID, true
	}
	ok, err := s.Approvals.Consume(c.ctx, a.ID)
	if err != nil {
		s.Log.Error("approval consume", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return false, a.ID, true
	}
	if !ok { // lost to a parallel replay or to expiry
		extra["original_request_id"] = a.RequestID
		c.fail(http.StatusConflict, audit.StatusDuplicate, "duplicate_request", "this approved request already ran or expired", extra)
		return false, a.ID, true
	}
	return true, a.ID, false
}

// queueApproval parks a request that needs a human. Its hold exists already
// and stays, so the money is locked while it waits.
func (c *call) queueApproval(h ledger.Hold, req policy.Request, idem, fp string, d policy.Decision) {
	subject := req.Model
	if req.Kind != policy.KindModel {
		subject = req.Tool
	}
	// The hold must outlive the wait, or the reconciler would release it
	// (I6) while a human is still deciding. Extend never shortens a lease.
	err := c.s.Ledger.Extend(c.ctx, h.ID, c.s.HoldTTL+c.s.ApprovalTTL)
	var a approval.Approval
	if err == nil {
		a, _, err = c.s.Approvals.Create(c.ctx, approval.Approval{AgentID: c.agent.ID, AccountID: c.agent.AccountID, IdemKey: idem,
			RequestID: c.ev.RequestID, Kind: string(req.Kind), Subject: subject, Payee: req.Payee, Amount: req.Amount, TaskID: req.TaskID,
			Fingerprint: fp, HoldID: h.ID}, c.s.ApprovalTTL)
	}
	if err != nil {
		c.s.Log.Error("approval create", "request_id", c.ev.RequestID, "err", err)
		c.release(h, "approval queue failed")
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return
	}
	c.ev.Err = d.Rule
	c.respondApprovalRequired(a)
}

func (c *call) respondApprovalRequired(a approval.Approval) {
	c.ev.Err = policy.RuleApprovalThreshold
	c.writeJSON(http.StatusAccepted, map[string]any{"status": "approval_required", "approval_id": a.ID, "request_id": c.ev.RequestID,
		"amount": a.Amount.String(), "expires_at": a.ExpiresAt.UTC().Format(time.RFC3339),
		"message": "a human must approve this request; replay it with the same Idempotency-Key after approval"})
	c.finish(StatusApprovalRequired)
}

// ---- kill switch (I12) ----

// WatchKills cancels the in-flight calls of any agent that is no longer
// active. The CLI kills by writing the database (it is another process), so
// the gateway polls; the poll only runs a query while something is in flight.
// Returns when ctx ends.
func (s *Server) WatchKills(ctx context.Context) {
	s.defaults()
	t := time.NewTicker(killPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ids := s.Registry.AgentIDs()
			if len(ids) == 0 {
				continue
			}
			dead, err := s.Agents.NotActive(ctx, ids)
			if err != nil {
				s.Log.Warn("kill watch", "err", err)
				continue
			}
			for _, id := range dead {
				if n := s.Registry.Cancel(id); n > 0 {
					s.Log.Info("kill switch", "agent", id, "cancelled", n)
				}
			}
		}
	}
}

// Kill is the in-process kill: record it and cancel at once, without waiting
// for the next poll.
func (s *Server) Kill(ctx context.Context, ref string) error {
	s.defaults()
	a, err := s.Agents.Kill(ctx, ref)
	if err != nil {
		return err
	}
	s.Registry.Cancel(a.ID)
	return nil
}

// trackInFlight registers the upstream call of an agent so the kill switch can
// cancel it. Cancelling the upstream context, not the client's, lets the
// handler go on to settle what was actually received (I8, I12).
func (c *call) trackInFlight(cancel context.CancelFunc) (remove func()) {
	return c.s.Registry.Add(c.agent.ID, func() { c.killed.Store(true); cancel() })
}

var errKilled = errors.New("agent killed")

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// releaseApproved frees the hold of an approved replay that the engine then
// refused (a budget that ran out while it waited): the approval is spent and
// the hold made when it was queued must not linger.
func (c *call) releaseApproved(approved bool) {
	if approved && c.ext.HoldID != "" {
		c.release(ledger.Hold{ID: c.ext.HoldID}, "refused after approval")
	}
}
