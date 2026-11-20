package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// Rule ids the server adds when Cedar enforces. The Go engine never reports them.
const (
	// RuleCedarError is the deny when Cedar failed (or returned something that
	// is not a verdict) in enforce mode: the gateway fails closed.
	RuleCedarError = "cedar_error"
	// RuleCedarNoProof is the deny for a payment that Cedar allowed but the Go
	// engine did not: only policy.Evaluate can make the Allowed that the wallet
	// needs (I10), so there is nothing to sign with.
	RuleCedarNoProof = "cedar_no_proof"
)

// decided is the outcome of one policy question: the decision that counts
// (d), what the Go engine said (goD), and what Cedar said.
type decided struct {
	d, goD           policy.Decision
	cedarV, cedarR   string // "" when Cedar is off; cedarV "error" when it failed
	cedarOn, enforce bool
}

// decide is the one place a policy decision is made. It always runs the Go
// engine. With Cedar loaded it runs Cedar on the same request and snapshot and
// records both. Shadow mode (the default): Go decides, Cedar only observes.
// Enforce mode: Cedar decides, and a Cedar error or panic is a deny
// (RuleCedarError), never a fall back to Go. A disagreement is one Warn line
// (ids and rules only, no bodies, I9).
func (c *call) decide(req policy.Request, snap policy.Snapshot) decided {
	snap = c.withRegistry(req, snap)
	g := policy.Evaluate(req, snap)
	x := decided{d: g, goD: g, enforce: c.s.CedarEnforce}
	if c.s.Cedar == nil {
		if x.enforce { // misconfigured: enforce without an engine must not run on Go alone
			c.s.Log.Error("cedar enforce without an engine", "request_id", c.ev.RequestID, "agent_id", c.agent.ID)
			x.cedarV, x.cedarOn = "error", true
			x.d = policy.Decision{Verdict: policy.Deny, Rule: RuleCedarError, Reason: "policy engine unavailable"}
		}
		return x
	}
	x.cedarOn = true
	verdict, rule, err := c.runCedar(req, snap)
	if err == nil {
		switch policy.Verdict(verdict) {
		case policy.Allow, policy.Deny, policy.RequireApproval:
		default:
			err = fmt.Errorf("cedar: unknown verdict %q", verdict)
		}
	}
	if err != nil {
		x.cedarV = "error"
		c.s.Log.Error("cedar "+c.mode(), "request_id", c.ev.RequestID, "agent_id", c.agent.ID, "err", err)
		if x.enforce {
			x.d = policy.Decision{Verdict: policy.Deny, Rule: RuleCedarError, Reason: "policy engine error"}
		}
		return x
	}
	x.cedarV, x.cedarR = verdict, rule
	if verdict != string(g.Verdict) || rule != g.Rule {
		c.s.Log.Warn("cedar "+c.mode()+" mismatch", "request_id", c.ev.RequestID, "agent_id", c.agent.ID,
			"go_verdict", string(g.Verdict), "go_rule", g.Rule, "cedar_verdict", verdict, "cedar_rule", rule)
		if x.enforce {
			x.d = policy.Decision{Verdict: policy.Verdict(verdict), Rule: rule, Reason: "decided by Cedar rule " + rule}
		}
	}
	return x
}

func (c *call) mode() string {
	if c.s.CedarEnforce {
		return "enforce"
	}
	return "shadow"
}

// runCedar calls the engine and turns a panic into an error.
func (c *call) runCedar(req policy.Request, snap policy.Snapshot) (verdict, rule string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return c.s.Cedar.Decide(req, snap)
}

// withRegistry fills snap.Registry with the AI Registry state of the model or
// tool the request names, so both engines judge the same lookup. With the check
// off it does nothing (no query). A caller that already looked (tools/list asks
// once per server) sets Registry.On itself and is left alone. A failed lookup
// fails closed: the status "unavailable" is not approved.
//
// Payments are checked on req.Tool, the same name the tool scope matches: for
// a paid MCP tool that is "server/tool", the tool's registry name. A direct
// POST /v1/payments names its tool freely; with the check on that name must be
// an approved tool entry too, which keeps "no approved entry, no spend" simple.
func (c *call) withRegistry(req policy.Request, snap policy.Snapshot) policy.Snapshot {
	if !c.s.RegistryEnforce || snap.Registry.On {
		return snap
	}
	kind, name := "tool", req.Tool
	if req.Kind == policy.KindModel {
		kind, name = "model", req.Model
	}
	snap.Registry.On = true
	if name == "" || c.s.AIRegistry == nil {
		return snap // no name: the engines refuse it as out of scope or invalid before this matters
	}
	st, err := c.s.AIRegistry.Status(c.ctx, kind, name)
	if err != nil {
		c.s.Log.Error("registry lookup", "request_id", c.ev.RequestID, "agent_id", c.agent.ID, "err", err)
		st = "unavailable"
	}
	snap.Registry.Status = st
	return snap
}

