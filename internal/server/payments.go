package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/run"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// Audit statuses written by the payments route.
const (
	StatusPolicyDenied      = "policy_denied"      // refused before signing (I10)
	StatusPaymentFailed     = "payment_failed"     // chain/server refused; hold released
	StatusSettlementPending = "settlement_pending" // money may have moved; hold left for a human/reconciler
	StatusUnknownTool       = "unknown_tool"
)

// Payments wires the payments route. Nil Server.Payments disables it.
//
// Why POST /v1/payments and not a tool-call path: the MCP tool gateway is T5.
// Until then a payment is its own verb with its own audit kind, and T5 can
// call the same function from tools/call.
//
// Who may pay whom is not here any more: the payee allowlist and the budgets
// are part of each agent's scope (internal/agents), checked by policy.Evaluate.
type Payments struct {
	Wallet  *wallet.Wallet
	Tools   map[string]string // tool name -> URL. Clients name tools; they never supply URLs (no SSRF).
	Fetcher x402.Fetcher
}

type payReq struct {
	Tool    string          `json:"tool"`
	AgentID string          `json:"agent_id"` // optional: the credential already names the agent
	Input   json.RawMessage `json:"input"`
}

// handlePayment: authenticate -> probe (402) -> reserve -> policy -> sign ->
// paid retry (the server settles on chain) -> settle ledger, or release.
func (s *Server) handlePayment(w http.ResponseWriter, r *http.Request) {
	reqID := uuid.NewString()
	ctx, span := s.Tracer.Start(r.Context(), "gateway.payment", trace.WithAttributes(attribute.String("request.id", reqID)))
	c := &call{s: s, w: w, r: r, f: provider.FormatOpenAI, ctx: ctx, span: span, start: time.Now(),
		ev: audit.Event{RequestID: reqID, Kind: "payment", Provider: "x402", At: time.Now()}}
	w.Header().Set("X-Request-Id", reqID)
	c.pay()
}

