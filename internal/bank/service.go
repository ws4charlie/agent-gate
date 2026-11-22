package bank

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
)

// Charges is the budget side of a payment, inside the caller's transaction
// (agents.Store implements it).
type Charges interface {
	// CloseChargeTx closes the hold's budget charge at what was really spent.
	CloseChargeTx(ctx context.Context, tx pgx.Tx, holdID string, actual money.Nanos) error
	// RecordSpendTx counts money that left whatever the limits say.
	RecordSpendTx(ctx context.Context, tx pgx.Tx, agentID, taskID, holdID string, amount money.Nanos) error
}

// TxLedger is the ledger with its operations that run in the caller's
// transaction. *ledger.Postgres implements it. The bank and the ledger share
// one database, so a payment's money and its state commit as one.
type TxLedger interface {
	ledger.Ledger
	ReserveTx(ctx context.Context, tx pgx.Tx, r ledger.ReserveReq) (ledger.Hold, error)
	SettleTx(ctx context.Context, tx pgx.Tx, r ledger.SettleReq) (ledger.Settlement, error)
	ReleaseTx(ctx context.Context, tx pgx.Tx, holdID, idemKey, reason string) (ledger.Settlement, error)
	DepositTx(ctx context.Context, tx pgx.Tx, accountID string, amount money.Nanos, idemKey, memo string) error
	HoldStatusTx(ctx context.Context, tx pgx.Tx, holdID string) (string, error)
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
	Ledger    TxLedger
	Charges   Charges
	Approvals ApprovalFinder
	Log       *slog.Logger
	// HoldTTL is how long a bank payment's hold lives; the poller extends it
	// on every pass while the payment waits.
	HoldTTL time.Duration
	// FaultAfterMoney is for tests: when set, it is called inside the
	// transaction after the ledger work and before the state is written; an
	// error rolls everything back, as a crash at that point would.
	FaultAfterMoney func(to State) error
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

// effect does the money side of entering state `to`, inside the transaction
// that writes the state. A crash or an error rolls both back; the next pass
// does both again. It may change p.HoldID (a re-debit); the state write then
// stores the new id.
func (s *Service) effect(ctx context.Context, tx pgx.Tx, p *Payment, to State, code string) error {
	switch to {
	case Sent:
		return s.debit(ctx, tx, p)
	case Cancelled, Failed, Refused:
		if p.HoldID == "" {
			return nil
		}
		_, err := s.Ledger.ReleaseTx(ctx, tx, p.HoldID, approval.ReleaseKey(p.HoldID), "bank payment "+string(to))
		if err != nil && !errors.Is(err, ledger.ErrHoldNotActive) {
			return err
		}
		return s.closeCharge(ctx, tx, *p)
	case Returned:
		// Never create money: only a payment whose debit exists can be refunded.
		if st, err := s.Ledger.HoldStatusTx(ctx, tx, p.HoldID); err != nil || st != "settled" {
			return fmt.Errorf("no debit to reverse for payment %s (hold is %q): %v", p.ID, st, err)
		}
		memo := "ACH return of bank payment " + p.ID
		if code != "" {
			memo += " (" + code + ")"
		}
		if err := s.Ledger.DepositTx(ctx, tx, p.AccountID, p.Amount, "bank-return:"+p.ID, memo); err != nil {
			return err
		}
		return s.closeCharge(ctx, tx, *p)
	}
	return nil
}

// debit takes the money for a payment that was sent. A hold that is already
// settled is our own earlier debit. A hold that was released is not: the bank
// went on after the money was freed. Then the debit is taken again from a new
// hold that is reserved and settled in this same transaction, so it can never
// be left half done or expire unsettled, and the spend is counted against the
// budget again. If the money is not there the transaction fails and the
// payment does not advance.
func (s *Service) debit(ctx context.Context, tx pgx.Tx, p *Payment) error {
	st, err := s.Ledger.HoldStatusTx(ctx, tx, p.HoldID)
	if err != nil {
		return err
	}
	switch st {
	case "settled":
		return nil
	case "active":
		_, err := s.Ledger.SettleTx(ctx, tx, ledger.SettleReq{HoldID: p.HoldID, Actual: p.Amount, IdemKey: "settle:" + p.HoldID, Memo: "bank payment " + p.ID})
		return err
	}
	h, err := s.Ledger.ReserveTx(ctx, tx, ledger.ReserveReq{AccountID: p.AccountID, Amount: p.Amount, IdemKey: "bank-redebit:" + p.ID,
		Kind: ledger.KindPayment, TTL: time.Hour, Memo: "re-debit of sent bank payment " + p.ID})
	if err != nil {
		return fmt.Errorf("re-debit of sent payment %s: %w", p.ID, err)
	}
	if _, err := s.Ledger.SettleTx(ctx, tx, ledger.SettleReq{HoldID: h.ID, Actual: p.Amount, IdemKey: "settle:" + h.ID, Memo: "bank payment " + p.ID}); err != nil {
		return err
	}
	old := p.HoldID
	p.HoldID = h.ID
	if s.Charges == nil {
		return nil
	}
	// The released hold's charge may still be open (whoever released it may
	// not have closed it): close it first, so the spend is counted once.
	if old != "" {
		if err := s.Charges.CloseChargeTx(ctx, tx, old, 0); err != nil {
			return err
		}
	}
	return s.Charges.RecordSpendTx(ctx, tx, p.AgentID, p.TaskID, h.ID, p.Amount)
}

func (s *Service) closeCharge(ctx context.Context, tx pgx.Tx, p Payment) error {
	if s.Charges == nil || p.HoldID == "" {
		return nil
	}
	return s.Charges.CloseChargeTx(ctx, tx, p.HoldID, 0)
}

// Hold reserves the payment's money and records the hold on the payment in one
// transaction, so a hold never exists without an owner the poller can find.
func (s *Service) Hold(ctx context.Context, p Payment) (ledger.Hold, Payment, error) {
	var h ledger.Hold
	np, err := s.Store.NoteWith(ctx, p.ID, "gate", map[string]any{"held": true}, Change{}, func(ctx context.Context, tx pgx.Tx, row *Payment) (err error) {
		h, err = s.Ledger.ReserveTx(ctx, tx, ledger.ReserveReq{AccountID: p.AccountID, Amount: p.Amount, IdemKey: "bank:" + p.IdemKey,
			Kind: ledger.KindPayment, TTL: s.holdTTL(), Memo: "bank payment " + p.RecipientID})
		if err == nil && h.Status == "active" {
			row.HoldID = h.ID
		}
		return err
	})
	return h, np, err
}

// Move takes a payment to `to`, stepping through the states in between (a
// transaction seen first as sent passes bank_approved), one event per step.
// Each step is one transaction: its money, its state row, its event and its
// audit row.
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
		np, _, err := s.Store.TransitionWith(ctx, p.ID, st, actor, d, c, func(ctx context.Context, tx pgx.Tx, row *Payment) error {
			if err := s.effect(ctx, tx, row, st, code); err != nil {
				return err
			}
			if s.FaultAfterMoney != nil {
				return s.FaultAfterMoney(st)
			}
			return nil
		})
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

// consumedAt is when the approval was spent (the payment's last change for an
// approval spent before that time was recorded).
func consumedAt(a approval.Approval, p Payment) time.Time {
	if a.ConsumedAt != nil {
		return *a.ConsumedAt
	}
	return p.UpdatedAt
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
	case found && a.Status == approval.Consumed && s.Store.at().Sub(consumedAt(a, p)) > stalledReceived:
		// The approval was spent by a retry that died before the payment moved. The
		// deadline runs from the moment it was spent: retries cannot extend it (a
		// retry that finds the approval consumed is turned away). Nothing else will
		// finish it: refuse it and free the hold. The agent may ask again with a new key.
		return s.Move(ctx, p, Refused, "gate", map[string]any{"approval_id": a.ID, "reason": "the approved request was interrupted"}, Change{Rule: "approval_interrupted"})
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
