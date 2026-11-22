package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/bank"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/server"
)

const rcpt = "rcpt-0001"

type bankEnv struct {
	*env
	fake *bank.Fake
	svc  *bank.Service
}

// newBankEnv starts a gateway with the bank rail on, backed by the fake rail.
// The agent may pay rcpt through tool mercury/ach, up to $100 a day, and a
// payment above $50 needs a Gate human.
func ledgerSettle(p bank.Payment) ledger.SettleReq {
	return ledger.SettleReq{HoldID: p.HoldID, Actual: p.Amount, IdemKey: "settle:" + p.HoldID, Memo: "bank payment " + p.ID}
}

func newBankEnv(t *testing.T) *bankEnv { return newBankEnvWith(t, nil) }

func newBankEnvWith(t *testing.T, extra func(*server.Server)) *bankEnv {
	t.Helper()
	b := &bankEnv{fake: bank.NewFake()}
	b.env = newEnv(t, 1000*usd, func(s *server.Server) {
		if extra != nil {
			extra(s)
		}
		s.Agents = agents.NewStore(s.Pool)
		s.Approvals = &approval.Queue{Store: approval.NewStore(s.Pool), Ledger: s.Ledger}
		b.svc = &bank.Service{Env: "sandbox", Store: bank.NewStore(s.Pool), Rail: b.fake, Ledger: s.Ledger, Charges: s.Agents, Approvals: s.Approvals.Store}
		s.Bank = b.svc
	})
	b.setScope(func(sc *policy.Scope) {
		sc.Tools, sc.Payees = []string{server.BankTool}, []string{rcpt}
		sc.DailyBudget, sc.ApprovalThreshold = 100*usd, 50*usd
	})
	return b
}

