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
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/run"
	"github.com/ws4charlie/agent-gate/internal/vaultsign"
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

// Payments wires the wallet into the gateway. Nil Server.Payments turns off
// both POST /v1/payments and the MCP 402 path.
//
// Two doors, one path: POST /v1/payments pays a named paid tool, and an MCP
// tools/call that gets HTTP 402 is paid the same way (payChallenge). Who may
// pay whom is not decided here: the payee allowlist, budgets and approval
// threshold are part of each agent's scope (internal/agents), checked by
// policy.Evaluate. Tools maps names to URLs for the first door only: clients
// name tools, they never supply URLs (no SSRF).
//
// Wallet is the signer: the local key (*wallet.Wallet, the default) or
// QuorumVault (*wallet.VaultSigner, AG_SIGNER=vault). Either way only a
// policy.Allowed gets an authorization prepared (I10).
type Payments struct {
	Wallet  wallet.Signer
	Tools   map[string]string // tool name -> URL, for POST /v1/payments; "GET <url>" for a GET resource
	Fetcher x402.Fetcher
	// PolicyVersion names the policy Gate decides under; it goes into the
	// Vault credential. Empty: DefaultPolicyVersion.
	PolicyVersion string
}

// DefaultPolicyVersion is the policy version when nothing more specific is known.
const DefaultPolicyVersion = "go1"

// Vault answers, as the agent sees them.
const (
	CodeVaultRefused     = "vault_refused"
	CodeVaultUnavailable = "vault_unavailable"
)

// toolTarget splits a Tools entry into method and URL. A plain URL is called
// with POST and the agent's input as the body; "GET <url>" is a fixed resource,
// as most x402 data sellers publish.
func toolTarget(v string) (method, url string) {
	if u, ok := strings.CutPrefix(v, "GET "); ok {
		return http.MethodGet, strings.TrimSpace(u)
	}
	return http.MethodPost, v
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
	target, ok := p.Tools[in.Tool]
	if !ok {
		c.fail(http.StatusNotFound, StatusUnknownTool, "unknown_tool", "no such paid tool", nil)
		return
	}
	method, url := toolTarget(target)
	input := []byte(in.Input)
	if len(input) == 0 {
		input = []byte("{}")
	}
	send := input
	if method == http.MethodGet { // a GET resource is the URL itself; input is not sent
		send = nil
	}

	// 1. First request, unpaid: learn the price and payee from the 402.
	ch, free, err := p.Fetcher.Probe(c.ctx, method, url, send)
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
	req, err := ch.Pick(p.Wallet.Network())
	atomic, aok := wallet.Nanos(parseBig(req.Amount))
	if err != nil || !aok || atomic <= 0 {
		c.fail(http.StatusBadGateway, StatusPaymentFailed, "bad_payment_requirements", "paid tool sent unusable requirements", nil)
		return
	}
	c.payChallenge(payTarget{
		Tool: in.Tool, PolicyTool: in.Tool, Input: input, Ch: ch, Req: req, Amount: atomic,
		Send: func(ctx context.Context, pl x402.PaymentPayload) (*http.Response, error) {
			return p.Fetcher.Pay(ctx, method, url, send, pl)
		},
		OK: func(out []byte, _ string, sr x402.SettleResponse, charged money.Nanos) {
			var result json.RawMessage = out
			if !json.Valid(out) {
				result, _ = json.Marshal(string(out))
			}
			c.writeJSON(http.StatusOK, map[string]any{"request_id": c.ev.RequestID, "tool": in.Tool, "charged": charged.String(),
				"transaction": sr.Transaction, "network": sr.Network, "result": result})
			c.finish(audit.StatusOK)
		},
	})
}

// payTarget is what differs between paying for a tool through POST
// /v1/payments and paying for an MCP tool that answered 402: where the paid
// retry goes and how a success is shown. Everything between (idempotency,
// approval, hold, policy, wallet, settlement) is one code path (I10, I11).
type payTarget struct {
	Tool       string // audit name
	PolicyTool string // the name the agent's tool scope matches
	Input      []byte // part of the approval fingerprint
	Ch         *x402.Challenge
	Req        x402.Requirements
	Amount     money.Nanos
	Send       func(ctx context.Context, p x402.PaymentPayload) (*http.Response, error)
	// OK writes the success response. ctype is the paid response's Content-Type.
	OK func(out []byte, ctype string, sr x402.SettleResponse, charged money.Nanos)
}

