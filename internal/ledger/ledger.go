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
}

type UsageLookup interface {
	Lookup(ctx context.Context, provider, upstreamID string) (Usage, bool, error)
}

// Report summarises one reconciler pass (I6). T1 decides the fields; T0 only
// reserves the name so Reconcile can be declared.
type Report struct{}

// Reconcile finds holds past expires_at and settles or releases them (I6).
func Reconcile(ctx context.Context, l Ledger, lookup UsageLookup, now time.Time) (Report, error) {
	return Report{}, ErrNotImplemented
}
