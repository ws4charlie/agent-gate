package bank

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
)

// Charges closes the budget charge of a hold (agents.Store implements it).
type Charges interface {
	CloseCharge(ctx context.Context, holdID string, actual money.Nanos) error
}

// ApprovalFinder looks up the Gate approval queued for a request.
type ApprovalFinder interface {
	Find(ctx context.Context, agentID, idemKey string) (approval.Approval, bool, error)
}

// Service moves payments through their states. The route, the poller and the
// simulated return all go through it, so every path writes the same events
// and does the same ledger work.
type Service struct {
	Store     *Store
	Rail      Rail // nil in the CLI, which only simulates a return
	Ledger    ledger.Ledger
	Charges   Charges
	Approvals ApprovalFinder
	Log       *slog.Logger
	// HoldTTL is how long a bank payment's hold lives; the poller extends it
	// on every pass while the payment waits.
	HoldTTL time.Duration
	// Env is where this gateway's bank is: "sandbox" or "production". Every
	// payment records it, and a simulated return is allowed only on a sandbox payment.
	Env string
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return s.Log
}

func (s *Service) holdTTL() time.Duration {
	if s.HoldTTL > 0 {
		return s.HoldTTL
	}
	return 30 * 24 * time.Hour
}

// HoldTTLDefault is the lease a new payment's hold gets.
func (s *Service) HoldLease() time.Duration { return s.holdTTL() }

// Target maps what the bank reports to the state the payment should be in.
// The transaction, when there is one, is the more advanced fact. ok=false
// means a status Gate does not know: the caller logs it and keeps the state.
func Target(cur State, req Request, txn *Txn) (to State, ok bool) {
	late := cur == Sent || cur == Settled // money already left: a bad ending is a return
	if txn != nil {
		switch txn.Status {
		case TxnPending:
			return BankApproved, true
		case TxnSent:
			if txn.Posted {
				return Settled, true
			}
			return Sent, true
		case TxnCancelled:
			return Cancelled, true
		case TxnFailed, TxnBlocked, TxnReversed:
			if late {
				return Returned, true
			}
			return Failed, true
		}
		return cur, false
	}
	switch req.Status {
	case ReqPending:
		return AwaitingBankApproval, true
	case ReqApproved:
		return BankApproved, true
	case ReqRejected:
		return Failed, true
	case ReqCancelled:
		return Cancelled, true
	}
	return cur, false
}

// effect does the ledger side of entering state `to`. Every call is
// idempotent (fixed keys), so a crash between the ledger and the state row is
// repaired by the next pass. It may change p.HoldID (a re-debit); the caller
// stores the new id with the state change.
func (s *Service) effect(ctx context.Context, p *Payment, to State, code string) error {
	switch to {
	case Sent:
		return s.debit(ctx, p)
	case Cancelled, Failed, Refused:
		if p.HoldID == "" {
			return nil
		}
		_, err := s.Ledger.Release(ctx, p.HoldID, approval.ReleaseKey(p.HoldID), "bank payment "+string(to))
		if err != nil && !errors.Is(err, ledger.ErrHoldNotActive) {
			return err
		}
		return s.closeCharge(ctx, *p)
	case Returned:
		// Never create money: only a payment whose debit exists can be refunded.
		if st, err := s.Store.HoldStatus(ctx, p.HoldID); err != nil || st != "settled" {
			return fmt.Errorf("no debit to reverse for payment %s (hold is %q): %v", p.ID, st, err)
		}
		memo := "ACH return of bank payment " + p.ID
		if code != "" {
			memo += " (" + code + ")"
		}
		if err := s.Ledger.Deposit(ctx, p.AccountID, p.Amount, "bank-return:"+p.ID, memo); err != nil {
			return err
		}
		return s.closeCharge(ctx, *p)
	}
	return nil
}

// debit takes the money for a payment that was sent. A hold that is already
// settled is our own earlier debit (a repeat after a crash). A hold that was
// released is not: the lease ran out and the reconciler freed it while the
// bank went on. Then the debit is taken again from a new hold, so a sent
// payment is never left unpaid (and a later return never refunds money that
// was not taken). If the money is not there, the payment does not advance.
func (s *Service) debit(ctx context.Context, p *Payment) error {
	st, err := s.Store.HoldStatus(ctx, p.HoldID)
	if err != nil {
		return err
	}
	if st == "active" {
		_, err := s.Ledger.Settle(ctx, ledger.SettleReq{HoldID: p.HoldID, Actual: p.Amount, IdemKey: "settle:" + p.HoldID, Memo: "bank payment " + p.ID})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ledger.ErrHoldNotActive) {
			return err
		}
		if st, err = s.Store.HoldStatus(ctx, p.HoldID); err != nil {
			return err
		}
	}
	if st == "settled" {
		return nil
	}
	h, err := s.Ledger.Reserve(ctx, ledger.ReserveReq{AccountID: p.AccountID, Amount: p.Amount, IdemKey: "bank-redebit:" + p.ID,
		Kind: ledger.KindPayment, TTL: time.Hour, Memo: "re-debit of sent bank payment " + p.ID})
	if err != nil {
		return fmt.Errorf("re-debit of sent payment %s: %w", p.ID, err)
	}
	if h.Status == "active" {
		if _, err := s.Ledger.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: p.Amount, IdemKey: "settle:" + h.ID, Memo: "bank payment " + p.ID}); err != nil && !errors.Is(err, ledger.ErrHoldNotActive) {
			return err
		}
	}
	p.HoldID = h.ID
	return nil
}

