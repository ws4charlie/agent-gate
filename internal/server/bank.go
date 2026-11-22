package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/bank"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/run"
)

// Bank rail names. The tool name is what the agent's tool scope and the AI
// Registry see for every bank payment; the network is what the payee rule sees.
const (
	BankTool    = "mercury/ach"
	BankNetwork = "ach"
)

// Audit status of a request that filed a payment at the bank.
const StatusBankSubmitted = "bank_submitted"

type bankReq struct {
	RecipientID string      `json:"recipient_id"`
	AmountUSD   json.Number `json:"amount_usd"`
	IdemKey     string      `json:"idempotency_key"`
	Memo        string      `json:"memo"`
}

func bankView(p bank.Payment, replay bool) map[string]any {
	v := map[string]any{"payment_id": p.ID, "state": string(p.State), "recipient_id": p.RecipientID, "amount_usd": p.Amount.String()}
	if p.RequestID != "" {
		v["mercury_request_id"] = p.RequestID
	}
	if p.Rule != "" {
		v["rule"] = p.Rule
	}
	if replay {
		v["idempotent_replay"] = true
	}
	return v
}

// handleBankPayment: POST /v1/bank/payments. Only routed when Server.Bank is set.
func (s *Server) handleBankPayment(w http.ResponseWriter, r *http.Request) {
	reqID := uuid.NewString()
	ctx, span := s.Tracer.Start(r.Context(), "gateway.bank_payment", trace.WithAttributes(attribute.String("request.id", reqID)))
	c := &call{s: s, w: w, r: r, f: provider.FormatOpenAI, ctx: ctx, span: span, start: time.Now(),
		ev: audit.Event{RequestID: reqID, Kind: "payment", Provider: "mercury", Model: BankTool, At: time.Now()}}
	w.Header().Set("X-Request-Id", reqID)
	c.bankPay()
}

