// Package approval is the queue for requests a human must approve: anything
// over an agent's approval threshold.
//
// The life of one entry:
//
//	request over threshold --> pending   (the hold stays: the funds are locked)
//	pending  --Allow--> approved --Consume--> consumed   (the replay runs, once)
//	pending  --Deny---> denied                           (hold released)
//	pending or approved, past expires_at --> expired     (hold released)
//
// Every transition is one conditional UPDATE on the status, so two people
// pressing Allow and Deny, or an approval racing its expiry, resolve to one
// outcome.
//
// An approval authorizes one exact request, never "something like it": the row
// stores a fingerprint of kind, model or tool, payee, amount, network, task and
// request body, and a replay whose fingerprint differs is refused. The
// approver is whoever has direct access to the database (the CLI); there is no
// HTTP route to approve, so an agent credential cannot approve itself.
package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
)

type Status string

const (
	Pending  Status = "pending"
	Approved Status = "approved"
	Denied   Status = "denied"
	Expired  Status = "expired"
	Consumed Status = "consumed"
)

// DefaultTTL is how long a request may wait for a decision, and how long an
// approval stays usable.
const DefaultTTL = 15 * time.Minute

var (
	ErrNotFound   = errors.New("approval: no such approval")
	ErrNotPending = errors.New("approval: not pending")
	ErrExpired    = errors.New("approval: expired")
)

type Approval struct {
	ID, AgentID, AccountID, IdemKey, RequestID string
	Kind, Subject, Payee, TaskID               string
	Amount                                     money.Nanos
	Fingerprint, HoldID                        string
	Status                                     Status
	DecidedBy                                  string
	DecidedAt                                  *time.Time
	CreatedAt, ExpiresAt                       time.Time
}

// Fingerprint hashes the fields that define a request. Fields are
// length-prefixed so ("ab","c") and ("a","bc") differ.
func Fingerprint(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		h.Write([]byte(strconv.Itoa(len(f)) + ":"))
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, now: time.Now} }

// SetClock is for tests.
func (s *Store) SetClock(f func() time.Time) { s.now = f }

const cols = `id, agent_id, account_id, idem_key, request_id, kind, subject, payee, amount_nanos, task_id,
	fingerprint, hold_id, status, decided_by, decided_at, created_at, expires_at`

func scan(row pgx.Row) (Approval, error) {
	var a Approval
	var st string
	err := row.Scan(&a.ID, &a.AgentID, &a.AccountID, &a.IdemKey, &a.RequestID, &a.Kind, &a.Subject, &a.Payee, &a.Amount,
		&a.TaskID, &a.Fingerprint, &a.HoldID, &st, &a.DecidedBy, &a.DecidedAt, &a.CreatedAt, &a.ExpiresAt)
	a.Status = Status(st)
	return a, err
}

// Create queues a pending approval for (agent, idem key). If one exists, it is
// returned with created=false: the same request asking twice is one entry.
func (s *Store) Create(ctx context.Context, a Approval, ttl time.Duration) (Approval, bool, error) {
	now := s.now().UTC().Truncate(time.Microsecond)
	got, err := scan(s.pool.QueryRow(ctx, `INSERT INTO approvals (agent_id, account_id, idem_key, request_id, kind, subject, payee,
		amount_nanos, task_id, fingerprint, hold_id, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (agent_id, idem_key) DO NOTHING RETURNING `+cols,
		a.AgentID, a.AccountID, a.IdemKey, a.RequestID, a.Kind, a.Subject, a.Payee, int64(a.Amount), a.TaskID,
		a.Fingerprint, a.HoldID, now, now.Add(ttl)))
	if err == nil {
		return got, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, false, err
	}
	cur, found, ferr := s.Find(ctx, a.AgentID, a.IdemKey)
	if ferr != nil || !found {
		return Approval{}, false, fmt.Errorf("approval: create: %v", ferr)
	}
	return cur, false, nil
}