func (s *Service) closeCharge(ctx context.Context, p Payment) error {
	if s.Charges == nil || p.HoldID == "" {
		return nil
	}
	return s.Charges.CloseCharge(ctx, p.HoldID, 0)
}

// Move takes a payment to `to`, stepping through the states in between (a
// transaction seen first as sent passes bank_approved), one event per step.
func (s *Service) Move(ctx context.Context, p Payment, to State, actor string, detail map[string]any, ch Change) (Payment, error) {
	steps := []State{to}
	if a, b := rank(p.State), rank(to); a >= 0 && b > a+1 {
		steps = append(append([]State{}, order[a+1:b]...), to)
	}
	code, _ := detail["code"].(string)
	for i, st := range steps {
		d, c := map[string]any{"via": "skipped-ahead"}, Change{}
		if i == len(steps)-1 {
			d, c = detail, ch
		}
		held := p.HoldID
		if err := s.effect(ctx, &p, st, code); err != nil {
			return p, err
		}
		if p.HoldID != held {
			c.HoldID = p.HoldID
		}
		np, _, err := s.Store.Transition(ctx, p.ID, st, actor, d, c)
		if err != nil {
			return p, err
		}
		p = np
	}
	return p, nil
}

// Submit files the payment request at the bank, once per idempotency key. A
// bank refusal (4xx) ends the payment as failed and frees the hold. Any other
// error leaves the payment as it is: the poller submits again with the same
// key, which the bank collapses, so a lost answer never pays twice.
func (s *Service) Submit(ctx context.Context, p Payment) (Payment, error) {
	req, err := s.Rail.RequestSend(ctx, SendReq{RecipientID: p.RecipientID, Cents: p.Cents(), IdempotencyKey: "ag-" + p.ID, Note: p.Memo})
	if errors.Is(err, ErrRejected) {
		np, merr := s.Move(ctx, p, Failed, "mercury", map[string]any{"error": err.Error()}, Change{Rule: "bank_refused"})
		if merr != nil {
			return p, merr
		}
		return np, err
	}
	if err != nil {
		return p, err
	}
	np, err := s.Store.Note(ctx, p.ID, "gate", map[string]any{"submitted": true, "mercury_request_id": req.ID, "mercury_status": req.Status}, Change{RequestID: req.ID})
	if err != nil {
		return p, err
	}
	return np, nil
}

func reviewsDetail(rs []Review) []map[string]string {
	var out []map[string]string
	for _, r := range rs {
		out = append(out, map[string]string{"reviewer": r.Reviewer, "status": r.Status, "at": r.At})
	}
	return out
}

// Poll makes one pass over the payments that are not final.
func (s *Service) Poll(ctx context.Context) (int, error) {
	open, err := s.Store.Open(ctx)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, p := range open {
		if ctx.Err() != nil {
			break
		}
		np, err := s.step(ctx, p)
		if err != nil {
			s.log().Warn("bank poll", "payment", p.ID, "err", err)
		}
		if np.State != p.State {
			changed++
		}
	}
	return changed, nil
}