// handleBankGet: GET /v1/bank/payments/{id}, for the agent that owns the payment's account.
func (s *Server) handleBankGet(w http.ResponseWriter, r *http.Request) {
	agent, err := s.authenticate(r.Context(), r)
	if err != nil {
		code, msg := authFailure(err)
		if !errors.Is(err, errUnauthorized) {
			code, msg = "internal_error", "internal error"
		}
		writeErr(w, provider.FormatOpenAI, http.StatusUnauthorized, code, msg, nil)
		return
	}
	p, err := s.Bank.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil || p.AccountID != agent.AccountID { // not yours reads as not found
		writeErr(w, provider.FormatOpenAI, http.StatusNotFound, "not_found", "no such payment", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(bankView(p, false))
}

func (c *call) bankPay() {
	s, b := c.s, c.s.Bank
	agent, err := s.authenticate(c.ctx, c.r)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			code, msg := authFailure(err)
			c.fail(http.StatusUnauthorized, audit.StatusUnauthorized, code, msg, nil)
		} else {
			s.Log.Error("auth lookup", "err", err)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		}
		return
	}
	c.agent = agent
	s.noteKeyExpiry(c.w, agent)
	acct := agent.AccountID
	c.ev.AccountID, c.ev.AgentID = acct, agent.ID
	if c.refuseInactive(policy.KindPayment) {
		return
	}
	if !c.beginRun(run.KindPayment) {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, 1<<16))
	var in bankReq
	if err != nil || json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.RecipientID) == "" || len(in.RecipientID) > 128 || len(in.Memo) > 200 {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "need JSON {recipient_id, amount_usd, idempotency_key, memo}", nil)
		return
	}
	amount, err := money.FromUSD(in.AmountUSD.String())
	if err != nil || amount <= 0 || int64(amount)%10_000_000 != 0 { // the bank takes whole cents
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "amount_usd must be a positive amount in whole cents", nil)
		return
	}
	idem := strings.TrimSpace(in.IdemKey)
	if idem == "" {
		idem = strings.TrimSpace(c.r.Header.Get("Idempotency-Key"))
	}
	if idem == "" || len(idem) > 255 {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "idempotency_key is required (at most 255 characters)", nil)
		return
	}
	task, ok := c.taskID()
	if !ok {
		return
	}
	in.RecipientID = strings.TrimSpace(in.RecipientID)
	fp := approval.Fingerprint("bank", BankNetwork, strings.ToLower(in.RecipientID), itoa(int64(amount)), task, in.Memo)

	// Claim the key first. The payment row is the claim: it exists before any
	// money is held, so a second request with the same key finds it and never
	// reserves, charges or releases anything. Only the request that created the
	// row (the owner) touches the hold.
	actor := "agent:" + agent.ID
	row, created := bank.Payment{}, false
	base := bank.Payment{AccountID: acct, AgentID: agent.ID, IdemKey: idem, Fingerprint: fp, RecipientID: in.RecipientID,
		Amount: amount, Memo: in.Memo, TaskID: task, State: bank.Received, Env: b.Env}
	if p, err := b.Store.ByIdem(c.ctx, acct, idem); err == nil {
		row = p
	} else if errors.Is(err, pgx.ErrNoRows) {
		row, created, err = b.Store.Create(c.ctx, base, actor, map[string]any{"request_id": c.ev.RequestID,
			"amount_usd": amount.String(), "recipient_id": in.RecipientID})
		if err != nil {
			s.Log.Error("bank claim", "err", err)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
			return
		}
	} else {
		s.Log.Error("bank lookup", "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return
	}
	if !created { // a key that was used before: the stored answer, never a second payment
		switch {
		case row.Fingerprint != fp:
			c.fail(http.StatusUnprocessableEntity, audit.StatusIdempotencyConflict, "idempotency_conflict", "idempotency_key was used for a different payment",
				map[string]any{"payment_id": row.ID})
			return
		case row.State == bank.Refused:
			c.fail(refusedStatus(row.Rule), StatusPolicyDenied, row.Rule, "this payment was refused",
				map[string]any{"type": "policy_denied", "rule": row.Rule, "payment_id": row.ID, "request_id": c.ev.RequestID})
			return
		case row.State == bank.Received:
			c.fail(http.StatusConflict, audit.StatusDuplicate, "request_in_flight", "a request with this idempotency_key is being decided; retry shortly",
				map[string]any{"payment_id": row.ID})
			return
		case row.State != bank.AwaitingGateApproval:
			c.writeJSON(http.StatusOK, bankView(row, true))
			c.finish(audit.StatusDuplicate)
			return
		}
	}
	dctx := context.WithoutCancel(c.ctx)
	end := func(to bank.State, rule string, facts map[string]any) {
		if np, err := b.Move(dctx, row, to, actor, facts, bank.Change{Rule: rule}); err != nil {
			s.Log.Error("bank end", "request_id", c.ev.RequestID, "payment", row.ID, "to", string(to), "err", err)
		} else {
			row = np
		}
	}

	if !created && row.State == bank.AwaitingGateApproval {
		// A retry of a payment that waits for a Gate human. Record it first: if the
		// process dies after the approval is consumed and before the payment moves,
		// the poller sees how recent the attempt was and frees the hold if it is old.
		if np, err := b.Store.Note(dctx, row.ID, actor, map[string]any{"retry": true, "request_id": c.ev.RequestID}, bank.Change{}); err == nil {
			row = np
		}
	}
	approved, approvalID, stop := c.checkApproval(idem, fp)
	if stop {
		if created { // cannot happen for a fresh key; never leave a claim behind
			end(bank.Failed, "approval_lookup", nil)
		}
		return
	}

	// The hold and its owner are recorded in one transaction. A payment that
	// waited for a Gate human already has its hold; reuse it, never reserve twice.
	var hold ledger.Hold
	if row.HoldID == "" {
		// Never assign the payment from a failed Hold: the claimed row must
		// stay the one the refusal below is stored on.
		h, held, herr := b.Hold(dctx, row)
		if herr == nil {
			hold, row = h, held
		}
		switch {
		case errors.Is(herr, ledger.ErrInsufficientFunds):
			end(bank.Refused, "insufficient_funds", nil)
			c.fail(http.StatusPaymentRequired, audit.StatusInsufficientFunds, "insufficient_funds", "insufficient funds", nil)
			return
		case errors.Is(herr, ledger.ErrAccountInactive):
			end(bank.Refused, "account_inactive", nil)
			c.fail(http.StatusForbidden, audit.StatusAccountInactive, "account_inactive", "account inactive", nil)
			return
		case herr != nil:
			s.Log.Error("bank hold", "err", herr)
			end(bank.Failed, "internal_error", nil)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
			return
		case hold.Status != "active":
			end(bank.Failed, "hold_not_active", nil)
			c.fail(http.StatusConflict, audit.StatusDuplicate, "duplicate_request", "this idempotency_key was already used", nil)
			return
		}
	} else {
		st, herr := b.Store.HoldStatus(dctx, row.HoldID)
		if herr != nil || st != "active" {
			end(bank.Failed, "hold_not_active", nil)
			c.fail(http.StatusConflict, audit.StatusDuplicate, "duplicate_request", "the hold of this payment is no longer active", nil)
			return
		}
		hold = ledger.Hold{ID: row.HoldID, AccountID: acct, Amount: amount, Status: st}
	}
	c.ext.HoldID = hold.ID

	preq := policy.Request{AgentID: agent.ID, Kind: policy.KindPayment, Tool: BankTool, Payee: in.RecipientID, Network: BankNetwork,
		Amount: amount, TaskID: task, HoldID: hold.ID, Approved: approved}
	d, ok := c.evaluate(preq, approvalID)
	c.notePayment(BankNetwork, int64(amount))
	if ok {
		c.noteDecision(d)
	}
	if !ok {
		c.release(hold, "policy unavailable")
		end(bank.Failed, "policy_unavailable", nil)
		return
	}
	facts := map[string]any{"request_id": c.ev.RequestID, "amount_usd": amount.String(), "recipient_id": in.RecipientID,
		"verdict": string(d.Verdict), "rule": d.Rule, "approval_id": approvalID}
	switch d.Verdict {
	case policy.Deny:
		c.release(hold, "policy: "+d.Rule)
		end(bank.Refused, d.Rule, facts)
		c.denyResponse(d)
		return
	case policy.RequireApproval:
		if row.State == bank.Received {
			if np, err := b.Move(dctx, row, bank.AwaitingGateApproval, actor, facts, bank.Change{Rule: d.Rule}); err == nil {
				row = np
			} else {
				s.Log.Error("bank queue", "err", err)
				c.release(hold, "bank payment not recorded")
				end(bank.Failed, "internal_error", nil)
				c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
				return
			}
		}
		c.queueApproval(hold, preq, idem, fp, d)
		return
	}
	if _, proof := d.Allowed(); !proof {
		nd := policy.Decision{Verdict: policy.Deny, Rule: RuleCedarNoProof, Reason: "no Go approval to sign with"}
		c.logDecision(preq, nd, approvalID)
		c.release(hold, "policy: "+nd.Rule)
		end(bank.Refused, nd.Rule, facts)
		c.denyResponse(nd)
		return
	}
	if !c.charge(hold, preq) { // a lost budget race: the response is written, the payment is refused
		end(bank.Refused, c.ev.Err, facts)
		return
	}

	// Allowed. From here the payment is real: the row is moved before anything
	// goes to the bank, so a crash leaves a payment the poller will submit.
	who := actor
	if row.State == bank.AwaitingGateApproval { // it waited for a Gate human: record who
		by := ""
		if a, err := s.Approvals.Store.Get(dctx, approvalID); err == nil {
			by = a.DecidedBy
		}
		facts["approved_by"] = by
		who = "gate-approver:" + by
	}
	p, err := b.Move(dctx, row, bank.AwaitingBankApproval, who, facts, bank.Change{Rule: d.Rule})
	if err != nil {
		s.Log.Error("bank record", "request_id", c.ev.RequestID, "err", err)
		c.release(hold, "bank payment not recorded")
		end(bank.Failed, "internal_error", nil)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return
	}
	sp, serr := b.Submit(dctx, p)
	switch {
	case errors.Is(serr, bank.ErrRejected):
		c.fail(http.StatusBadGateway, StatusPaymentFailed, "bank_refused", "the bank refused the payment request",
			map[string]any{"payment_id": p.ID, "state": string(bank.Failed)})
		return
	case serr != nil:
		// The answer was lost, not the request: the poller sends it again with the same key.
		s.Log.Warn("bank submit", "request_id", c.ev.RequestID, "payment", p.ID, "err", serr)
	}
	v := bankView(sp, false)
	if serr != nil {
		v["note"] = "the bank did not answer; Gate will retry the submission"
	}
	c.writeJSON(http.StatusAccepted, v)
	c.finish(StatusBankSubmitted)
}

// refusedStatus is the HTTP status a refused payment answers with, as the
// policy gate does it: budget and velocity are "not now", the rest is "no".
func refusedStatus(rule string) int {
	switch rule {
	case policy.RuleBudgetDaily, policy.RuleBudgetTask, policy.RuleVelocity:
		return http.StatusTooManyRequests
	case "insufficient_funds":
		return http.StatusPaymentRequired
	}
	return http.StatusForbidden
}