// refuseInactive is rule 1 applied right after authentication, before the
// body is even read: a killed or revoked credential is refused on the next
// request whatever that request looks like (I12). It goes through decide like
// every other decision; in enforce mode Cedar's answer is the one used.
func (c *call) refuseInactive(kind policy.Kind) bool {
	a := c.agent
	if a.Status == policy.StatusActive {
		return false
	}
	x := c.decide(policy.Request{AgentID: a.ID, Kind: kind, Amount: 1},
		policy.Snapshot{AgentID: a.ID, Status: a.Status, Scope: a.Scope})
	c.logDecided(policy.Request{AgentID: a.ID, Kind: kind}, x, "")
	if x.d.Verdict != policy.Deny {
		return false // only Cedar in enforce mode can get here; the full evaluation that follows decides
	}
	c.denyResponse(x.d)
	return true
}

// logDecision writes the decision row (allow included), detached from the
// client so a disconnect cannot drop it. It records no Cedar verdict: it is for
// decisions that did not come from the engines (a lost budget race).
func (c *call) logDecision(req policy.Request, d policy.Decision, approvalID string) {
	c.logDecided(req, decided{d: d, goD: d}, approvalID)
}

// logDecided writes the row for x. verdict/rule are the decision that counted,
// cedar_verdict/cedar_rule are Cedar's. In enforce mode go_verdict/go_rule keep
// what the Go engine said, so `agentgate cedar shadow` still measures
// agreement; in shadow mode they stay empty (the Go engine is the decision).
func (c *call) logDecided(req policy.Request, x decided, approvalID string) {
	subject := req.Model
	if req.Kind != policy.KindModel {
		subject = req.Tool
	}
	ad := agents.Decision{RequestID: c.ev.RequestID, AgentID: c.agent.ID, AccountID: c.agent.AccountID,
		Kind: string(req.Kind), Subject: subject, Payee: req.Payee, TaskID: req.TaskID, Amount: req.Amount,
		Verdict: x.d.Verdict, Rule: x.d.Rule, Reason: x.d.Reason, ApprovalID: approvalID,
		CedarVerdict: x.cedarV, CedarRule: x.cedarR}
	if x.enforce {
		ad.GoVerdict, ad.GoRule = x.goD.Verdict, x.goD.Rule
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if err := c.s.Agents.LogDecision(ctx, ad); err != nil {
		c.s.Log.Error("decision log", "request_id", c.ev.RequestID, "err", err)
	}
}

// evaluate loads the snapshot, decides, and logs the decision.
// ok=false means a response was already written (internal error).
func (c *call) evaluate(req policy.Request, approvalID string) (policy.Decision, bool) {
	snap, err := c.s.Agents.Snapshot(c.ctx, c.agent.ID, req.TaskID)
	if err != nil {
		c.s.Log.Error("policy snapshot", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return policy.Decision{}, false
	}
	c.snap = snap
	x := c.decide(req, snap)
	c.logDecided(req, x, approvalID)
	return x.d, true
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
	if c.rpc { // a tool call: the answer is a JSON-RPC error carrying the approval id
		c.writeRPCError(http.StatusOK, "approval_required", "a human must approve this tool call; replay it with the same Idempotency-Key after approval",
			map[string]any{"approval_id": a.ID, "request_id": c.ev.RequestID, "amount": a.Amount.String(), "expires_at": a.ExpiresAt.UTC().Format(time.RFC3339)})
		c.finish(StatusApprovalRequired)
		return
	}
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
			if ids := s.Registry.AgentIDs(); len(ids) > 0 {
				dead, err := s.Agents.NotActive(ctx, ids)
				if err != nil {
					s.Log.Warn("kill watch", "err", err)
				}
				for _, id := range dead {
					if n := s.Registry.Cancel(id); n > 0 {
						s.Log.Info("kill switch", "agent", id, "cancelled", n)
					}
				}
			}
			// Kill by run (I12): same poll, same cancel, keyed by run id.
			if ids := s.RunRegistry.AgentIDs(); len(ids) > 0 {
				dead, err := s.Runs.NotActive(ctx, ids)
				if err != nil {
					s.Log.Warn("run kill watch", "err", err)
				}
				for _, id := range dead {
					if n := s.RunRegistry.Cancel(id); n > 0 {
						s.Log.Info("kill switch", "run", id, "cancelled", n)
					}
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
	f := func() { c.killed.Store(true); cancel() }
	rmAgent := c.s.Registry.Add(c.agent.ID, f)
	if c.runID == "" {
		return rmAgent
	}
	rmRun := c.s.RunRegistry.Add(c.runID, f)
	return func() { rmAgent(); rmRun() }
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
