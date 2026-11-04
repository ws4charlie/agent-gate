package server_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/paidtool"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/server"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// flakyChain fails settlement on demand, after the wallet has already signed.
type flakyChain struct {
	x402.Chain
	fail atomic.Bool
}

func (f *flakyChain) Settle(ctx context.Context, tok common.Address, a x402.Auth, sig []byte) (common.Hash, error) {
	if f.fail.Load() {
		return common.Hash{}, x402.ErrReverted
	}
	return f.Chain.Settle(ctx, tok, a, sig)
}

type payEnv struct {
	*env
	sim   *x402.Sim
	w     *wallet.Wallet
	chain *flakyChain
	payTo common.Address
	key   *ecdsa.PrivateKey
}

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
	fc := &flakyChain{Chain: sim.Chain}
	ver := x402.NewVerifier(x402.Config{Network: x402.NetworkLocal, Token: sim.Token, TokenName: "USDC", TokenVersion: "2", PayTo: payTo, Chain: fc})
	tool := httptest.NewServer(paidtool.Handler(ver))
	t.Cleanup(tool.Close)

	pe := &payEnv{sim: sim, w: w, chain: fc, payTo: payTo, key: key}
	pe.env = newEnv(t, deposit, func(s *server.Server) {
		s.Payments = &server.Payments{
			Policy: policy.New(policy.Config{Agents: map[string]policy.AgentRule{"agent-1": {Budget: usd * 3 / 10}}, Payees: []string{payTo.Hex()}}),
			Wallet: w,
			Tools:  map[string]string{"echo": tool.URL + "/echo", "premium": tool.URL + "/premium"},
		}
	})
	return pe
}

func (p *payEnv) pay(tool, idem string) (int, map[string]any, string) {
	p.t.Helper()
	resp, b := p.do("/v1/payments", `{"tool":"`+tool+`","agent_id":"agent-1","input":{"text":"hello"}}`, map[string]string{"Idempotency-Key": idem})
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
	if code != 403 {
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
	if row.Status != server.StatusPolicyDenied || row.Err != "over_budget" || row.Cost != 0 || row.HoldID == "" {
		t.Fatalf("audit %+v", row)
	}
	if b := p.balance(); b.Balance != 10*usd || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	p.noActiveHolds()
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

// The chain refuses after the wallet signed: hold released, audited, and a
// retry charges exactly once.
func TestChainFailureAfterSigningReleases(t *testing.T) {
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
	if row.Status != server.StatusPaymentFailed || row.Err == "" || row.Cost != 0 {
		t.Fatalf("audit %+v", row)
	}
	if b := p.balance(); b.Balance != 10*usd || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	if got := strings.Join(p.entryKinds(), ","); got != "hold,release" {
		t.Fatalf("entries %s", got)
	}
	// Same key: the first attempt consumed it; no second attempt, no double charge.
	if code, _, _ := p.pay("echo", "pay-f"); code != 409 {
		t.Fatalf("same key: %d", code)
	}
	// New key after recovery: one charge, and the budget the failure refunded is available.
	p.chain.fail.Store(false)
	if code, m, _ := p.pay("echo", "pay-f2"); code != 200 {
		t.Fatalf("retry: %d %v", code, m)
	}
	if b := p.balance(); b.Balance != 10*usd-10_000_000 || p.chainBal(p.payTo) != 10_000 {
		t.Fatalf("balance %+v payee %d", b, p.chainBal(p.payTo))
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
	resp, _ := p.do("/v1/payments", `{"tool":"echo","agent_id":"agent-1"}`, map[string]string{"Authorization": "Bearer bad"})
	if resp.StatusCode != 401 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if p.w.SignCount() != 0 {
		t.Fatal("signed")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func (p *payEnv) auditRowID(id string) audit.Row {
	p.t.Helper()
	rows, err := p.audit.ByRequest(context.Background(), id)
	if err != nil || len(rows) != 1 {
		p.t.Fatalf("audit rows for %s: %d %v", id, len(rows), err)
	}
	return rows[0]
}
