package ledger

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// Postgres is the Ledger backed by the schema in migrations/0001_ledger.sql.
// T0 stub: every method returns ErrNotImplemented. T1 fills the bodies.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Ledger = (*Postgres)(nil)

func New(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) CreateAccount(ctx context.Context, name string) (string, error) {
	return "", ErrNotImplemented
}

func (p *Postgres) Deposit(ctx context.Context, accountID string, amount money.Nanos, idemKey, memo string) error {
	return ErrNotImplemented
}

func (p *Postgres) Reserve(ctx context.Context, r ReserveReq) (Hold, error) {
	return Hold{}, ErrNotImplemented
}

func (p *Postgres) Extend(ctx context.Context, holdID string, ttl time.Duration) error {
	return ErrNotImplemented
}

func (p *Postgres) AttachUpstream(ctx context.Context, holdID, provider, upstreamID string) error {
	return ErrNotImplemented
}

func (p *Postgres) Settle(ctx context.Context, r SettleReq) (Settlement, error) {
	return Settlement{}, ErrNotImplemented
}

func (p *Postgres) Release(ctx context.Context, holdID, idemKey, reason string) (Settlement, error) {
	return Settlement{}, ErrNotImplemented
}

func (p *Postgres) Balance(ctx context.Context, accountID string) (Balance, error) {
	return Balance{}, ErrNotImplemented
}

func (p *Postgres) Audit(ctx context.Context) error { return ErrNotImplemented }

// Reconcile finds holds past expires_at and settles or releases them (I6). It
// lives on Postgres, not the Ledger interface: scanning expired holds with
// FOR UPDATE SKIP LOCKED needs a transaction the interface cannot express.
func (p *Postgres) Reconcile(ctx context.Context, lookup UsageLookup, now time.Time) (Report, error) {
	return Report{}, ErrNotImplemented
}
