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

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/provider"
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
type Payments struct {
	Policy  *policy.Policy
	Wallet  *wallet.Wallet
	Tools   map[string]string // tool name -> URL. Clients name tools; they never supply URLs (no SSRF).
	Fetcher x402.Fetcher
}

type payReq struct {
	Tool    string          `json:"tool"`
	AgentID string          `json:"agent_id"`
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

	acct, err := authenticate(c.ctx, s.Pool, c.r)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			c.fail(http.StatusUnauthorized, audit.StatusUnauthorized, "unauthorized", "missing or invalid API key", nil)
		} else {
			s.Log.Error("auth lookup", "err", err)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		}
		return
	}
	c.ev.AccountID = acct

	body, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, 1<<20))
	var in payReq
	if err != nil || json.Unmarshal(body, &in) != nil || in.Tool == "" || in.AgentID == "" {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "need JSON {tool, agent_id, input}", nil)
		return
	}
	c.ev.Model, c.ev.AgentID = in.Tool, in.AgentID
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

	// 2. Claim the idempotency key (same mechanism as model calls).
	idem := strings.TrimSpace(c.r.Header.Get("Idempotency-Key"))
	if idem == "" {
		idem = "gen-" + c.ev.RequestID
	}
	if len(idem) > 255 {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "Idempotency-Key too long", nil)
		return
	}
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

	// 3. Reserve (kind payment). Refusals below also leave ledger rows: hold, then release.
	hold, err := s.Ledger.Reserve(c.ctx, ledger.ReserveReq{AccountID: acct, Amount: amount, IdemKey: idem,
		Kind: ledger.KindPayment, TTL: s.HoldTTL, Memo: "payment " + in.Tool})
	if err != nil {
		_, _ = s.Pool.Exec(context.WithoutCancel(c.ctx), `DELETE FROM gateway_requests WHERE account_id=$1 AND idem_key=$2 AND request_id=$3`, acct, idem, c.ev.RequestID)
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
	allowed, denied := p.Policy.Check(policy.Request{AccountID: acct, AgentID: in.AgentID, Payee: req.PayTo, Amount: amount, Network: req.Network})
	if denied != nil {
		c.release(hold, "policy: "+denied.Reason)
		c.ev.Err = denied.Reason
		c.writeJSON(http.StatusForbidden, map[string]any{"error": map[string]any{"type": "policy_denied", "code": denied.Reason,
			"message": denied.Detail, "request_id": c.ev.RequestID}})
		c.finish(StatusPolicyDenied)
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
		p.Policy.Refund(allowed)
		c.release(hold, "prepare failed")
		s.Log.Error("prepare", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, StatusPaymentFailed, "sign_failed", "could not prepare the payment", nil)
		return
	}
	payload, err := p.Wallet.Sign(pending)
	if err != nil {
		p.Policy.Refund(allowed)
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
		p.Policy.Refund(allowed)
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
