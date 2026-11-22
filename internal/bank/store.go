package bank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// Payment is one row of bank_payments.
type Payment struct {
	ID, AccountID, AgentID, IdemKey, Fingerprint string
	RecipientID, Memo, TaskID                    string
	Amount                                       money.Nanos
	State                                        State
	Rule, HoldID, RequestID, TxnID, Env          string
	CreatedAt, UpdatedAt                         time.Time
	SentAt                                       *time.Time
}

// Cents is the amount in whole cents (the route only accepts whole cents).
func (p Payment) Cents() int64 { return int64(p.Amount) / 10_000_000 }

// Event is one step of a payment's chain.
type Event struct {
	PaymentID string
	Seq       int
	From, To  State
	Actor     string
	Detail    string // JSON
	At        time.Time
}

// ErrIllegal is a state change the state machine does not allow.
var ErrIllegal = errors.New("bank: illegal state change")

// Store keeps payments and their event chain.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, now: time.Now} }

// SetClock replaces the clock (tests).
func (s *Store) SetClock(f func() time.Time) { s.now = f }

func (s *Store) at() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

const cols = `id, account_id, agent_id, idem_key, fingerprint, recipient_id, amount_nanos, memo, task_id, state, rule,
	hold_id, mercury_request_id, mercury_txn_id, created_at, updated_at, sent_at, bank_env`

func scan(row pgx.Row) (Payment, error) {
	var p Payment
	var amt int64
	var st string
	err := row.Scan(&p.ID, &p.AccountID, &p.AgentID, &p.IdemKey, &p.Fingerprint, &p.RecipientID, &amt, &p.Memo, &p.TaskID, &st,
		&p.Rule, &p.HoldID, &p.RequestID, &p.TxnID, &p.CreatedAt, &p.UpdatedAt, &p.SentAt, &p.Env)
	p.Amount, p.State = money.Nanos(amt), State(st)
	return p, err
}

func js(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// writeEvent appends the chain row and its audit row inside tx. The audit row
// has request_id "bank:<payment id>", so one query pulls a payment's whole
// audit trail. Only a send carries a cost, so spend reports do not double-count.
func writeEvent(ctx context.Context, tx pgx.Tx, p Payment, from, to State, actor, detail string, at time.Time) error {
	var seq int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0)+1 FROM bank_payment_events WHERE payment_id=$1`, p.ID).Scan(&seq); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bank_payment_events (payment_id, seq, from_state, to_state, actor, detail, at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		p.ID, seq, string(from), string(to), actor, detail, at); err != nil {
		return err
	}
	status := "bank_" + string(to)
	if strings.HasPrefix(string(to), "bank_") {
		status = string(to)
	}
	cost := int64(0)
	if to == Sent && from != Sent {
		cost = int64(p.Amount)
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_events (request_id, account_id, agent_id, kind, model, provider, status, cost_source, cost_nanos, hold_id, err, upstream_id, at)
		VALUES ($1,$2,$3,'payment','mercury/ach','mercury',$4,'bank',$5,$6,$7,$8,$9)`,
		"bank:"+p.ID, p.AccountID, p.AgentID, status, cost, p.HoldID, p.Rule, p.RequestID, at)
	return err
}

// Create inserts a payment in its first state and writes event 1. A second
// call with the same (account, idempotency key) inserts nothing and returns
// the stored row with created=false.
func (s *Store) Create(ctx context.Context, p Payment, actor string, detail map[string]any) (Payment, bool, error) {
	p.ID, p.CreatedAt, p.UpdatedAt = "bp_"+uuid.NewString(), s.at(), s.at()
	if p.Env != "sandbox" { // anything else is production: the safe reading
		p.Env = "production"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Payment{}, false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO bank_payments (`+cols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (account_id, idem_key) DO NOTHING`,
		p.ID, p.AccountID, p.AgentID, p.IdemKey, p.Fingerprint, p.RecipientID, int64(p.Amount), p.Memo, p.TaskID, string(p.State), p.Rule,
		p.HoldID, p.RequestID, p.TxnID, p.CreatedAt, p.UpdatedAt, p.SentAt, p.Env)
	if err != nil {
		return Payment{}, false, err
	}
	if tag.RowsAffected() == 0 {
		// Read through the transaction we already hold. Asking the pool for a
		// second connection here deadlocks when every connection is held by a
		// transaction waiting on the winner's row.
		old, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM bank_payments WHERE account_id=$1 AND idem_key=$2`, p.AccountID, p.IdemKey))
		return old, false, err
	}
	if err := writeEvent(ctx, tx, p, "", p.State, actor, js(detail), p.CreatedAt); err != nil {
		return Payment{}, false, err
	}
	return p, true, tx.Commit(ctx)
}

func (s *Store) ByIdem(ctx context.Context, account, idem string) (Payment, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM bank_payments WHERE account_id=$1 AND idem_key=$2`, account, idem))
}

