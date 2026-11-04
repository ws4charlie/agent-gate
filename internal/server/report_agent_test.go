package server_test

import (
	"bytes"
	"context"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// Model spend and tool payments of one agent appear side by side, and the
// ledger's payments agree with the chain.
func TestReportByAgentSideBySideAndReconciled(t *testing.T) {
	p := newPayEnv(t)
	if resp, _ := p.do("/v1/chat/completions", chatBody("oa/m", false), nil); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if code, m, _ := p.pay("echo", "rep-1"); code != 200 {
		t.Fatalf("%d %v", code, m)
	}
	lines, err := audit.ReportByAgent(context.Background(), p.pool, time.Now().Add(-time.Hour))
	if err != nil || len(lines) != 1 {
		t.Fatalf("%v %+v", err, lines)
	}
	l := lines[0]
	if l.ModelReqs != 1 || l.Model <= 0 || l.PaymentReqs != 1 || l.Payment != 10_000_000 || l.Total() != l.Model+l.Payment {
		t.Fatalf("%+v", l)
	}
	var out bytes.Buffer
	audit.PrintAgents(&out, lines)
	if !strings.Contains(out.String(), "model USD") || !strings.Contains(out.String(), "payments USD") || !strings.Contains(out.String(), "TOTAL") {
		t.Fatalf("table:\n%s", out.String())
	}
	rep, err := wallet.ReconcileChain(context.Background(), p.pool, p.sim.Chain, time.Now().Add(-time.Hour))
	if err != nil || rep.Checked != 1 || rep.Matched != 1 || len(rep.Findings) != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
}

// A pending payment shows as pending in the report, with no spend until the chain says so.
func TestReportShowsPendingPayment(t *testing.T) {
	p := newPayEnv(t)
	p.toolLies.Store(true)
	if code, _, _ := p.pay("echo", "rep-2"); code != 202 {
		t.Fatal(code)
	}
	lines, _ := audit.ReportByAgent(context.Background(), p.pool, time.Now().Add(-time.Hour))
	if len(lines) != 1 || lines[0].PaymentsPending != 1 || lines[0].Payment != 0 {
		t.Fatalf("%+v", lines)
	}
	rep, err := wallet.ReconcileChain(context.Background(), p.pool, p.sim.Chain, time.Now().Add(-time.Hour))
	if err != nil || rep.Open != 1 || len(rep.Findings) != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
}

// A ledger charge whose authorization never executed on chain is found.
func TestReconcileFindsLedgerEntryWithNoTransfer(t *testing.T) {
	p := newPayEnv(t)
	ctx := context.Background()
	h, err := p.led.Reserve(ctx, ledger.ReserveReq{AccountID: p.acct, Amount: 10_000_000, IdemKey: "ghost", Kind: ledger.KindPayment, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var nonce [32]byte = x402.PaymentNonce(h.ID)
	ref := x402.AuthRef{From: p.w.Address(), Nonce: nonce, Value: "10000", ValidBefore: time.Now().Unix() + 60, Token: p.sim.Token}
	if err := p.led.AttachUpstream(ctx, h.ID, x402.Provider, ref.Encode()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.led.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 10_000_000, IdemKey: "settle:" + h.ID}); err != nil {
		t.Fatal(err)
	}
	rep, err := wallet.ReconcileChain(ctx, p.pool, p.sim.Chain, time.Now().Add(-time.Hour))
	if err != nil || len(rep.Findings) != 1 || rep.Findings[0].Kind != wallet.FindingNoTransfer || rep.Findings[0].HoldID != h.ID {
		t.Fatalf("%v %+v", err, rep)
	}
}

// The reverse: money moved on chain, the ledger holds nothing for it.
func TestReconcileFindsTransferWithNoLedgerEntry(t *testing.T) {
	p := newPayEnv(t)
	p.toolLies.Store(true)
	if code, _, _ := p.pay("echo", "rev-1"); code != 202 {
		t.Fatal(code)
	}
	h, _ := p.kept.Load().(string)
	rq := httptest.NewRequest("POST", "/echo", strings.NewReader(`{"text":"x"}`))
	rq.Header.Set(x402.HeaderSignature, h)
	rec := httptest.NewRecorder()
	p.submit.ServeHTTP(rec, rq)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	// The hold is still open: moved on chain, not yet in the ledger.
	rep, err := wallet.ReconcileChain(context.Background(), p.pool, p.sim.Chain, time.Now().Add(-time.Hour))
	if err != nil || len(rep.Findings) != 1 || rep.Findings[0].Kind != wallet.FindingNoLedger {
		t.Fatalf("active: %v %+v", err, rep)
	}
	// Released although the chain moved (the failure this task closes): still found.
	var id string
	_ = p.pool.QueryRow(context.Background(), `SELECT id FROM holds WHERE kind='payment'`).Scan(&id)
	if _, err := p.led.Release(context.Background(), id, "bad-release", "test"); err != nil {
		t.Fatal(err)
	}
	rep, _ = wallet.ReconcileChain(context.Background(), p.pool, p.sim.Chain, time.Now().Add(-time.Hour))
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != wallet.FindingNoLedger || !strings.Contains(rep.Findings[0].Detail, "released") {
		t.Fatalf("released: %+v", rep)
	}
	_ = big.NewInt
	_ = money.Nanos(0)
}
