package server_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/paidtool"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/server"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// flakyChain lets a test fail or stall settlement after the wallet has signed.
type flakyChain struct {
	x402.Chain
	fail    atomic.Bool
	gate    atomic.Bool   // when set, Settle waits for release
	entered chan struct{} // signalled when a gated Settle starts
	release chan struct{}
}

func (f *flakyChain) Settle(ctx context.Context, tok common.Address, a x402.Auth, sig []byte) (common.Hash, error) {
	if f.fail.Load() {
		return common.Hash{}, x402.ErrReverted
	}
	if f.gate.Load() {
		f.entered <- struct{}{}
		<-f.release
	}
	// Settle on a context the client cannot cancel would hide the bug under test:
	// pass the request's own context through.
	return f.Chain.Settle(ctx, tok, a, sig)
}

// lossyTransport sits between the gateway and the paid tool.
type lossyTransport struct {
	mode atomic.Int32 // 0 normal, 1 send, then lose the response; 2 lose the request before it is sent
}

func (l *lossyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	paid := r.Header.Get(x402.HeaderSignature) != ""
	if paid && l.mode.Load() == 2 {
		return nil, errors.New("connection refused (injected)")
	}
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && paid && l.mode.Load() == 1 {
		resp.Body.Close()
		return nil, errors.New("connection reset (injected)")
	}
	return resp, err
}

// failSettleLedger fails Settle on demand: the chain moved, the ledger write did not.
type failSettleLedger struct {
	ledger.Ledger
	fail atomic.Bool
}

func (f *failSettleLedger) Settle(ctx context.Context, r ledger.SettleReq) (ledger.Settlement, error) {
	if f.fail.Load() {
		return ledger.Settlement{}, errors.New("injected ledger failure")
	}
	return f.Ledger.Settle(ctx, r)
}

type payEnv struct {
	*env
	sim     *x402.Sim
	w       *wallet.Wallet
	chain   *flakyChain
	net     *lossyTransport
	led     *failSettleLedger
	tool500 atomic.Bool // the tool answers 500 after it has settled
	payTo   common.Address
	key     *ecdsa.PrivateKey
}

// The credential names the agent; the body field is optional and, when present, must agree.
const agent = ""

// $0.30 budget: the $0.01 echo fits, the $0.50 premium never does.
func newPayEnv(t *testing.T) *payEnv { return newPayEnvFunded(t, 10*usd) }

func newPayEnvFunded(t *testing.T, deposit money.Nanos) *payEnv {
	t.Helper()
	sim, err := x402.NewSim()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sim.Close() })
	key, _ := crypto.GenerateKey()
	w, err := wallet.New(key, x402.LocalChainID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.Mint(context.Background(), w.Address(), big.NewInt(5_000_000)); err != nil {
		t.Fatal(err)
	}
	payTo := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	fc := &flakyChain{Chain: sim.Chain, entered: make(chan struct{}, 4), release: make(chan struct{})}
	ver := x402.NewVerifier(x402.Config{Network: x402.NetworkLocal, Token: sim.Token, TokenName: "USDC", TokenVersion: "2", PayTo: payTo, Chain: fc})
	pe := &payEnv{sim: sim, w: w, chain: fc, net: &lossyTransport{}, payTo: payTo, key: key}

	inner := paidtool.Handler(ver)
	tool := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if pe.tool500.Load() && r.Header.Get(x402.HeaderSignature) != "" {
			inner.ServeHTTP(httptest.NewRecorder(), r) // does the work, settles, then crashes
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(rw, r)
	}))
	t.Cleanup(tool.Close)

	pe.env = newEnv(t, deposit, func(s *server.Server) {
		pe.led = &failSettleLedger{Ledger: s.Ledger}
		s.Ledger = pe.led
		s.Payments = &server.Payments{
			Wallet:  w,
			Tools:   map[string]string{"echo": tool.URL + "/echo", "premium": tool.URL + "/premium"},
			Fetcher: x402.Fetcher{HTTP: &http.Client{Transport: pe.net}},
		}
	})
	// The agent behind the test key may pay the one payee, up to $0.30 a day.
	pe.setScope(func(sc *policy.Scope) { sc.Payees = []string{payTo.Hex()}; sc.DailyBudget = usd * 3 / 10 })
	return pe
}

func (p *payEnv) pay(tool, idem string) (int, map[string]any, string) {
	p.t.Helper()
	return p.payAs(p.secret, agent, tool, idem)
}