func (c *call) pay() {
	s, p := c.s, c.s.Payments

	agent, err := s.authenticate(c.ctx, c.r)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			c.fail(http.StatusUnauthorized, audit.StatusUnauthorized, "unauthorized", "missing or invalid API key", nil)
		} else {
			s.Log.Error("auth lookup", "err", err)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		}
		return
	}
	c.agent = agent
	acct := agent.AccountID
	c.ev.AccountID, c.ev.AgentID = acct, agent.ID
	if c.refuseInactive(policy.KindPayment) { // rule 1, before anything else (I12)
		return
	}
	if !c.beginRun(run.KindPayment) {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, 1<<20))
	var in payReq
	if err != nil || json.Unmarshal(body, &in) != nil || in.Tool == "" {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "need JSON {tool, input}", nil)
		return
	}
	// The credential is the identity. A body that names another agent is a
	// confused or malicious caller, not a way to spend under someone else's scope.
	if in.AgentID != "" && in.AgentID != agent.ID && in.AgentID != agent.Name {
		c.fail(http.StatusForbidden, StatusPolicyDenied, policy.RuleUnknownAgent, "this credential belongs to a different agent",
			map[string]any{"type": "policy_denied", "rule": policy.RuleUnknownAgent, "request_id": c.ev.RequestID})
		return
	}
	c.ev.Model = in.Tool
	url, ok := p.Tools[in.Tool]
	if !ok {
		c.fail(http.StatusNotFound, StatusUnknownTool, "unknown_tool", "no such paid tool", nil)
		return
	}
	input := []byte(in.Input)
	if len(input) == 0 {
		input = []byte("{}")
	}

	// 1. First request, unpaid: learn the price and payee from the 402.
	ch, free, err := p.Fetcher.Probe(c.ctx, http.MethodPost, url, input)
	if err != nil {
		s.Log.Error("probe", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusBadGateway, StatusPaymentFailed, "tool_unreachable", "paid tool did not answer", nil)
		return
	}
	if free != nil { // not paid: pass it through, nothing to charge
		defer free.Body.Close()
		c.w.Header().Set("Content-Type", "application/json")
		c.w.WriteHeader(free.StatusCode)
		_, _ = io.Copy(c.w, io.LimitReader(free.Body, 1<<20))
		c.finish(audit.StatusOK)
		return
	}
	req, err := ch.Pick()
	atomic, aok := wallet.Nanos(parseBig(req.Amount))
	if err != nil || !aok || atomic <= 0 {
		c.fail(http.StatusBadGateway, StatusPaymentFailed, "bad_payment_requirements", "paid tool sent unusable requirements", nil)
		return
	}
	amount := atomic
	c.ev.Cost = 0

	// 2. Idempotency key, task, and any approval already queued for this request.
	task, ok := c.taskID()
	if !ok {
		return
	}
	idem, ok := c.idemKey()
	if !ok {
		return
	}
	// The fingerprint pins what a human approves: the tool, the payee, the
	// amount and network the tool asked for, the task and the tool input.
	fp := approval.Fingerprint("payment", in.Tool, strings.ToLower(req.PayTo), itoa(int64(amount)), req.Network, task, bodyHash(input))
	approved, approvalID, stop := c.checkApproval(idem, fp)
	if stop {
		return
	}

	// Claim the key (same mechanism as model calls). An approved replay already
	// holds the claim and the hold from when it was queued.
	if !approved {
		tag, err := s.Pool.Exec(c.ctx, `INSERT INTO gateway_requests (account_id, idem_key, request_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
			acct, idem, c.ev.RequestID)
		if err != nil {
			s.Log.Error("claim", "err", err)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
			return
		}
		if tag.RowsAffected() == 0 {
			var orig string
			_ = s.Pool.QueryRow(c.ctx, `SELECT request_id FROM gateway_requests WHERE account_id=$1 AND idem_key=$2`, acct, idem).Scan(&orig)
			c.fail(http.StatusConflict, audit.StatusDuplicate, "duplicate_request", "this Idempotency-Key was already used",
				map[string]any{"original_request_id": orig})
			return
		}
	}

	// 3. Reserve (kind payment). Refusals below also leave ledger rows: hold, then release.
	// The hold id is needed before the policy decision: the Allowed is tied to it (I10).
	hold, err := s.Ledger.Reserve(c.ctx, ledger.ReserveReq{AccountID: acct, Amount: amount, IdemKey: idem,
		Kind: ledger.KindPayment, TTL: s.HoldTTL, Memo: "payment " + in.Tool})
	if err != nil {
		if !approved {
			_, _ = s.Pool.Exec(context.WithoutCancel(c.ctx), `DELETE FROM gateway_requests WHERE account_id=$1 AND idem_key=$2 AND request_id=$3`, acct, idem, c.ev.RequestID)
		}
		switch {
		case errors.Is(err, ledger.ErrInsufficientFunds):
			c.fail(http.StatusPaymentRequired, audit.StatusInsufficientFunds, "insufficient_funds", "insufficient funds", nil)
		case errors.Is(err, ledger.ErrAccountInactive):
			c.fail(http.StatusForbidden, audit.StatusAccountInactive, "account_inactive", "account inactive", nil)
		default:
			s.Log.Error("reserve", "err", err)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		}
		return
	}
	c.ext.HoldID = hold.ID

	// 4. Screen before sign (I10). A refusal never reaches the wallet.
	preq := policy.Request{AgentID: agent.ID, Kind: policy.KindPayment, Tool: in.Tool, Payee: req.PayTo, Network: req.Network,
		Amount: amount, TaskID: task, HoldID: hold.ID, Approved: approved}
	d, ok := c.evaluate(preq, approvalID)
	if !ok {
		c.release(hold, "policy unavailable")
		return
	}
	switch d.Verdict {
	case policy.Deny:
		c.release(hold, "policy: "+d.Rule)
		c.denyResponse(d)
		return
	case policy.RequireApproval:
		c.queueApproval(hold, preq, idem, fp, d) // the hold stays; nothing is signed
		return
	}
	allowed, _ := d.Allowed()
	if !c.charge(hold, preq) {
		return
	}

	// 5. Build the authorization, record it on the hold, THEN sign. After a
	// signature exists the chain, not this process, decides whether money
	// moved; the recorded ref is how the reconciler asks it later. If the
	// record cannot be written, nothing is signed.
	pending, err := p.Wallet.Prepare(c.ctx, allowed, hold.ID, ch.Required.Resource, req)
	if err == nil {
		actx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
		err = s.Ledger.AttachUpstream(actx, hold.ID, x402.Provider, pending.Ref())
		cancel()
	}
	if err != nil {
		c.release(hold, "prepare failed")
		s.Log.Error("prepare", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, StatusPaymentFailed, "sign_failed", "could not prepare the payment", nil)
		return
	}
	payload, err := p.Wallet.Sign(pending)
	if err != nil {
		c.release(hold, "sign failed")
		s.Log.Error("sign", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, StatusPaymentFailed, "sign_failed", "could not sign the payment", nil)
		return
	}

	// 6. Paid retry and everything after it run detached from the client: a
	// dropped connection must not abort a settlement that may already be on
	// chain. Bounded by the authorization window plus slack.
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(c.ctx), time.Duration(req.MaxTimeoutSeconds)*time.Second+30*time.Second)
	defer pcancel()

	resp, err := p.Fetcher.Pay(pctx, http.MethodPost, url, input, payload)
	if err != nil {
		c.unknown("transport_error", "")
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var sr x402.SettleResponse
	hasSettle := x402.DecodeHeader(resp.Header.Get(x402.HeaderResponse), &sr) == nil

	// Only a definite "no" releases the hold:
	//   - the tool answered 402 from verification (no signature was accepted, nothing was sent), or
	//   - the settlement reported a revert / rejected send (ReasonTxState).
	// Everything else (5xx, unparsable, unexpected_settle_error, pending) may
	// hide a transfer that happened, so the hold stays and the chain decides.
	definiteNo := (resp.StatusCode == http.StatusPaymentRequired && !hasSettle && resp.Header.Get(x402.HeaderRequired) != "") ||
		(hasSettle && !sr.Success && sr.ErrorReason == x402.ReasonTxState)

	switch {
	case resp.StatusCode == http.StatusOK && hasSettle && sr.Success:
		c.ev.Usage.UpstreamID = sr.Transaction
		st, err := s.Ledger.Settle(pctx, ledger.SettleReq{HoldID: hold.ID, Actual: amount,
			IdemKey: "settle:" + hold.ID, Memo: "payment " + in.Tool + " tx " + sr.Transaction})
		switch {
		case errors.Is(err, ledger.ErrHoldNotActive):
			// The reconciler got there first and read the same chain fact.
			c.ev.Cost, c.ev.CostSource, c.ev.Err = amount, "chain", "settled_by_reconciler"
		case err != nil:
			// Money moved on chain but the ledger write failed. The hold stays,
			// and the reconciler settles it from the chain.
			s.Log.Error("settle ledger after chain settle", "request_id", c.ev.RequestID, "tx", sr.Transaction, "err", err)
			c.unknown("ledger_settle_failed_after_chain", sr.Transaction)
			return
		default:
			c.closeCharge(hold.ID, st.Charged)
			c.ev.Cost, c.ev.CostSource = st.Charged, "chain"
		}
		var result json.RawMessage = out
		if !json.Valid(out) {
			result, _ = json.Marshal(string(out))
		}
		c.writeJSON(http.StatusOK, map[string]any{"request_id": c.ev.RequestID, "tool": in.Tool, "charged": c.ev.Cost.String(),
			"transaction": sr.Transaction, "network": sr.Network, "result": result})
		c.finish(audit.StatusOK)
	case definiteNo:
		c.release(hold, "payment failed")
		reason := sr.ErrorReason
		if reason == "" {
			reason = "payment_rejected"
		}
		c.ev.Err = reason
		c.writeJSON(http.StatusBadGateway, map[string]any{"error": map[string]any{"type": "payment_failed", "code": reason,
			"message": "payment was not settled; nothing was charged", "request_id": c.ev.RequestID}})
		c.finish(StatusPaymentFailed)
	default:
		reason := sr.ErrorReason
		if reason == "" {
			reason = "http_" + strconv.Itoa(resp.StatusCode)
		}
		c.unknown(reason, sr.Transaction)
	}
}

// unknown is the "signed, outcome not known" exit: the hold and the policy
// budget stay as they are, the client gets 202, and the reconciler decides
// from the chain once the authorization window has closed. Trade-off: a tool
// failure before settlement lands here too, so that money is held until the
// window closes (about a minute plus the reconciler's margin) instead of
// being returned at once.
func (c *call) unknown(reason, tx string) {
	c.ev.Err = reason
	if tx != "" {
		c.ev.Usage.UpstreamID = tx
	}
	c.writeJSON(http.StatusAccepted, map[string]any{"request_id": c.ev.RequestID, "status": "settlement_pending", "reason": reason,
		"message": "the payment may have settled; the ledger will resolve it from the chain"})
	c.finish(StatusSettlementPending)
}

func (c *call) writeJSON(status int, v any) {
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_ = json.NewEncoder(c.w).Encode(v)
}

func parseBig(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return big.NewInt(-1)
	}
	return n
}