func (s *Service) step(ctx context.Context, p Payment) (Payment, error) {
	if p.State == Received {
		return s.stepReceived(ctx, p)
	}
	if p.State == AwaitingGateApproval {
		return s.stepGate(ctx, p)
	}
	// The debit is taken when a payment enters sent, and the state is written
	// right after. A crash between the two leaves the money taken with the
	// state behind. The stored debit is the fact: finish the step first, so a
	// later reversal is a return and is refunded.
	if p.State == AwaitingBankApproval || p.State == BankApproved {
		if st, err := s.Store.HoldStatus(ctx, p.HoldID); err != nil {
			return p, err
		} else if st == "settled" {
			np, err := s.Move(ctx, p, Sent, "gate", map[string]any{"recovered": "the debit was committed before the state"}, Change{})
			if err != nil {
				return p, err
			}
			p = np
		}
	}
	if p.State == AwaitingBankApproval || p.State == BankApproved {
		if p.HoldID != "" { // the wait can be long: keep the hold alive
			if err := s.Ledger.Extend(ctx, p.HoldID, s.holdTTL()); err != nil && !errors.Is(err, ledger.ErrHoldNotActive) {
				s.log().Warn("bank hold extend", "payment", p.ID, "err", err)
			}
		}
	}
	if s.Rail == nil {
		return p, errors.New("no bank rail configured")
	}
	if p.RequestID == "" { // submission was lost or failed: send again, same key
		return s.Submit(ctx, p)
	}
	req, err := s.Rail.GetRequest(ctx, p.RequestID)
	if err != nil {
		return p, err
	}
	var txn *Txn
	if req.Status == ReqApproved || p.State == BankApproved || p.State == Sent || p.State == Settled {
		if txn, err = s.Rail.TxnForRequest(ctx, p.RequestID); err != nil {
			return p, err
		}
	}
	to, ok := Target(p.State, req, txn)
	if !ok {
		s.log().Warn("bank: unknown status, state kept", "payment", p.ID, "request_status", req.Status, "txn_status", txnStatus(txn))
		return p, nil
	}
	// Never go backwards on the forward path (a late read of an older status).
	if a, b := rank(p.State), rank(to); a >= 0 && b >= 0 && b <= a {
		return p, nil
	}
	d := map[string]any{"mercury_request_status": req.Status, "mercury_request_id": p.RequestID}
	if len(req.Reviews) > 0 {
		d["mercury_reviews"] = reviewsDetail(req.Reviews)
	}
	ch := Change{}
	if txn != nil {
		d["mercury_txn_status"], ch.TxnID = txn.Status, txn.ID
	}
	if to == Failed && req.Status == ReqRejected && txn == nil {
		ch.Rule = "bank_rejected"
	}
	return s.Move(ctx, p, to, "mercury", d, ch)
}

func txnStatus(t *Txn) string {
	if t == nil {
		return ""
	}
	return t.Status
}

// stalledReceived is how long a payment may stay in received before the poller
// gives up on it: its request died between the claim and the decision.
const stalledReceived = 10 * time.Minute

// stepReceived frees a claim whose request died before it decided: the hold, if
// it got one, is released and the payment ends failed.
func (s *Service) stepReceived(ctx context.Context, p Payment) (Payment, error) {
	if s.Store.at().Sub(p.UpdatedAt) < stalledReceived {
		return p, nil
	}
	return s.Move(ctx, p, Failed, "gate", map[string]any{"reason": "the request ended before a decision"}, Change{Rule: "abandoned"})
}

// stepGate watches a payment that waits for a Gate human: when the approval
// was denied or expired the payment is refused (the queue already released the hold).
func (s *Service) stepGate(ctx context.Context, p Payment) (Payment, error) {
	if s.Approvals == nil {
		return p, nil
	}
	a, found, err := s.Approvals.Find(ctx, p.AgentID, p.IdemKey)
	if err != nil {
		return p, err
	}
	switch {
	case found && a.Status == approval.Denied:
		return s.Move(ctx, p, Refused, "gate-approver:"+a.DecidedBy, map[string]any{"approval_id": a.ID}, Change{Rule: "approval_denied"})
	case found && a.Status == approval.Expired:
		return s.Move(ctx, p, Refused, "gate", map[string]any{"approval_id": a.ID}, Change{Rule: "approval_expired"})
	case !found && s.Store.at().Sub(p.CreatedAt) > 24*time.Hour:
		return s.Move(ctx, p, Refused, "gate", map[string]any{"reason": "no approval was queued"}, Change{Rule: "approval_missing"})
	}
	return p, nil
}

// Return records an ACH return on a payment that was sent. A real return is
// found by Poll (the bank reports the transaction reversed); the sandbox
// cannot produce one, so the CLI calls this with actor "simulated-return"
// after checking the base URL is the sandbox. Both paths run the same Move.
func (s *Service) Return(ctx context.Context, id, code, actor string) (Payment, error) {
	p, err := s.Store.Get(ctx, id)
	if err != nil {
		return Payment{}, err
	}
	if p.Env != "sandbox" {
		return p, fmt.Errorf("payment %s was made on %s, not the sandbox: a return of it must come from the bank", p.ID, p.Env)
	}
	if p.State != Sent && p.State != Settled {
		return p, fmt.Errorf("payment is %s: only a sent or settled payment can be returned", p.State)
	}
	d := map[string]any{"code": code}
	if actor == "simulated-return" {
		d["simulated"] = true
	}
	return s.Move(ctx, p, Returned, actor, d, Change{Rule: "ach_return"})
}

// Run polls until ctx ends. It only reads the database, so a restart picks
// every open payment up again.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Poll(ctx); err != nil {
				s.log().Error("bank poll", "err", err)
			}
		}
	}
}