func (p *payEnv) payAs(secret, agentID, tool, idem string) (int, map[string]any, string) {
	p.t.Helper()
	named := ""
	if agentID != "" {
		named = `"agent_id":"` + agentID + `",`
	}
	resp, b := p.do("/v1/payments", `{"tool":"`+tool+`",`+named+`"input":{"text":"hello"}}`,
		map[string]string{"Idempotency-Key": idem, "Authorization": "Bearer " + secret})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m, resp.Header.Get("X-Request-Id")
}

func (p *payEnv) chainBal(who common.Address) int64 {
	b, err := p.sim.Chain.BalanceOf(context.Background(), p.sim.Token, who)
	if err != nil {
		p.t.Fatal(err)
	}
	return b.Int64()
}

func (p *payEnv) head() uint64 {
	h, err := p.sim.Chain.Client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		p.t.Fatal(err)
	}
	return h.Number.Uint64()
}

func (p *payEnv) entryKinds() []string {
	rows, err := p.pool.Query(context.Background(), `SELECT kind FROM ledger_entries WHERE account_id=$1 AND kind <> 'deposit' ORDER BY id`, p.acct)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var ks []string
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		ks = append(ks, k)
	}
	return ks
}

func (p *payEnv) auditRowID(id string) audit.Row {
	p.t.Helper()
	rows, err := p.audit.ByRequest(context.Background(), id)
	if err != nil || len(rows) != 1 {
		p.t.Fatalf("audit rows for %s: %d %v", id, len(rows), err)
	}
	return rows[0]
}

func (p *payEnv) activeHolds() int {
	var n int
	_ = p.pool.QueryRow(context.Background(), `SELECT count(*) FROM holds WHERE status='active'`).Scan(&n)
	return n
}

// reconcile runs the reconciler as it runs in serve: payment holds go to the
// chain. `now` decides which holds have expired; `chainNow` is the clock the
// authorization-window check uses.
func (p *payEnv) reconcile(now, chainNow time.Time) ledger.Report {
	p.t.Helper()
	lookup := &server.RouteLookup{
		Payments: &x402.AuthLookup{Chain: p.sim.Chain, Now: func() time.Time { return chainNow }},
		Default:  &provider.GenerationLookup{Providers: map[string]provider.ProviderCfg{}},
	}
	rep, err := server.Reconcile(context.Background(), p.env.led, lookup, p.audit, p.env.srv.Agents, now)
	if err != nil {
		p.t.Fatal(err)
	}
	return rep
}

func (p *payEnv) holdStatus() string {
	var s string
	_ = p.pool.QueryRow(context.Background(), `SELECT status FROM holds WHERE account_id=$1 ORDER BY created_at DESC LIMIT 1`, p.acct).Scan(&s)
	return s
}

func TestPaymentSettles(t *testing.T) {
	p := newPayEnv(t)
	code, m, rid := p.pay("echo", "pay-1")
	if code != 200 || m["transaction"] == "" {
		t.Fatalf("%d %v", code, m)
	}
	if !strings.Contains(string(mustJSON(m["result"])), "HELLO") {
		t.Fatalf("tool result missing: %v", m)
	}
	if p.chainBal(p.payTo) != 10_000 {
		t.Fatalf("payee has %d", p.chainBal(p.payTo))
	}
	if b := p.balance(); b.Balance != 10*usd-10_000_000 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	row := p.auditRowID(rid)
	if row.Kind != "payment" || row.Status != "ok" || row.Cost != 10_000_000 || row.Usage.UpstreamID != m["transaction"] {
		t.Fatalf("audit %+v", row)
	}
	if got := strings.Join(p.entryKinds(), ","); got != "hold,settle" {
		t.Fatalf("entries %s", got)
	}
	p.noActiveHolds()
}

