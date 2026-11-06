// Package ledger is the credits ledger. T0 fixes the types and interface;
// T1 implements them. The invariants I1-I8 live in PLAN.md.
package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/ws4charlie/agent-gate/internal/money"
)

type Kind string // "model" | "tool" | "payment"

const (
	KindModel   Kind = "model"
	KindTool    Kind = "tool"
	KindPayment Kind = "payment"
)

var (
	ErrInsufficientFunds   = errors.New("ledger: insufficient funds")
	ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with different parameters")
	ErrAccountInactive     = errors.New("ledger: account inactive")
	ErrNotImplemented      = errors.New("not implemented")

	// ErrHoldNotActive: the hold is already terminal (or, for Extend, not
	// active) under a different key. A caller that lost a race to another
	// settle or release must skip, not retry (I3).
	ErrHoldNotActive   = errors.New("ledger: hold not active")
	ErrHoldNotFound    = errors.New("ledger: hold not found")
	ErrAccountNotFound = errors.New("ledger: account not found")
	ErrInvalidRequest  = errors.New("ledger: invalid request")
)

type ReserveReq struct {
	AccountID string
	Amount    money.Nanos
	IdemKey   string
	Kind      Kind
	TTL       time.Duration
	Memo      string
}

type Hold struct {
	ID, AccountID, IdemKey string
	Kind                   Kind
	Amount                 money.Nanos
	Status                 string
	ExpiresAt              time.Time
	Provider, UpstreamID   string
}

type SettleReq struct {
	HoldID  string
	Actual  money.Nanos
	IdemKey string
	Memo    string
}

type Settlement struct {
	HoldID                         string
	Charged, Released, Unrecovered money.Nanos
}

type Balance struct {
	AccountID     string
	Balance, Held money.Nanos
}

type Ledger interface {
	CreateAccount(ctx context.Context, name string) (accountID string, err error)
	Deposit(ctx context.Context, accountID string, amount money.Nanos, idemKey, memo string) error
	Reserve(ctx context.Context, r ReserveReq) (Hold, error) // ErrInsufficientFunds, ErrIdempotencyConflict, ErrAccountInactive
	Extend(ctx context.Context, holdID string, ttl time.Duration) error
	AttachUpstream(ctx context.Context, holdID, provider, upstreamID string) error
	Settle(ctx context.Context, r SettleReq) (Settlement, error)
	Release(ctx context.Context, holdID, idemKey, reason string) (Settlement, error)
	Balance(ctx context.Context, accountID string) (Balance, error)
	Audit(ctx context.Context) error // proves I1, I2
}

type Usage struct {
	Cost   money.Nanos
	Source string
	// ChargeHold means the real cost cannot be known (the provider has no usage
	// lookup, yet may have billed the call), so the reconciler charges the hold
	// in full and ignores Cost. Overcharging up to the hold beats silent loss (I6).
	ChargeHold bool
}

type UsageLookup interface {
	Lookup(ctx context.Context, provider, upstreamID string) (Usage, bool, error)
}

// Report summarises one reconciler pass (I6). Holds another process had
// locked, or that went terminal first, are skipped and counted nowhere.
type Report struct {
	Settled  int // settled from usage found by UsageLookup
	Released int // released unmetered: no upstream id, or lookup found nothing
	Errors   int // holds left active because lookup or the write failed

	// Outcomes lists every hold this pass closed, so the caller can audit them.
	Outcomes []ReconcileOutcome
}

// ReconcileOutcome is one hold closed by the reconciler. For "settled", Amount
// is what was charged and Source says where the usage came from; for
// "released" (unmetered), Amount is the hold returned to the account.
type ReconcileOutcome struct {
	HoldID, AccountID string
	Result            string // "settled" | "released"
	Amount            money.Nanos
	Source            string
}