func (s *Store) Get(ctx context.Context, id string) (Payment, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM bank_payments WHERE id=$1`, id))
}

// List returns the newest payments first.
func (s *Store) List(ctx context.Context, limit int) ([]Payment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM bank_payments ORDER BY created_at DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Open returns the payments the poller still has to look at: not final, and a
// settled one only while an ACH return is still possible.
func (s *Store) Open(ctx context.Context) ([]Payment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM bank_payments
		WHERE state IN ('received','awaiting_gate_approval','awaiting_bank_approval','bank_approved','sent')
		   OR (state='settled' AND sent_at > $1)
		ORDER BY created_at, id`, s.at().Add(-ReturnWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Events returns a payment's chain in order.
func (s *Store) Events(ctx context.Context, id string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT payment_id, seq, from_state, to_state, actor, detail, at FROM bank_payment_events WHERE payment_id=$1 ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var f, t string
		if err := rows.Scan(&e.PaymentID, &e.Seq, &f, &t, &e.Actor, &e.Detail, &e.At); err != nil {
			return nil, err
		}
		e.From, e.To = State(f), State(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Change is what a transition may also set on the row.
type Change struct {
	Rule      string
	RequestID string
	TxnID     string
	HoldID    string
}

// Transition moves a payment to `to` and appends the event and the audit row
// in one transaction. Already in `to`: no change, changed=false. A step the
// state machine does not allow is ErrIllegal.
func (s *Store) Transition(ctx context.Context, id string, to State, actor string, detail map[string]any, ch Change) (Payment, bool, error) {
	return s.apply(ctx, id, to, actor, detail, ch, true, nil)
}

// Money is work on the ledger and the budget that must commit with a state
// change, in the caller's transaction (it takes no connection of its own). It
// sees the locked row and may change p.HoldID.
type Money func(ctx context.Context, tx pgx.Tx, p *Payment) error

// TransitionWith is Transition with its money: the ledger effect, the state
// row, the event and the audit row commit together or not at all.
func (s *Store) TransitionWith(ctx context.Context, id string, to State, actor string, detail map[string]any, ch Change, m Money) (Payment, bool, error) {
	return s.apply(ctx, id, to, actor, detail, ch, true, m)
}

// NoteWith is Note with money in the same transaction.
func (s *Store) NoteWith(ctx context.Context, id, actor string, detail map[string]any, ch Change, m Money) (Payment, error) {
	p, _, err := s.apply(ctx, id, "", actor, detail, ch, false, m)
	return p, err
}

// Note appends an event that does not change the state (the request was
// submitted, a review arrived) and may set the bank ids.
func (s *Store) Note(ctx context.Context, id, actor string, detail map[string]any, ch Change) (Payment, error) {
	p, _, err := s.apply(ctx, id, "", actor, detail, ch, false, nil)
	return p, err
}

func (s *Store) apply(ctx context.Context, id string, to State, actor string, detail map[string]any, ch Change, move bool, m Money) (Payment, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Payment{}, false, err
	}
	defer tx.Rollback(ctx)
	p, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM bank_payments WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return Payment{}, false, err
	}
	from := p.State
	if !move {
		to = from
	} else if from == to {
		return p, false, nil
	} else if !allowed(from, to) {
		return p, false, fmt.Errorf("%w: %s -> %s", ErrIllegal, from, to)
	}
	if m != nil {
		if err := m(ctx, tx, &p); err != nil {
			return Payment{}, false, err
		}
	}
	now := s.at()
	p.State, p.UpdatedAt = to, now
	if ch.Rule != "" {
		p.Rule = ch.Rule
	}
	if ch.RequestID != "" {
		p.RequestID = ch.RequestID
	}
	if ch.TxnID != "" {
		p.TxnID = ch.TxnID
	}
	if ch.HoldID != "" {
		p.HoldID = ch.HoldID
	}
	if to == Sent && p.SentAt == nil {
		p.SentAt = &now
	}
	if _, err := tx.Exec(ctx, `UPDATE bank_payments SET state=$2, rule=$3, mercury_request_id=$4, mercury_txn_id=$5, updated_at=$6, sent_at=$7, hold_id=$8 WHERE id=$1`,
		id, string(p.State), p.Rule, p.RequestID, p.TxnID, p.UpdatedAt, p.SentAt, p.HoldID); err != nil {
		return Payment{}, false, err
	}
	if err := writeEvent(ctx, tx, p, from, to, actor, js(detail), now); err != nil {
		return Payment{}, false, err
	}
	return p, true, tx.Commit(ctx)
}

// HoldStatus returns the ledger status of a hold ("active", "settled",
// "released"), or "" when there is no such hold.
func (s *Store) HoldStatus(ctx context.Context, holdID string) (string, error) {
	if holdID == "" {
		return "", nil
	}
	var st string
	err := s.pool.QueryRow(ctx, `SELECT status FROM holds WHERE id=$1`, holdID).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return st, err
}