// I10: an over-policy payment never reaches signing or the chain, and still
// leaves a ledger trail (hold, release) and an audit row.
func TestRefusedPaymentNeverSigns(t *testing.T) {
	p := newPayEnv(t)
	before := p.head()
	code, m, rid := p.pay("premium", "pay-refuse")
	if code != 429 { // budget_daily: not now, as opposed to a scope refusal (403)
		t.Fatalf("%d %v", code, m)
	}
	if p.w.SignCount() != 0 {
		t.Fatalf("signer ran %d times", p.w.SignCount())
	}
	if p.head() != before || p.chainBal(p.payTo) != 0 {
		t.Fatal("a transaction reached the chain")
	}
	if got := strings.Join(p.entryKinds(), ","); got != "hold,release" {
		t.Fatalf("ledger entries %s", got)
	}
	row := p.auditRowID(rid)
	if row.Status != server.StatusPolicyDenied || row.Err != "budget_daily" || row.Cost != 0 || row.HoldID == "" {
		t.Fatalf("audit %+v", row)
	}
	if b := p.balance(); b.Balance != 10*usd || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	p.noActiveHolds()
}

// The credential is the identity. Another account's key has its own scope (no
// payees), and a body that names another agent is refused.
func TestAgentBoundToCredential(t *testing.T) {
	p := newPayEnv(t)
	other, err := p.env.led.CreateAccount(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.env.led.Deposit(context.Background(), other, 5*usd, "dep", ""); err != nil {
		t.Fatal(err)
	}
	secretB, _, err := server.CreateKey(context.Background(), p.pool, other, "b")
	if err != nil {
		t.Fatal(err)
	}
	// B's key is a default-scope agent: no payees, so no payments.
	code, m, rid := p.payAs(secretB, agent, "echo", "b-1")
	if code != 403 || !strings.Contains(string(mustJSON(m)), "scope_payee") {
		t.Fatalf("%d %v", code, m)
	}
	if p.w.SignCount() != 0 || p.chainBal(p.payTo) != 0 {
		t.Fatal("B paid")
	}
	if row := p.auditRowID(rid); row.Status != server.StatusPolicyDenied || row.Err != "scope_payee" {
		t.Fatalf("audit %+v", row)
	}
	// Naming another agent in the body does not borrow its scope.
	code, m, rid = p.payAs(p.secret, "someone-else", "echo", "a-1")
	if code != 403 || !strings.Contains(string(mustJSON(m)), "unknown_agent") {
		t.Fatalf("%d %v", code, m)
	}
	if row := p.auditRowID(rid); row.Status != server.StatusPolicyDenied || row.Err != "unknown_agent" {
		t.Fatalf("audit %+v", row)
	}
	// The owner is unaffected.
	if code, _, _ := p.pay("echo", "a-1"); code != 200 {
		t.Fatalf("owner: %d", code)
	}
}

// I11 at the gateway: the same Idempotency-Key again is a 409; one transfer, one charge.
func TestPaymentReplayChargesOnce(t *testing.T) {
	p := newPayEnv(t)
	if code, _, _ := p.pay("echo", "pay-r"); code != 200 {
		t.Fatal(code)
	}
	signs := p.w.SignCount()
	code, m, _ := p.pay("echo", "pay-r")
	if code != 409 {
		t.Fatalf("%d %v", code, m)
	}
	if p.w.SignCount() != signs || p.chainBal(p.payTo) != 10_000 {
		t.Fatal("replay signed or paid again")
	}
	if b := p.balance(); b.Balance != 10*usd-10_000_000 {
		t.Fatalf("charged twice: %+v", b)
	}
	p.noActiveHolds()
}

// A reverted transaction is a definite no: released at once, audited, and a
// retry under a new key charges exactly once.
func TestChainRevertReleasesAtOnce(t *testing.T) {
	p := newPayEnv(t)
	p.chain.fail.Store(true)
	code, m, rid := p.pay("echo", "pay-f")
	if code != 502 {
		t.Fatalf("%d %v", code, m)
	}
	if p.w.SignCount() != 1 {
		t.Fatalf("signs %d", p.w.SignCount())
	}
	row := p.auditRowID(rid)
	if row.Status != server.StatusPaymentFailed || row.Err != x402.ReasonTxState || row.Cost != 0 {
		t.Fatalf("audit %+v", row)
	}
	if b := p.balance(); b.Balance != 10*usd || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	if got := strings.Join(p.entryKinds(), ","); got != "hold,release" {
		t.Fatalf("entries %s", got)
	}
	if code, _, _ := p.pay("echo", "pay-f"); code != 409 {
		t.Fatalf("same key: %d", code)
	}
	p.chain.fail.Store(false)
	if code, m, _ := p.pay("echo", "pay-f2"); code != 200 {
		t.Fatalf("retry: %d %v", code, m)
	}
	if b := p.balance(); b.Balance != 10*usd-10_000_000 || p.chainBal(p.payTo) != 10_000 {
		t.Fatalf("balance %+v payee %d", b, p.chainBal(p.payTo))
	}
	p.noActiveHolds()
}

// expectPendingThenReconcileSettles checks the shared tail of the "money moved,
// the gateway did not learn it" cases: 202, hold still active, no release, and
// after expiry the reconciler charges exactly what the chain moved.
func (p *payEnv) expectPendingThenReconcileSettles(code int, m map[string]any, rid, wantReason string) {
	p.t.Helper()
	if code != 202 || m["status"] != "settlement_pending" {
		p.t.Fatalf("%d %v", code, m)
	}
	row := p.auditRowID(rid)
	if row.Status != server.StatusSettlementPending || row.Err != wantReason {
		p.t.Fatalf("audit %+v", row)
	}
	if p.activeHolds() != 1 {
		p.t.Fatal("the hold must stay active while the outcome is unknown")
	}
	moved := p.chainBal(p.payTo)
	if moved != 10_000 {
		p.t.Fatalf("chain moved %d", moved)
	}
	// Not expired yet: nothing happens.
	if rep := p.reconcile(time.Now(), time.Now()); rep.Settled+rep.Released != 0 {
		p.t.Fatalf("closed an unexpired hold: %+v", rep)
	}
	rep := p.reconcile(time.Now().Add(10*time.Minute), time.Now())
	if rep.Settled != 1 || rep.Released != 0 || rep.Errors != 0 {
		p.t.Fatalf("report %+v", rep)
	}
	want, _ := x402.NanosFromAtomic(big.NewInt(moved))
	if b := p.balance(); b.Balance != 10*usd-want || b.Held != 0 {
		p.t.Fatalf("ledger charge != token movement: balance %+v, chain moved %d atomic", b, moved)
	}
	if got := strings.Join(p.entryKinds(), ","); got != "hold,settle" {
		p.t.Fatalf("entries %s", got)
	}
	p.noActiveHolds() // includes ledger.Audit
}

// The tool settled, then the response was lost on the way back.
func TestResponseLostAfterChainSettle(t *testing.T) {
	p := newPayEnv(t)
	p.net.mode.Store(1)
	code, m, rid := p.pay("echo", "lost-1")
	p.expectPendingThenReconcileSettles(code, m, rid, "transport_error")
}

// The tool settled, then crashed with a 500: not a refund.
func TestTool500AfterSettleIsNotRefunded(t *testing.T) {
	p := newPayEnv(t)
	p.tool500.Store(true)
	code, m, rid := p.pay("echo", "five-hundred")
	p.expectPendingThenReconcileSettles(code, m, rid, "http_500")
}

// The chain moved but the ledger write failed: the reconciler finishes it.
func TestLedgerSettleFailsAfterChainSuccess(t *testing.T) {
	p := newPayEnv(t)
	p.led.fail.Store(true)
	code, m, rid := p.pay("echo", "ledger-fail")
	p.expectPendingThenReconcileSettles(code, m, rid, "ledger_settle_failed_after_chain")
}

// Signed, never delivered: while the window is open the chain might still
// execute it, so the hold stays; once it closes, the reconciler releases.
func TestSignedButNeverSent(t *testing.T) {
	p := newPayEnv(t)
	p.net.mode.Store(2)
	code, m, _ := p.pay("echo", "never-sent")
	if code != 202 {
		t.Fatalf("%d %v", code, m)
	}
	expired := time.Now().Add(10 * time.Minute)
	rep := p.reconcile(expired, time.Now()) // hold expired, authorization window still open
	if rep.Errors != 1 || rep.Settled+rep.Released != 0 || p.activeHolds() != 1 {
		t.Fatalf("window open: %+v active=%d", rep, p.activeHolds())
	}
	rep = p.reconcile(expired, expired) // window closed: it can never execute
	if rep.Released != 1 || rep.Settled != 0 || p.activeHolds() != 0 {
		t.Fatalf("window closed: %+v", rep)
	}
	var st, kind string
	_ = p.pool.QueryRow(context.Background(), `SELECT status, kind FROM audit_events WHERE status='reconciled_unmetered' LIMIT 1`).Scan(&st, &kind)
	if st != "reconciled_unmetered" {
		t.Fatal("no reconciled_unmetered audit row")
	}
	if p.chainBal(p.payTo) != 0 {
		t.Fatal("money moved")
	}
	if b := p.balance(); b.Balance != 10*usd {
		t.Fatalf("balance %+v", b)
	}
	p.noActiveHolds()
}

// A client that hangs up during the paid retry must not abort settlement.
func TestClientDisconnectDuringSettlement(t *testing.T) {
	p := newPayEnv(t)
	p.chain.gate.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := p.doCtx(ctx, "/v1/payments", `{"tool":"echo","input":{}}`, map[string]string{"Idempotency-Key": "hangup"})
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-p.chain.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("settlement never started")
	}
	cancel() // the client drops while the chain call is in flight
	<-done
	time.Sleep(100 * time.Millisecond) // let the server notice the disconnect
	p.chain.release <- struct{}{}

	deadline := time.Now().Add(10 * time.Second)
	for p.holdStatus() != "settled" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if p.holdStatus() != "settled" {
		t.Fatalf("hold is %q: settlement was aborted by the disconnect", p.holdStatus())
	}
	if p.chainBal(p.payTo) != 10_000 {
		t.Fatalf("payee %d", p.chainBal(p.payTo))
	}
	if got := strings.Join(p.entryKinds(), ","); got != "hold,settle" {
		t.Fatalf("entries %s", got)
	}
	p.noActiveHolds()
}