func (b *bankEnv) pay(recipient, amount, idem string) (int, map[string]any) {
	b.t.Helper()
	body := fmt.Sprintf(`{"recipient_id":%q,"amount_usd":%q,"idempotency_key":%q,"memo":"invoice 7"}`, recipient, amount, idem)
	resp, raw := b.do("/v1/bank/payments", body, nil)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func (b *bankEnv) poll() {
	b.t.Helper()
	if _, err := b.svc.Poll(context.Background()); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bankEnv) payment(id string) bank.Payment {
	b.t.Helper()
	p, err := b.svc.Store.Get(context.Background(), id)
	if err != nil {
		b.t.Fatal(err)
	}
	return p
}

func (b *bankEnv) chain(id string) []string {
	b.t.Helper()
	evs, err := b.svc.Store.Events(context.Background(), id)
	if err != nil {
		b.t.Fatal(err)
	}
	var out []string
	for i, e := range evs {
		if e.Seq != i+1 {
			b.t.Fatalf("chain not contiguous: %+v", evs)
		}
		out = append(out, string(e.From)+">"+string(e.To))
	}
	return out
}

// auditChain counts the audit rows of a payment: one per event, same transaction.
func (b *bankEnv) auditStatuses(id string) []string {
	b.t.Helper()
	rows, err := b.pool.Query(context.Background(), `SELECT status FROM audit_events WHERE request_id=$1 ORDER BY id`, "bank:"+id)
	if err != nil {
		b.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func wantChain(t *testing.T, got []string, want ...string) {
	t.Helper()
	// Every payment starts with the claim (received) and the note that its hold exists.
	// The first want entry is the state the payment left received for.
	want = append([]string{">received", "received>received", "received" + want[0]}, want[1:]...)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("chain\n got  %v\n want %v", got, want)
	}
}

func TestBankRailOffIsNotFound(t *testing.T) {
	e := newEnv(t, usd, nil)
	resp, _ := e.do("/v1/bank/payments", `{}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rail off: want 404, got %d", resp.StatusCode)
	}
}

// A refusal never reaches the bank, and leaves a refused payment with its chain and audit row.
func TestBankRefusedNeverCallsRail(t *testing.T) {
	b := newBankEnv(t)
	cases := []struct{ name, recipient, amount, rule string }{
		{"payee not on the allowlist", "someone-else", "5", policy.RuleScopePayee},
		{"over the daily budget", rcpt, "101", policy.RuleBudgetDaily},
	}
	for i, c := range cases {
		code, m := b.pay(c.recipient, c.amount, fmt.Sprintf("refuse-%d", i))
		if code != http.StatusForbidden && code != http.StatusTooManyRequests {
			t.Fatalf("%s: status %d %v", c.name, code, m)
		}
		if errCode(m) != c.rule {
			t.Fatalf("%s: rule %q, want %s (%v)", c.name, errCode(m), c.rule, m)
		}
	}
	if b.fake.Calls != 0 {
		t.Fatalf("the rail was called %d times for refused payments", b.fake.Calls)
	}
	ps, _ := b.svc.Store.List(context.Background(), 10)
	if len(ps) != 2 {
		t.Fatalf("want 2 refused payments, got %d", len(ps))
	}
	for _, p := range ps {
		if p.State != bank.Refused || p.Rule == "" {
			t.Fatalf("payment %+v", p)
		}
		wantChain(t, b.chain(p.ID), ">refused")
		if got := b.auditStatuses(p.ID); len(got) != 3 || got[2] != "bank_refused" {
			t.Fatalf("audit %v", got)
		}
	}
	b.noActiveHolds()
	if b.balance().Balance != 1000*usd {
		t.Fatalf("balance moved: %+v", b.balance())
	}
	// A repeat of a refused key answers the same, still without the bank.
	if code, _ := b.pay("someone-else", "5", "refuse-0"); code != http.StatusForbidden || b.fake.Calls != 0 {
		t.Fatalf("refused replay: %d calls %d", code, b.fake.Calls)
	}
}

func TestBankAllowedWaitsForBankApproval(t *testing.T) {
	b := newBankEnv(t)
	code, m := b.pay(rcpt, "12.34", "k1")
	if code != http.StatusAccepted || m["state"] != "awaiting_bank_approval" || m["mercury_request_id"] != "req-1" {
		t.Fatalf("%d %v", code, m)
	}
	id := m["payment_id"].(string)
	if b.fake.Sent != 1 || b.balance().Held != 12_340_000_000 {
		t.Fatalf("sent %d held %v", b.fake.Sent, b.balance().Held)
	}
	// Same key again: the stored answer, no second request at the bank.
	code, m = b.pay(rcpt, "12.34", "k1")
	if code != http.StatusOK || m["idempotent_replay"] != true || m["payment_id"] != id {
		t.Fatalf("replay %d %v", code, m)
	}
	if b.fake.Calls != 1 {
		t.Fatalf("replay reached the bank: %d calls", b.fake.Calls)
	}
	// Same key, different amount: refused.
	if code, _ = b.pay(rcpt, "12.35", "k1"); code != http.StatusUnprocessableEntity {
		t.Fatalf("different body, same key: %d", code)
	}
	wantChain(t, b.chain(id), ">awaiting_bank_approval", "awaiting_bank_approval>awaiting_bank_approval")
	// The agent can read its payment; another account cannot.
	req, _ := http.NewRequest(http.MethodGet, b.gw.URL+"/v1/bank/payments/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+b.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: %v %v", err, resp)
	}
	resp.Body.Close()
}

func TestBankOverThresholdWaitsForGateHumanThenSubmitsOnce(t *testing.T) {
	b := newBankEnv(t)
	code, m := b.pay(rcpt, "60", "big")
	if code != http.StatusAccepted || m["status"] != "approval_required" {
		t.Fatalf("%d %v", code, m)
	}
	aid := m["approval_id"].(string)
	if b.fake.Calls != 0 {
		t.Fatal("the bank was called before a Gate human approved")
	}
	ps, _ := b.svc.Store.List(context.Background(), 10)
	if len(ps) != 1 || ps[0].State != bank.AwaitingGateApproval {
		t.Fatalf("%+v", ps)
	}
	id := ps[0].ID
	// Asking again while it waits: still waiting, still one payment.
	if code, m = b.pay(rcpt, "60", "big"); code != http.StatusAccepted || m["status"] != "approval_required" {
		t.Fatalf("%d %v", code, m)
	}
	if _, err := b.srv.Approvals.Allow(context.Background(), aid, "alice"); err != nil {
		t.Fatal(err)
	}
	code, m = b.pay(rcpt, "60", "big")
	if code != http.StatusAccepted || m["state"] != "awaiting_bank_approval" || m["payment_id"] != id {
		t.Fatalf("%d %v", code, m)
	}
	// A third call is a replay: the approval is spent and the bank is not asked again.
	if code, m = b.pay(rcpt, "60", "big"); code != http.StatusOK || m["idempotent_replay"] != true {
		t.Fatalf("%d %v", code, m)
	}
	if b.fake.Sent != 1 || b.fake.Calls != 1 {
		t.Fatalf("submitted %d times (%d calls)", b.fake.Sent, b.fake.Calls)
	}
	wantChain(t, b.chain(id), ">awaiting_gate_approval", "awaiting_gate_approval>awaiting_bank_approval", "awaiting_bank_approval>awaiting_bank_approval")
	evs, _ := b.svc.Store.Events(context.Background(), id)
	if evs[3].Actor != "gate-approver:alice" || evs[0].Actor != "agent:"+b.payment(id).AgentID {
		t.Fatalf("actors %q %q", evs[0].Actor, evs[3].Actor)
	}
	if len(b.auditStatuses(id)) != len(evs) {
		t.Fatalf("audit rows %v for %d events", b.auditStatuses(id), len(evs))
	}
}

func TestBankGateDenialRefusesThePayment(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "60", "big")
	if _, err := b.srv.Approvals.Deny(context.Background(), m["approval_id"].(string), "alice"); err != nil {
		t.Fatal(err)
	}
	b.poll()
	ps, _ := b.svc.Store.List(context.Background(), 10)
	if ps[0].State != bank.Refused || ps[0].Rule != "approval_denied" || b.fake.Calls != 0 {
		t.Fatalf("%+v calls %d", ps[0], b.fake.Calls)
	}
	b.noActiveHolds()
}

// The whole good path through the poller, ending at settled, with the ledger following.
func TestBankPollerSendsAndSettles(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "20", "k")
	id := m["payment_id"].(string)

	b.poll() // still pending at the bank: nothing changes
	if b.payment(id).State != bank.AwaitingBankApproval {
		t.Fatal("moved without the bank")
	}
	b.fake.SetRequest("req-1", bank.ReqApproved, bank.Review{Reviewer: "user-2", Status: "approved"})
	b.fake.SetTxn("req-1", bank.TxnPending, false)
	b.poll()
	if b.payment(id).State != bank.BankApproved {
		t.Fatalf("state %s", b.payment(id).State)
	}
	b.fake.SetTxn("req-1", bank.TxnSent, false)
	b.poll()
	if b.payment(id).State != bank.Sent || b.balance().Balance != 980*usd || b.balance().Held != 0 {
		t.Fatalf("state %s balance %+v", b.payment(id).State, b.balance())
	}
	b.fake.SetTxn("req-1", bank.TxnSent, true)
	b.poll()
	if b.payment(id).State != bank.Settled {
		t.Fatalf("state %s", b.payment(id).State)
	}
	wantChain(t, b.chain(id), ">awaiting_bank_approval", "awaiting_bank_approval>awaiting_bank_approval",
		"awaiting_bank_approval>bank_approved", "bank_approved>sent", "sent>settled")
	evs, _ := b.svc.Store.Events(context.Background(), id)
	if !strings.Contains(evs[4].Detail, "user-2") { // the Mercury approver is in the chain
		t.Fatalf("no Mercury reviewer in %s", evs[4].Detail)
	}
	if len(b.auditStatuses(id)) != len(evs) {
		t.Fatalf("audit %v", b.auditStatuses(id))
	}
	b.noActiveHolds()
}

// A bank that shows the transaction already sent: the chain still passes bank_approved.
func TestBankSkippedAheadStillWritesEveryStep(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "5", "k")
	b.fake.SetRequest("req-1", bank.ReqApproved)
	b.fake.SetTxn("req-1", bank.TxnSent, true)
	b.poll()
	wantChain(t, b.chain(m["payment_id"].(string)), ">awaiting_bank_approval", "awaiting_bank_approval>awaiting_bank_approval",
		"awaiting_bank_approval>bank_approved", "bank_approved>sent", "sent>settled")
}

// A cancel in the Mercury dashboard is noticed by polling: the hold and the budget come back.
func TestBankCancelNoticedByPollingReleasesHold(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "40", "k")
	id := m["payment_id"].(string)
	b.fake.SetRequest("req-1", bank.ReqCancelled)
	b.poll()
	if b.payment(id).State != bank.Cancelled {
		t.Fatalf("state %s", b.payment(id).State)
	}
	b.noActiveHolds()
	if b.balance().Balance != 1000*usd {
		t.Fatalf("balance %+v", b.balance())
	}
	// The budget is free again: $100 fits (it would not if $40 were still counted).
	if code, m := b.pay(rcpt, "100", "k2"); code != http.StatusAccepted {
		t.Fatalf("budget not freed: %d %v", code, m)
	}
}

func TestBankBankRejectedAndFailed(t *testing.T) {
	b := newBankEnv(t)
	_, m1 := b.pay(rcpt, "5", "a")
	b.fake.SetRequest("req-1", bank.ReqRejected)
	_, m2 := b.pay(rcpt, "6", "b")
	b.fake.SetRequest("req-2", bank.ReqApproved)
	b.fake.SetTxn("req-2", bank.TxnFailed, false)
	b.poll()
	for _, id := range []string{m1["payment_id"].(string), m2["payment_id"].(string)} {
		if b.payment(id).State != bank.Failed {
			t.Fatalf("%s is %s", id, b.payment(id).State)
		}
	}
	b.noActiveHolds()
	if b.balance().Balance != 1000*usd {
		t.Fatalf("balance %+v", b.balance())
	}
}

// A bank 4xx on submit ends the payment at once; a lost answer is retried by the poller with the same key.
func TestBankSubmitRefusedAndRetried(t *testing.T) {
	b := newBankEnv(t)
	b.fake.SendErr = bank.ErrRejected
	code, m := b.pay(rcpt, "5", "a")
	if code != http.StatusBadGateway || errCode(m) != "bank_refused" {
		t.Fatalf("%d %v", code, m)
	}
	b.noActiveHolds()

	b.fake.SendErr = errors.New("connection reset")
	code, m = b.pay(rcpt, "7", "b")
	if code != http.StatusAccepted || m["mercury_request_id"] != nil || m["note"] == nil {
		t.Fatalf("%d %v", code, m)
	}
	id := m["payment_id"].(string)
	b.fake.SendErr = nil
	b.poll()
	if p := b.payment(id); p.RequestID != "req-1" || p.State != bank.AwaitingBankApproval {
		t.Fatalf("%+v", p)
	}
	if b.fake.Sent != 1 {
		t.Fatalf("sent %d", b.fake.Sent)
	}
}

func TestBankUnknownBankStatusKeepsState(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "5", "k")
	b.fake.SetRequest("req-1", "somethingNew")
	b.poll()
	if b.payment(m["payment_id"].(string)).State != bank.AwaitingBankApproval {
		t.Fatal("state changed on an unknown status")
	}
	b.fake.SetRequest("req-1", bank.ReqApproved)
	b.fake.SetTxn("req-1", "alsoNew", false)
	b.poll()
	if b.payment(m["payment_id"].(string)).State != bank.AwaitingBankApproval {
		t.Fatal("state changed on an unknown transaction status")
	}
}

// An ACH return after sent: state, ledger, budget and audit agree.
func TestBankReturnReversesLedgerAndBudget(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "40", "k")
	id := m["payment_id"].(string)
	b.fake.SetRequest("req-1", bank.ReqApproved)
	b.fake.SetTxn("req-1", bank.TxnSent, true)
	b.poll()
	if b.balance().Balance != 960*usd {
		t.Fatalf("not charged: %+v", b.balance())
	}
	// The bank reports the transaction reversed: a real return, found by polling.
	b.fake.SetTxn("req-1", bank.TxnReversed, true)
	b.poll()
	if p := b.payment(id); p.State != bank.Returned {
		t.Fatalf("state %s", p.State)
	}
	if b.balance().Balance != 1000*usd || b.balance().Held != 0 {
		t.Fatalf("ledger not reversed: %+v", b.balance())
	}
	if err := b.led.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantChain(t, b.chain(id), ">awaiting_bank_approval", "awaiting_bank_approval>awaiting_bank_approval",
		"awaiting_bank_approval>bank_approved", "bank_approved>sent", "sent>settled", "settled>returned")
	if got := b.auditStatuses(id); got[len(got)-1] != "bank_returned" || len(got) != 8 {
		t.Fatalf("audit %v", got)
	}
	evs, _ := b.svc.Store.Events(context.Background(), id)
	for _, e := range evs {
		t.Logf("%d %s -> %s by %s", e.Seq, e.From, e.To, e.Actor)
	}
	t.Logf("balance after return: %+v; audit rows %v", b.balance(), b.auditStatuses(id))
	// Budget freed: $100 fits again.
	if code, m := b.pay(rcpt, "100", "k2"); code != http.StatusAccepted {
		t.Fatalf("budget not freed: %d %v", code, m)
	}
	// Polling again changes nothing (the return is recorded once).
	b.poll()
	if bl := b.balance(); bl.Balance != 1000*usd || bl.Held != 100*usd {
		t.Fatalf("%+v", b.balance())
	}
}

func TestBankSimulatedReturn(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "10", "k")
	id := m["payment_id"].(string)
	if _, err := b.svc.Return(context.Background(), id, "R01", "simulated-return"); err == nil {
		t.Fatal("a payment that was never sent cannot be returned")
	}
	b.fake.SetRequest("req-1", bank.ReqApproved)
	b.fake.SetTxn("req-1", bank.TxnSent, false)
	b.poll()
	p, err := b.svc.Return(context.Background(), id, "R01", "simulated-return")
	if err != nil || p.State != bank.Returned || b.balance().Balance != 1000*usd {
		t.Fatalf("%v %+v %+v", err, p, b.balance())
	}
	evs, _ := b.svc.Store.Events(context.Background(), id)
	last := evs[len(evs)-1]
	if last.Actor != "simulated-return" || !strings.Contains(last.Detail, "R01") || !strings.Contains(last.Detail, "simulated") {
		t.Fatalf("%+v", last)
	}
}

// Everything the poller needs is in the database: a new Service picks the payment up where the old one stopped.
func TestBankRestartResumesFromDatabase(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "20", "k")
	id := m["payment_id"].(string)
	b.fake.SetRequest("req-1", bank.ReqApproved)
	b.fake.SetTxn("req-1", bank.TxnPending, false)
	b.poll()

	fresh := &bank.Service{Store: bank.NewStore(b.pool), Rail: b.fake, Ledger: b.led, Charges: agents.NewStore(b.pool), Approvals: approval.NewStore(b.pool)}
	b.fake.SetTxn("req-1", bank.TxnSent, true)
	if _, err := fresh.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.payment(id).State != bank.Settled || b.fake.Sent != 1 {
		t.Fatalf("state %s, sent %d", b.payment(id).State, b.fake.Sent)
	}
}

// The event chain cannot be edited.
func TestBankEventsAreAppendOnly(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "5", "k")
	id := m["payment_id"].(string)
	for _, q := range []string{`UPDATE bank_payment_events SET actor='x' WHERE payment_id=$1`, `DELETE FROM bank_payment_events WHERE payment_id=$1`} {
		if _, err := b.pool.Exec(context.Background(), q, id); err == nil {
			t.Fatalf("%q was allowed", q)
		}
	}
}

// With Cedar deciding and the registry check on, a bank payment needs an
// approved registry entry for mercury/ach, and both engines agree.
func TestBankCedarEnforceAndRegistry(t *testing.T) {
	b := newBankEnvWith(t, func(s *server.Server) { registryOn(t, s) })
	code, m := b.pay(rcpt, "5", "r1")
	if code != http.StatusForbidden || errCode(m) != policy.RuleRegistryUnapproved || b.fake.Calls != 0 {
		t.Fatalf("no registry entry: %d %v calls %d", code, m, b.fake.Calls)
	}
	b.setRegistry("tool", server.BankTool, "approved")
	code, m = b.pay(rcpt, "5", "r2")
	if code != http.StatusAccepted || m["state"] != "awaiting_bank_approval" {
		t.Fatalf("approved entry: %d %v", code, m)
	}
	rows, err := b.pool.Query(context.Background(), `SELECT rule, go_rule, cedar_rule FROM policy_decisions WHERE kind='payment' ORDER BY at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var rule, goRule, cedarRule string
		_ = rows.Scan(&rule, &goRule, &cedarRule)
		if rule != goRule || rule != cedarRule {
			t.Fatalf("engines disagree: %s go %s cedar %s", rule, goRule, cedarRule)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("want 2 decision rows, got %d", n)
	}
}

// ---- fix1: guards from the PR review ----

// Two requests with the same key at once: one payment, one hold, one charge,
// one submit, and the hold is still active. The loser never touches the hold.
func TestBankConcurrentSameKeyOneHoldOneSubmit(t *testing.T) {
	b := newBankEnv(t)
	const n = 8
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() { c, _ := b.pay(rcpt, "10", "same"); codes <- c }()
	}
	for i := 0; i < n; i++ {
		switch c := <-codes; c {
		case http.StatusAccepted, http.StatusOK, http.StatusConflict:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	ps, _ := b.svc.Store.List(context.Background(), 10)
	if len(ps) != 1 || b.fake.Sent != 1 {
		t.Fatalf("%d payments, %d submits", len(ps), b.fake.Sent)
	}
	var holds, active, charges int
	_ = b.pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE status='active') FROM holds WHERE kind='payment'`).Scan(&holds, &active)
	_ = b.pool.QueryRow(context.Background(), `SELECT count(*) FROM budget_charges`).Scan(&charges)
	if holds != 1 || active != 1 || charges != 1 || b.balance().Held != 10*usd {
		t.Fatalf("holds %d active %d charges %d held %v", holds, active, charges, b.balance().Held)
	}
}

// Sent while the hold was released behind our back (the lease ran out and the
// reconciler freed it): the debit is taken again, and a later return refunds
// exactly what was debited, never more.
func TestBankSentAfterHoldReleasedStillDebits(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "40", "k")
	id := m["payment_id"].(string)
	p := b.payment(id)
	if _, err := b.led.Release(context.Background(), p.HoldID, "test-release", "lease ran out"); err != nil {
		t.Fatal(err)
	}
	b.fake.SetRequest("req-1", bank.ReqApproved)
	b.fake.SetTxn("req-1", bank.TxnSent, false)
	b.poll()
	if got := b.payment(id).State; got != bank.Sent || b.balance().Balance != 960*usd {
		t.Fatalf("state %s balance %+v: a sent payment must be debited", got, b.balance())
	}
	b.fake.SetTxn("req-1", bank.TxnReversed, true)
	b.poll()
	if got := b.payment(id).State; got != bank.Returned || b.balance().Balance != 1000*usd {
		t.Fatalf("state %s balance %+v", got, b.balance())
	}
	if err := b.led.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A crash after the debit but before the state write leaves bank_approved with
// the money taken. A reversal after that must still refund.
func TestBankReturnAfterCrashBetweenDebitAndState(t *testing.T) {
	b := newBankEnv(t)
	_, m := b.pay(rcpt, "40", "k")
	id := m["payment_id"].(string)
	p := b.payment(id)
	b.fake.SetRequest("req-1", bank.ReqApproved)
	b.fake.SetTxn("req-1", bank.TxnPending, false)
	b.poll() // bank_approved
	// The debit committed, the process died before the state was written:
	if _, err := b.led.Settle(context.Background(), ledgerSettle(p)); err != nil {
		t.Fatal(err)
	}
	if b.payment(id).State != bank.BankApproved || b.balance().Balance != 960*usd {
		t.Fatal("setup wrong")
	}
	b.fake.SetTxn("req-1", bank.TxnReversed, true)
	b.poll()
	if got := b.payment(id).State; got != bank.Returned || b.balance().Balance != 1000*usd {
		t.Fatalf("state %s balance %+v: the debit was lost", got, b.balance())
	}
}

// A payment made on production Mercury cannot be returned by the simulation,
// whatever base URL the command is given: the row says where it was made.
func TestBankSimulatedReturnRefusedForProductionPayment(t *testing.T) {
	b := newBankEnv(t)
	b.svc.Env = "production"
	_, m := b.pay(rcpt, "10", "k")
	id := m["payment_id"].(string)
	b.fake.SetRequest("req-1", bank.ReqApproved)
	b.fake.SetTxn("req-1", bank.TxnSent, false)
	b.poll()
	before := b.balance()
	if _, err := b.svc.Return(context.Background(), id, "R01", "simulated-return"); err == nil {
		t.Fatal("a production payment was returned by the simulation")
	}
	if b.balance() != before || b.payment(id).State != bank.Sent {
		t.Fatalf("something changed: %+v %s", b.balance(), b.payment(id).State)
	}
}

// A request that dies after the claim leaves a received payment. The poller
// ends it and frees its hold.
func TestBankStalledClaimIsFreed(t *testing.T) {
	b := newBankEnv(t)
	hold, err := b.led.Reserve(context.Background(), ledger.ReserveReq{AccountID: b.acct, Amount: 5 * usd, IdemKey: "bank:dead", Kind: ledger.KindPayment, TTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := b.svc.Store.Create(context.Background(), bank.Payment{AccountID: b.acct, AgentID: "a", IdemKey: "dead", Fingerprint: "f",
		RecipientID: rcpt, Amount: 5 * usd, State: bank.Received, HoldID: hold.ID, Env: "sandbox"}, "agent:a", nil)
	if err != nil {
		t.Fatal(err)
	}
	b.poll()
	if b.payment(p.ID).State != bank.Received {
		t.Fatal("a fresh claim was taken away")
	}
	b.svc.Store.SetClock(func() time.Time { return time.Now().Add(11 * time.Minute) })
	b.poll()
	if got := b.payment(p.ID); got.State != bank.Failed || got.Rule != "abandoned" {
		t.Fatalf("%+v", got)
	}
	b.noActiveHolds()
}
