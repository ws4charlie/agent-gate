package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
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
	allowed, denied := p.Policy.Check(policy.Request{AgentID: in.AgentID, Payee: req.PayTo, Amount: amount, Network: req.Network})
	if denied != nil {
		c.release(hold, "policy: "+denied.Reason)
		c.ev.Err = denied.Reason
		c.writeJSON(http.StatusForbidden, map[string]any{"error": map[string]any{"type": "policy_denied", "code": denied.Reason,
			"message": denied.Detail, "request_id": c.ev.RequestID}})
		c.finish(StatusPolicyDenied)
		return
	}

	// 5. Sign. Only now does the key get used.
	payload, err := p.Wallet.Pay(c.ctx, allowed, hold.ID, ch.Required.Resource, req)
	if err != nil {
		p.Policy.Refund(allowed)
		c.release(hold, "sign failed")
		s.Log.Error("sign", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, StatusPaymentFailed, "sign_failed", "could not sign the payment", nil)
		return
	}

	// 6. Paid retry. The tool's server verifies, runs the tool, settles on chain.
	resp, err := p.Fetcher.Pay(c.ctx, http.MethodPost, url, input, payload)
	if err != nil {
		// The request may have reached the server: do not release. The hold stays
		// active; funds are only returned by the reconciler after expiry, flagged.
		s.Log.Error("paid retry", "request_id", c.ev.RequestID, "err", err)
		c.ev.Err = "settlement_unknown"
		c.writeJSON(http.StatusBadGateway, map[string]any{"error": map[string]any{"type": "settlement_unknown", "code": "settlement_unknown",
			"message": "the payment may or may not have settled", "request_id": c.ev.RequestID}})
		c.finish(StatusSettlementPending)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var sr x402.SettleResponse
	_ = x402.DecodeHeader(resp.Header.Get(x402.HeaderResponse), &sr)

	switch {
	case resp.StatusCode == http.StatusOK && sr.Success:
		c.attach(hold, "x402", sr.Transaction)
		c.ev.Usage.UpstreamID = sr.Transaction
		st, err := s.Ledger.Settle(context.WithoutCancel(c.ctx), ledger.SettleReq{HoldID: hold.ID, Actual: amount,
			IdemKey: "settle:" + hold.ID, Memo: "payment " + in.Tool + " tx " + sr.Transaction})
		if err != nil {
			// Money moved on chain but the ledger write failed: the loudest failure there is.
			s.Log.Error("settle ledger after chain settle", "request_id", c.ev.RequestID, "tx", sr.Transaction, "err", err)
			c.ev.Err = "ledger_settle_failed_after_chain"
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
			return
		}
		c.ev.Cost, c.ev.CostSource = st.Charged, "chain"
		var result json.RawMessage = out
		if !json.Valid(out) {
			result, _ = json.Marshal(string(out))
		}
		c.writeJSON(http.StatusOK, map[string]any{"request_id": c.ev.RequestID, "tool": in.Tool, "charged": st.Charged.String(),
			"transaction": sr.Transaction, "network": sr.Network, "result": result})
		c.finish(audit.StatusOK)
	case sr.ErrorReason == x402.ReasonPending || resp.StatusCode == http.StatusAccepted:
		c.attach(hold, "x402", sr.Transaction)
		c.ev.Usage.UpstreamID, c.ev.Err = sr.Transaction, "settlement_pending"
		c.writeJSON(http.StatusAccepted, map[string]any{"request_id": c.ev.RequestID, "status": "settlement_pending", "transaction": sr.Transaction})
		c.finish(StatusSettlementPending)
	default:
		// The chain or the server refused: nothing moved. Release the hold,
		// return the budget, and say so in the audit log.
		p.Policy.Refund(allowed)
		c.release(hold, "payment failed")
		reason := sr.ErrorReason
		if reason == "" {
			reason = "http_" + http.StatusText(resp.StatusCode)
		}
		c.ev.Err = reason
		c.writeJSON(http.StatusBadGateway, map[string]any{"error": map[string]any{"type": "payment_failed", "code": reason,
			"message": "payment was not settled; nothing was charged", "request_id": c.ev.RequestID}})
		c.finish(StatusPaymentFailed)
	}
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