// Find looks an approval up by the request's identity.
func (s *Store) Find(ctx context.Context, agentID, idemKey string) (Approval, bool, error) {
	a, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM approvals WHERE agent_id=$1 AND idem_key=$2`, agentID, idemKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, false, nil
	}
	return a, err == nil, err
}

func (s *Store) Get(ctx context.Context, id string) (Approval, error) {
	a, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM approvals WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

// List returns approvals, newest first. An empty status lists all.
func (s *Store) List(ctx context.Context, status Status) ([]Approval, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM approvals WHERE ($1 = '' OR status = $1) ORDER BY created_at DESC, id`, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// decide moves pending -> to, recording who decided.
func (s *Store) decide(ctx context.Context, id, by string, to Status) (Approval, error) {
	if by == "" {
		return Approval{}, errors.New("approval: the approver must be named")
	}
	a, err := scan(s.pool.QueryRow(ctx, `UPDATE approvals SET status=$2, decided_by=$3, decided_at=$4
		WHERE id=$1 AND status='pending' AND expires_at > $4 RETURNING `+cols, id, string(to), by, s.now().UTC().Truncate(time.Microsecond)))
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, err
	}
	cur, gerr := s.Get(ctx, id)
	if gerr != nil {
		return Approval{}, gerr
	}
	if cur.Status == Pending { // still pending, so the time ran out
		return cur, ErrExpired
	}
	return cur, fmt.Errorf("%w: it is %s", ErrNotPending, cur.Status)
}

// Consume spends an approved approval, once. It returns false if the entry is
// not approved (anymore): never approved, denied, expired, or already used.
// This is what makes a replay one execution.
func (s *Store) Consume(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE approvals SET status='consumed'
		WHERE id=$1 AND status='approved' AND expires_at > $2`, id, s.now().UTC().Truncate(time.Microsecond))
	return tag.RowsAffected() == 1, err
}

// ExpireDue marks pending and approved entries past their expiry as expired
// and returns them, so the caller can release their holds.
func (s *Store) ExpireDue(ctx context.Context) ([]Approval, error) {
	rows, err := s.pool.Query(ctx, `UPDATE approvals SET status='expired'
		WHERE status IN ('pending','approved') AND expires_at <= $1 RETURNING `+cols, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Queue ties the store to the ledger: deciding and expiring also settle the
// money side (the hold).
type Queue struct {
	*Store
	Ledger ledger.Ledger
}

// ReleaseKey is the idempotency key every release of a hold uses, so the
// request handler, deny and expiry all release the same hold the same way.
func ReleaseKey(holdID string) string { return "release:" + holdID }

// Allow approves a pending request. The hold stays; the agent replays the
// request with the same Idempotency-Key and it runs.
func (q *Queue) Allow(ctx context.Context, id, by string) (Approval, error) {
	a, err := q.decide(ctx, id, by, Approved)
	if errors.Is(err, ErrExpired) {
		_, _ = q.Sweep(ctx) // make the stored state say so, and release the hold
	}
	return a, err
}

// Deny refuses a pending request and releases its hold.
func (q *Queue) Deny(ctx context.Context, id, by string) (Approval, error) {
	a, err := q.decide(ctx, id, by, Denied)
	if err != nil {
		if errors.Is(err, ErrExpired) {
			_, _ = q.Sweep(ctx)
		}
		return a, err
	}
	q.release(ctx, a, "approval denied")
	return a, nil
}

// Sweep expires everything past its time and releases the holds.
func (q *Queue) Sweep(ctx context.Context) (int, error) {
	due, err := q.ExpireDue(ctx)
	for _, a := range due {
		q.release(ctx, a, "approval expired")
	}
	return len(due), err
}

// release returns the funds. A failure is not fatal: the hold's own expiry and
// the reconciler (I6) close it later.
func (q *Queue) release(ctx context.Context, a Approval, reason string) {
	_, _ = q.Ledger.Release(context.WithoutCancel(ctx), a.HoldID, ReleaseKey(a.HoldID), reason)
}
