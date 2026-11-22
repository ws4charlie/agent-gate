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

	// A key that was used before: the same request gets the stored answer, a
	// different one is refused. Nothing is sent to the bank twice.
	prior, found := bank.Payment{}, false
	switch p, err := b.Store.ByIdem(c.ctx, acct, idem); {
	case err == nil:
		prior, found = p, true
	case !errors.Is(err, pgx.ErrNoRows):
		s.Log.Error("bank lookup", "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return
	}
	if found {
		switch {
		case prior.Fingerprint != fp:
			c.fail(http.StatusUnprocessableEntity, audit.StatusIdempotencyConflict, "idempotency_conflict", "idempotency_key was used for a different payment",
				map[string]any{"payment_id": prior.ID})
			return
		case prior.State == bank.Refused:
			c.fail(http.StatusForbidden, StatusPolicyDenied, prior.Rule, "this payment was refused",
				map[string]any{"type": "policy_denied", "rule": prior.Rule, "payment_id": prior.ID, "request_id": c.ev.RequestID})
			return
		case prior.State != bank.AwaitingGateApproval:
			c.writeJSON(http.StatusOK, bankView(prior, true))
			c.finish(audit.StatusDuplicate)
			return
		}
	}
	approved, approvalID, stop := c.checkApproval(idem, fp)
	if stop {
		return
	}

	hold, err := s.Ledger.Reserve(c.ctx, ledger.ReserveReq{AccountID: acct, Amount: amount, IdemKey: "bank:" + idem,
		Kind: ledger.KindPayment, TTL: b.HoldLease(), Memo: "bank payment " + in.RecipientID})
	switch {
	case errors.Is(err, ledger.ErrInsufficientFunds):
		c.fail(http.StatusPaymentRequired, audit.StatusInsufficientFunds, "insufficient_funds", "insufficient funds", nil)
		return
	case errors.Is(err, ledger.ErrAccountInactive):
		c.fail(http.StatusForbidden, audit.StatusAccountInactive, "account_inactive", "account inactive", nil)
		return
	case err != nil:
		s.Log.Error("reserve", "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return
	case hold.Status != "active":
		c.fail(http.StatusConflict, audit.StatusDuplicate, "duplicate_request", "this idempotency_key was already used", nil)
		return
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
		return
	}
	base := bank.Payment{AccountID: acct, AgentID: agent.ID, IdemKey: idem, Fingerprint: fp, RecipientID: in.RecipientID,
		Amount: amount, Memo: in.Memo, TaskID: task, HoldID: hold.ID}
	facts := map[string]any{"request_id": c.ev.RequestID, "amount_usd": amount.String(), "recipient_id": in.RecipientID,
		"verdict": string(d.Verdict), "rule": d.Rule, "approval_id": approvalID}
	actor := "agent:" + agent.ID
	switch d.Verdict {
	case policy.Deny:
		c.release(hold, "policy: "+d.Rule)
		c.bankRefuse(base, prior, found, d.Rule, actor, facts)
		c.denyResponse(d)
		return
	case policy.RequireApproval:
		if !found {
			p := base
			p.State, p.Rule = bank.AwaitingGateApproval, d.Rule
			if _, _, err := b.Store.Create(c.ctx, p, actor, facts); err != nil {
				s.Log.Error("bank create", "err", err)
				c.release(hold, "bank payment not recorded")
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
		c.bankRefuse(base, prior, found, nd.Rule, actor, facts)
		c.denyResponse(nd)
		return
	}
	if !c.charge(hold, preq) { // a lost budget race: the response is written, the payment is refused
		c.bankRefuse(base, prior, found, c.ev.Err, actor, facts)
		return
	}

	// Allowed. From here the payment is real: the row exists before anything
	// goes to the bank, so a crash leaves a payment the poller will submit.
	dctx := context.WithoutCancel(c.ctx)
	var p bank.Payment
	if found { // it waited for a Gate human: record who
		by := ""
		if a, err := s.Approvals.Store.Get(dctx, approvalID); err == nil {
			by = a.DecidedBy
		}
		facts["approved_by"] = by
		p, err = b.Move(dctx, prior, bank.AwaitingBankApproval, "gate-approver:"+by, facts, bank.Change{})
	} else {
		p = base
		p.State, p.Rule = bank.AwaitingBankApproval, d.Rule
		var created bool
		if p, created, err = b.Store.Create(dctx, p, actor, facts); err == nil && !created {
			c.release(hold, "duplicate payment") // lost a race with the same key: the other request owns it
			c.writeJSON(http.StatusOK, bankView(p, true))
			c.finish(audit.StatusDuplicate)
			return
		}
	}
	if err != nil {
		s.Log.Error("bank record", "request_id", c.ev.RequestID, "err", err)
		c.release(hold, "bank payment not recorded")
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return
	}
	c.ext.HoldID = hold.ID
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

// bankRefuse records a refused payment: a new row in refused, or the row that
// was waiting for a Gate human moved to refused.
func (c *call) bankRefuse(base, prior bank.Payment, found bool, rule, actor string, facts map[string]any) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	b := c.s.Bank
	var err error
	if found {
		_, err = b.Move(dctx, prior, bank.Refused, actor, facts, bank.Change{Rule: rule})
	} else {
		base.State, base.Rule = bank.Refused, rule
		_, _, err = b.Store.Create(dctx, base, actor, facts)
	}
	if err != nil {
		c.s.Log.Error("bank refuse record", "request_id", c.ev.RequestID, "err", err)
	}
}