// payChallenge: idempotency -> approval -> reserve -> policy -> prepare+record
// -> sign -> paid retry -> settle ledger. It always ends the request.
func (c *call) payChallenge(t payTarget) {
	s, p := c.s, c.s.Payments
	in := struct{ Tool string }{t.Tool}
	agent, acct := c.agent, c.agent.AccountID
	req, amount, ch, input := t.Req, t.Amount, t.Ch, t.Input
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
		Kind: ledger.KindPayment, TTL: x402.HoldTTL(t.Req), Memo: "payment " + in.Tool})
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
	preq := policy.Request{AgentID: agent.ID, Kind: policy.KindPayment, Tool: t.PolicyTool, Payee: req.PayTo, Network: req.Network,
		Amount: amount, TaskID: task, HoldID: hold.ID, Approved: approved}
	d, ok := c.evaluate(preq, approvalID)
	c.notePayment(req.Network, int64(amount))
	if ok {
		c.noteDecision(d)
	}
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
	allowed, proof := d.Allowed()
	if !proof {
		// Cedar enforcing allowed what the Go engine did not: no Allowed exists
		// to sign with (I10), so the payment is refused.
		nd := policy.Decision{Verdict: policy.Deny, Rule: RuleCedarNoProof, Reason: "no Go approval to sign with"}
		c.logDecision(preq, nd, approvalID)
		c.release(hold, "policy: "+nd.Rule)
		c.denyResponse(nd)
		return
	}
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
		if errors.Is(err, wallet.ErrWindowTooShort) { // refused before Vault is asked
			c.fail(http.StatusBadGateway, StatusPaymentFailed, "bad_payment_requirements", err.Error(), nil)
			return
		}
		s.Log.Error("prepare", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, StatusPaymentFailed, "sign_failed", "could not prepare the payment", nil)
		return
	}
	dec, err := c.vaultDecision(p, approved, approvalID)
	if err != nil {
		c.release(hold, "decision for vault failed")
		s.Log.Error("vault decision", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, StatusPaymentFailed, "sign_failed", "could not sign the payment", nil)
		return
	}
	payload, at, err := p.Wallet.SignFor(c.ctx, pending, dec)
	if at != nil { // one Vault attempt per request: its fields go on the request's own audit row
		c.ext.Vault = &audit.Vault{CredentialID: at.CredentialID, WalletID: at.WalletID,
			Payee: at.Payee, Amount: at.Amount, Code: at.Code, Field: at.Field, Outcome: at.Outcome}
	}
	if err != nil {
		c.signFailed(hold, at, err)
		return
	}

	// 6. Paid retry and everything after it run detached from the client: a
	// dropped connection must not abort a settlement that may already be on
	// chain. Bounded by the authorization window plus slack.
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(c.ctx), time.Duration(req.MaxTimeoutSeconds)*time.Second+30*time.Second)
	defer pcancel()

	resp, err := t.Send(pctx, payload)
	if err != nil {
		c.unknown("transport_error", "")
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, maxMCPBody))
	var sr x402.SettleResponse
	hasSettle := x402.DecodeHeader(resp.Header.Get(x402.HeaderResponse), &sr) == nil

	// A signature exists, so from here only the chain can say "no" (I11).
	// The tool's own word that nothing moved (a 402, a revert report, a 5xx) is
	// NOT enough to release: the tool still holds a signed authorization and
	// could submit it until validBefore. So the only exit that charges is a
	// success the tool reported AND that the ledger could record; every other
	// exit keeps the hold active, answers 202, and leaves the release to
	// x402.AuthLookup, which asks the chain once the window has closed.
	if resp.StatusCode != http.StatusOK || !hasSettle || !sr.Success {
		reason := sr.ErrorReason
		if reason == "" {
			reason = "http_" + strconv.Itoa(resp.StatusCode)
		}
		c.unknown(reason, sr.Transaction)
		return
	}
	c.ev.Usage.UpstreamID = sr.Transaction
	st, err := s.Ledger.Settle(pctx, ledger.SettleReq{HoldID: hold.ID, Actual: amount,
		IdemKey: "settle:" + hold.ID, Memo: "payment " + t.Tool + " tx " + sr.Transaction})
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
	t.OK(out, resp.Header.Get("Content-Type"), sr, c.ev.Cost)
}