func TestPaymentInsufficientFundsNeverSigns(t *testing.T) {
	p := newPayEnvFunded(t, 1_000) // less than the $0.01 price
	if code, _, _ := p.pay("echo", "pay-poor"); code != 402 {
		t.Fatalf("%d", code)
	}
	if p.w.SignCount() != 0 {
		t.Fatal("signed without funds")
	}
}

func TestPaymentUnknownToolAndAuth(t *testing.T) {
	p := newPayEnv(t)
	if code, _, _ := p.pay("nope", "x"); code != 404 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := p.payAs("bad", agent, "echo", "y"); code != 401 {
		t.Fatalf("%d", code)
	}
	if p.w.SignCount() != 0 {
		t.Fatal("signed")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// ---- T4: the policy engine in front of payments ----

// I10 through the engine: a payee that is not in the agent's scope is refused
// before signing, and the signer never runs.
func TestPayeeOutsideScopeNeverSigns(t *testing.T) {
	p := newPayEnv(t)
	p.setScope(func(s *policy.Scope) {
		s.Payees = []string{"0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"}
		s.DailyBudget = usd
	})
	before := p.head()
	code, m, rid := p.pay("echo", "scope-1")
	if code != 403 || !strings.Contains(string(mustJSON(m)), "scope_payee") {
		t.Fatalf("%d %v", code, m)
	}
	if p.w.SignCount() != 0 || p.head() != before {
		t.Fatal("a refused payment reached signing or the chain")
	}
	if row := p.auditRowID(rid); row.Status != server.StatusPolicyDenied || row.Err != "scope_payee" || row.HoldID == "" {
		t.Fatalf("audit %+v", row)
	}
	if d, _ := p.srv.Agents.Decisions(context.Background(), rid); len(d) != 1 || d[0].Rule != "scope_payee" || d[0].Payee == "" {
		t.Fatalf("decisions %+v", d)
	}
	p.noActiveHolds()
}

func TestPaymentToolOutsideScopeNeverSigns(t *testing.T) {
	p := newPayEnv(t)
	p.setScope(func(s *policy.Scope) { s.Tools = []string{"premium"}; s.Payees = []string{p.payTo.Hex()} })
	code, m, _ := p.pay("echo", "scope-2")
	if code != 403 || !strings.Contains(string(mustJSON(m)), "scope_tool") || p.w.SignCount() != 0 {
		t.Fatalf("%d %v signs=%d", code, m, p.w.SignCount())
	}
}

func TestPaymentBudgetFreedWhenPaymentSettlesAndFailures(t *testing.T) {
	p := newPayEnv(t)
	// $0.30 a day: the $0.01 echo fits many times, and each settles for exactly its price.
	for i := 0; i < 3; i++ {
		if code, m, _ := p.pay("echo", fmt.Sprint("b-", i)); code != 200 {
			t.Fatalf("%d %v", code, m)
		}
	}
	if s := p.snapshot(""); s.DailySpent != 30_000_000 {
		t.Fatalf("daily spent %d", s.DailySpent)
	}
	// A refused premium payment (over budget) leaves the window alone.
	if code, _, _ := p.pay("premium", "b-x"); code != 429 {
		t.Fatalf("%d", code)
	}
	if s := p.snapshot(""); s.DailySpent != 30_000_000 {
		t.Fatalf("daily spent %d after a refusal", s.DailySpent)
	}
}

// Over the threshold: queued, nothing signed, the money held. After approval
// the replay pays once; a replay of the replay is a duplicate; and approving
// "echo" never lets the same key pay for something else.
func TestPaymentApprovalFlow(t *testing.T) {
	p := newPayEnv(t)
	p.setScope(func(s *policy.Scope) {
		s.Payees = []string{p.payTo.Hex()}
		s.ApprovalThreshold = 5_000_000 // $0.005: the $0.01 echo needs a human
	})
	code, m, rid := p.pay("echo", "appr-1")
	if code != 202 || m["status"] != "approval_required" || m["approval_id"] == "" {
		t.Fatalf("%d %v", code, m)
	}
	id := m["approval_id"].(string)
	if p.w.SignCount() != 0 || p.chainBal(p.payTo) != 0 {
		t.Fatal("signed before approval")
	}
	if b := p.balance(); b.Held != 10_000_000 {
		t.Fatalf("held %d: the money must be locked while waiting", b.Held)
	}
	if row := p.auditRowID(rid); row.Status != server.StatusApprovalRequired || row.Kind != "payment" || row.HoldID == "" {
		t.Fatalf("audit %+v", row)
	}

	// The same key asking for another tool is not covered by this approval.
	if _, err := p.srv.Approvals.Allow(context.Background(), id, "alice"); err != nil {
		t.Fatal(err)
	}
	code, m, _ = p.pay("premium", "appr-1")
	if code != 422 || !strings.Contains(string(mustJSON(m)), "approval_mismatch") || p.w.SignCount() != 0 {
		t.Fatalf("%d %v signs=%d", code, m, p.w.SignCount())
	}

	code, m, _ = p.pay("echo", "appr-1")
	if code != 200 || m["transaction"] == "" {
		t.Fatalf("replay after approval: %d %v", code, m)
	}
	if p.w.SignCount() != 1 || p.chainBal(p.payTo) != 10_000 {
		t.Fatalf("signs=%d payee=%d", p.w.SignCount(), p.chainBal(p.payTo))
	}
	if code, _, _ = p.pay("echo", "appr-1"); code != 409 || p.w.SignCount() != 1 {
		t.Fatalf("second replay: %d signs=%d", code, p.w.SignCount())
	}
	if got := strings.Join(p.entryKinds(), ","); got != "hold,settle" {
		t.Fatalf("ledger entries %s", got)
	}
	if s := p.snapshot(""); s.DailySpent != 10_000_000 {
		t.Fatalf("daily spent %d", s.DailySpent)
	}
	p.noActiveHolds()
}

func TestPaymentApprovalDenyReleasesHold(t *testing.T) {
	p := newPayEnv(t)
	p.setScope(func(s *policy.Scope) { s.Payees = []string{p.payTo.Hex()}; s.ApprovalThreshold = 5_000_000 })
	_, m, _ := p.pay("echo", "appr-2")
	id := m["approval_id"].(string)
	if _, err := p.srv.Approvals.Deny(context.Background(), id, "bob"); err != nil {
		t.Fatal(err)
	}
	if b := p.balance(); b.Held != 0 || b.Balance != 10*usd {
		t.Fatalf("balance %+v", b)
	}
	if code, m, _ := p.pay("echo", "appr-2"); code != 403 || !strings.Contains(string(mustJSON(m)), "approval_denied") {
		t.Fatalf("%d %v", code, m)
	}
	if p.w.SignCount() != 0 {
		t.Fatal("signed a denied payment")
	}
	p.noActiveHolds()
}

func TestRevokedAgentCannotPay(t *testing.T) {
	p := newPayEnv(t)
	var id string
	_ = p.pool.QueryRow(context.Background(), `SELECT id FROM agents WHERE account_id=$1`, p.acct).Scan(&id)
	if _, err := p.srv.Agents.Revoke(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	code, m, _ := p.pay("echo", "rv-1")
	if code != 403 || !strings.Contains(string(mustJSON(m)), "agent_revoked") || p.w.SignCount() != 0 {
		t.Fatalf("%d %v", code, m)
	}
}