// signFailed ends a payment whose signer produced no usable signature.
//
// Vault refused, or never decided: nothing signed is in anyone's hands, so
// the hold is released and the agent's next try is a new hold, a new nonce and
// a new credential (Vault burned this nonce). A 200 whose signature does not
// verify is different: Vault may have signed something, so the nonce counts as
// possibly spent and the hold stays until the reconciler has asked the chain
// after validBefore (the authorization ref is already on the hold).
func (c *call) signFailed(hold ledger.Hold, at *wallet.Attempt, err error) {
	s := c.s
	if at == nil { // the local key
		c.release(hold, "sign failed")
		s.Log.Error("sign", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, StatusPaymentFailed, "sign_failed", "could not sign the payment", nil)
		return
	}
	logAt := s.Log.Warn
	if at.Config {
		logAt = s.Log.Error // the operator must fix Gate's setup, not this payment
	}
	switch at.Outcome {
	case wallet.OutcomeBadSignature:
		s.Log.Error("vault signature does not verify; hold kept until the authorization window closes",
			"request_id", c.ev.RequestID, "hold_id", hold.ID, "credential_id", at.CredentialID)
		c.unknown("vault_bad_signature", "")
	case wallet.OutcomeRefused:
		c.release(hold, "vault: "+at.Code)
		logAt("vault refused", "request_id", c.ev.RequestID, "credential_id", at.CredentialID, "code", at.Code, "field", at.Field, "config", at.Config)
		msg := "QuorumVault refused to sign: " + at.Code
		if at.Code == vaultsign.CodeFastLaneCap {
			msg = "over the Vault fast-lane limit: QuorumVault refused to sign"
		}
		c.fail(http.StatusForbidden, StatusPaymentFailed, CodeVaultRefused, msg,
			map[string]any{"type": CodeVaultRefused, "code": at.Code, "field": at.Field, "credential_id": at.CredentialID, "request_id": c.ev.RequestID})
	case wallet.OutcomeUnavailable:
		// No decision reached Gate: retries ran out, the deadline passed, the
		// client left, or TLS/token failed. Vault may still have signed, but
		// the signature never left Vault to Gate or to the tool, so no money
		// can move with it: release. Vault's fast-lane counters may still
		// count this attempt that Gate abandoned.
		c.release(hold, "vault unavailable")
		logAt("vault unavailable", "request_id", c.ev.RequestID, "credential_id", at.CredentialID, "config", at.Config, "err", err)
		c.fail(http.StatusBadGateway, StatusPaymentFailed, CodeVaultUnavailable, "QuorumVault did not answer; nothing was signed",
			map[string]any{"type": CodeVaultUnavailable, "credential_id": at.CredentialID, "request_id": c.ev.RequestID})
	default:
		c.release(hold, "sign failed")
		s.Log.Error("vault sign", "request_id", c.ev.RequestID, "credential_id", at.CredentialID, "err", err)
		c.fail(http.StatusInternalServerError, StatusPaymentFailed, "sign_failed", "could not sign the payment", nil)
	}
}

// vaultDecision is what the Vault credential records about the decision.
// Only the Vault signer uses it, so the default path makes no extra query.
func (c *call) vaultDecision(p *Payments, approved bool, approvalID string) (wallet.Decision, error) {
	d := wallet.Decision{PolicyVersion: p.PolicyVersion, Reason: "allow"}
	if d.PolicyVersion == "" {
		d.PolicyVersion = DefaultPolicyVersion
	}
	if _, vault := p.Wallet.(*wallet.VaultSigner); !vault || !approved {
		return d, nil
	}
	if c.s.Approvals == nil || approvalID == "" {
		return d, errors.New("approved payment without an approval to name")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	a, err := c.s.Approvals.Get(ctx, approvalID)
	if err != nil {
		return d, err
	}
	d.Reason = approvedBy(a.DecidedBy)
	return d, nil
}

// approvedBy is the credential's decision_reason for an approved payment.
// The approver's name is whatever the CLI was given, so it is made safe for
// the credential (valid UTF-8, no control characters, at most 256 bytes):
// a name Mint would refuse must not fail a payment whose approval is spent.
func approvedBy(name string) string {
	const prefix, limit = "approved_by:", 256
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.ToValidUTF8(name, "_"))
	name = strings.TrimSpace(name)
	if name == "" {
		name = "unknown"
	}
	return cutUTF8(prefix+name, limit)
}

// cutUTF8 shortens s (valid UTF-8) to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// unknown is the "signed, outcome not known" exit: the hold and the policy
// budget stay as they are, the client gets 202, and the reconciler decides
// from the chain once the authorization window has closed. Trade-off: every
// failure after signing lands here, so that money is held until the window
// closes (about a minute plus the reconciler's margin) instead of being
// returned at once. That is the price of not trusting the tool (I11).
func (c *call) unknown(reason, tx string) {
	c.ev.Err = reason
	if tx != "" {
		c.ev.Usage.UpstreamID = tx
	}
	if c.rpc {
		c.writeRPCError(http.StatusOK, "settlement_pending", "the payment may have settled; the ledger will resolve it from the chain",
			map[string]any{"request_id": c.ev.RequestID, "reason": reason})
		c.finish(StatusSettlementPending)
		return
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
