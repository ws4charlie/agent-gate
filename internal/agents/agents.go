// Package agents stores agent identities: one credential, one scope and one
// ledger account each. It also keeps the spend counters the policy engine
// reads and the atomic charge that enforces them.
//
// Credentials follow I9: the secret is 256 random bits, shown once, and only
// its SHA-256 is stored.
package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

const keyPrefix = "ag_"

var (
	ErrUnauthorized = errors.New("agents: unauthorized")
	ErrNotFound     = errors.New("agents: no such agent")
	ErrBadName      = errors.New("agents: name must be 1-64 characters of letters, digits, '.', '_' or '-'")
	ErrBadState     = errors.New("agents: wrong state for this change")
	ErrNameTaken    = errors.New("agents: another agent has this name")
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Agent is one stored identity. The secret is never part of it.
type Agent struct {
	ID, AccountID, Name string
	Scope               policy.Scope
	Status              policy.Status
	CreatedAt           time.Time
}

type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, now: time.Now} }

// SetClock is for tests (day windows and the velocity minute).
func (s *Store) SetClock(f func() time.Time) { s.now = f }

// Create makes an agent and returns its secret, the only time it exists in
// plaintext.
func (s *Store) Create(ctx context.Context, accountID, name string, scope policy.Scope) (secret string, a Agent, err error) {
	if !nameRE.MatchString(name) {
		return "", Agent{}, ErrBadName
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", Agent{}, err
	}
	secret = keyPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	h := sha256.Sum256([]byte(secret))
	sj, err := json.Marshal(scope)
	if err != nil {
		return "", Agent{}, err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO agents (account_id, name, key_hash, scope) VALUES ($1,$2,$3,$4)
		RETURNING id, created_at`, accountID, name, h[:], sj).Scan(&a.ID, &a.CreatedAt)
	if err != nil {
		return "", Agent{}, fmt.Errorf("create agent: %w", err)
	}
	a.AccountID, a.Name, a.Scope, a.Status = accountID, name, scope, policy.StatusActive
	return secret, a, nil
}

const agentCols = `id, account_id, name, scope, status, created_at`

func scan(row pgx.Row) (Agent, error) {
	var a Agent
	var sj []byte
	var st string
	if err := row.Scan(&a.ID, &a.AccountID, &a.Name, &sj, &st, &a.CreatedAt); err != nil {
		return Agent{}, err
	}
	a.Status = policy.Status(st)
	if err := json.Unmarshal(sj, &a.Scope); err != nil {
		return Agent{}, fmt.Errorf("agent %s: bad scope: %w", a.ID, err)
	}
	return a, nil
}

// Authenticate resolves a bearer secret to its agent. A killed or revoked
// agent still authenticates: the policy engine refuses it with a rule id, so
// the refusal is audited like any other decision.
//
// The lookup is an indexed equality on SHA-256(secret), not a constant-time
// compare: the secret has 256 bits of entropy and the database only ever sees
// its hash, so timing cannot walk toward a valid hash without a preimage.
func (s *Store) Authenticate(ctx context.Context, token string) (Agent, error) {
	if token == "" {
		return Agent{}, ErrUnauthorized
	}
	h := sha256.Sum256([]byte(token))
	a, err := scan(s.pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE key_hash=$1`, h[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrUnauthorized
	}
	return a, err
}

// Get finds an agent by id or by name.
func (s *Store) Get(ctx context.Context, ref string) (Agent, error) {
	a, err := scan(s.pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id=$1 OR name=$1`, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

func (s *Store) List(ctx context.Context) ([]Agent, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+agentCols+` FROM agents ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Revoke ends a credential for good. Idempotent.
func (s *Store) Revoke(ctx context.Context, ref string) (Agent, error) {
	return s.setStatus(ctx, ref, policy.StatusRevoked, "status <> 'revoked'")
}

// Kill is the kill switch (I12): the next request is refused, and a running
// gateway cancels the agent's in-flight calls. Idempotent. A revoked agent
// stays revoked.
func (s *Store) Kill(ctx context.Context, ref string) (Agent, error) {
	return s.setStatus(ctx, ref, policy.StatusKilled, "status = 'active'")
}

// Resume undoes a kill. A revoked agent cannot be resumed.
func (s *Store) Resume(ctx context.Context, ref string) (Agent, error) {
	return s.setStatus(ctx, ref, policy.StatusActive, "status = 'killed'")
}

// SetScope replaces an agent's scope. The next request is judged by the new
// one: the policy snapshot reads the scope on every request. Spend already
// counted in the budget windows stays, so lowering a budget below today's
// spend refuses the next request. A revoked agent cannot be changed.
func (s *Store) SetScope(ctx context.Context, ref string, scope policy.Scope) (Agent, error) {
	sj, err := json.Marshal(scope)
	if err != nil {
		return Agent{}, err
	}
	a, err := scan(s.pool.QueryRow(ctx, `UPDATE agents SET scope=$2
		WHERE (id=$1 OR name=$1) AND status <> 'revoked' RETURNING `+agentCols, ref, sj))
	if errors.Is(err, pgx.ErrNoRows) {
		cur, gerr := s.Get(ctx, ref)
		if gerr != nil {
			return Agent{}, gerr
		}
		return Agent{}, fmt.Errorf("%w: agent %s is %s", ErrBadState, cur.Name, cur.Status)
	}
	return a, err
}

// setStatus moves an agent to `to` when `from` (a SQL condition) holds. If the
// agent is already there, it returns it unchanged; any other state is ErrBadState.
// Rename changes an agent's name. The key, id, scope and history stay, so
// clients that hold the key keep working. A revoked agent that still holds the
// name gives it up: it becomes name.revoked.<id prefix>, since nothing can use
// a revoked agent and its history is tied to its id, not its name.
func (s *Store) Rename(ctx context.Context, ref, name string) (Agent, error) {
	if !nameRE.MatchString(name) {
		return Agent{}, ErrBadName
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Agent{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var oldID string
	err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE name=$1 AND status = 'revoked' AND id <> $2 FOR UPDATE`, name, ref).Scan(&oldID)
	switch {
	case err == nil:
		parked := revokedName(name, oldID)
		if _, err := tx.Exec(ctx, `UPDATE agents SET name=$2 WHERE id=$1`, oldID, parked); err != nil {
			return Agent{}, err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return Agent{}, err
	}
	a, err := scan(tx.QueryRow(ctx, `UPDATE agents SET name=$2
		WHERE (id=$1 OR name=$1) AND status <> 'revoked' RETURNING `+agentCols, ref, name))
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe) && pe.Code == "23505":
		return Agent{}, fmt.Errorf("%w: %s", ErrNameTaken, name)
	case errors.Is(err, pgx.ErrNoRows):
		_ = tx.Rollback(ctx)
		cur, gerr := s.Get(ctx, ref)
		if gerr != nil {
			return Agent{}, gerr
		}
		return Agent{}, fmt.Errorf("%w: agent %s is %s", ErrBadState, cur.Name, cur.Status)
	case err != nil:
		return Agent{}, err
	}
	return a, tx.Commit(ctx)
}

// revokedName is the name a revoked agent moves to when its name is reused.
func revokedName(name, id string) string {
	suffix := ".revoked." + strings.ReplaceAll(id, "-", "")
	if len(suffix) > 17 {
		suffix = suffix[:17]
	}
	if len(name)+len(suffix) > 64 {
		name = name[:64-len(suffix)]
	}
	return name + suffix
}

func (s *Store) setStatus(ctx context.Context, ref string, to policy.Status, from string) (Agent, error) {
	a, err := scan(s.pool.QueryRow(ctx, `UPDATE agents SET status=$2, status_at=now()
		WHERE (id=$1 OR name=$1) AND `+from+` RETURNING `+agentCols, ref, string(to)))
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, err
	}
	cur, gerr := s.Get(ctx, ref)
	if gerr != nil {
		return Agent{}, gerr
	}
	if cur.Status == to {
		return cur, nil
	}
	return Agent{}, fmt.Errorf("%w: agent %s is %s", ErrBadState, cur.Name, cur.Status)
}

// NotActive returns the ids, out of the given ones, that are killed or revoked.
// The gateway polls it to cancel in-flight calls (I12).
func (s *Store) NotActive(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM agents WHERE id = ANY($1) AND status <> 'active'`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- budget windows, velocity ----

const velocityWindow = time.Minute

func dayKey(t time.Time) string { return "day:" + t.UTC().Format("2006-01-02") }

// taskKey: a request without X-AgentGate-Task counts as the task "default", so
// leaving the header out cannot dodge a task budget.
func taskKey(id string) string {
	if id == "" {
		id = "default"
	}
	return "task:" + id
}

// Snapshot loads the agent's current status, scope and counters for one task.
// It is a read: Charge is what enforces.
func (s *Store) Snapshot(ctx context.Context, agentID, taskID string) (policy.Snapshot, error) {
	now := s.now()
	var sj []byte
	var st string
	var daily, task, vel int64
	err := s.pool.QueryRow(ctx, `SELECT a.status, a.scope,
		COALESCE((SELECT spent_nanos FROM budget_windows WHERE agent_id=a.id AND window_key=$2), 0),
		COALESCE((SELECT spent_nanos FROM budget_windows WHERE agent_id=a.id AND window_key=$3), 0),
		COALESCE((SELECT sum(amount_nanos) FROM budget_charges WHERE agent_id=a.id AND at > $4), 0)::bigint
		FROM agents a WHERE a.id=$1`, agentID, dayKey(now), taskKey(taskID), now.Add(-velocityWindow).UTC()).Scan(&st, &sj, &daily, &task, &vel)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy.Snapshot{}, ErrNotFound
	}
	if err != nil {
		return policy.Snapshot{}, err
	}
	snap := policy.Snapshot{AgentID: agentID, Status: policy.Status(st),
		DailySpent: money.Nanos(daily), TaskSpent: money.Nanos(task), VelocitySpent: money.Nanos(vel)}
	if err := json.Unmarshal(sj, &snap.Scope); err != nil {
		return policy.Snapshot{}, fmt.Errorf("agent %s: bad scope: %w", agentID, err)
	}
	return snap, nil
}

// ChargeReq counts one admitted request against the agent's windows.
type ChargeReq struct {
	HoldID  string // the funding hold; one charge per hold
	AgentID string
	TaskID  string
	Amount  money.Nanos
	Scope   policy.Scope // the limits to enforce
}

// Charge counts Amount against the daily window, the task window and the
// velocity minute, all or none, and returns "" on success or the rule id that
// refused ("budget_daily", "budget_task", "velocity").
//
// Each limit is one conditional statement, not a read followed by a write
// (I5 for budgets): the upsert only changes the row when the new total still
// fits, so N parallel requests cannot all pass a check that holds one. The
// velocity sum spans many rows, which a row-level conditional cannot guard, so
// that statement runs under a per-agent advisory lock taken in the same
// transaction.
func (s *Store) Charge(ctx context.Context, c ChargeReq) (string, error) {
	now := s.now()
	amt := int64(c.Amount)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	if c.Scope.VelocityPerMinute > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, c.AgentID); err != nil {
			return "", err
		}
	}
	window := func(key string, limit money.Nanos) (bool, error) {
		var spent int64
		err := tx.QueryRow(ctx, `INSERT INTO budget_windows (agent_id, window_key, spent_nanos)
			SELECT $1::text, $2::text, $3::bigint WHERE ($4::bigint = 0 OR $3::bigint <= $4::bigint)
			ON CONFLICT (agent_id, window_key) DO UPDATE
			SET spent_nanos = budget_windows.spent_nanos + $3::bigint
			WHERE $4::bigint = 0 OR budget_windows.spent_nanos + $3::bigint <= $4::bigint
			RETURNING spent_nanos`, c.AgentID, key, amt, int64(limit)).Scan(&spent)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	dk, tk := dayKey(now), taskKey(c.TaskID)
	if ok, err := window(dk, c.Scope.DailyBudget); err != nil || !ok {
		return policy.RuleBudgetDaily, err
	}
	if ok, err := window(tk, c.Scope.TaskBudget); err != nil || !ok {
		return policy.RuleBudgetTask, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO budget_charges (hold_id, agent_id, day_key, task_key, amount_nanos, at)
		SELECT $1::text, $2::text, $3::text, $4::text, $5::bigint, $6::timestamptz
		WHERE $7::bigint = 0 OR (SELECT COALESCE(sum(amount_nanos), 0) FROM budget_charges WHERE agent_id=$2 AND at > $8::timestamptz) + $5::bigint <= $7::bigint`,
		c.HoldID, c.AgentID, dk, tk, amt, now.UTC(), int64(c.Scope.VelocityPerMinute), now.Add(-velocityWindow).UTC())
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return policy.RuleVelocity, nil
	}
	return "", tx.Commit(ctx)
}

// CloseCharge corrects the windows once the real cost of a hold is known: the
// windows held the estimate, and now they hold `actual` (0 for a release). It
// runs once per hold, whoever gets there first (the request handler or the
// reconciler), and does nothing for a hold that was never charged.
func (s *Store) CloseCharge(ctx context.Context, holdID string, actual money.Nanos) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var agent, dk, tk string
	var old int64
	err = tx.QueryRow(ctx, `WITH o AS (SELECT hold_id, agent_id, day_key, task_key, amount_nanos AS old
			FROM budget_charges WHERE hold_id=$1 AND status='open' FOR UPDATE),
		u AS (UPDATE budget_charges c SET status='closed', amount_nanos=$2::bigint FROM o WHERE c.hold_id=o.hold_id RETURNING c.hold_id)
		SELECT o.agent_id, o.day_key, o.task_key, o.old FROM o JOIN u USING (hold_id)`, holdID, int64(actual)).Scan(&agent, &dk, &tk, &old)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // never charged, or already closed
	}
	if err != nil {
		return err
	}
	if delta := int64(actual) - old; delta != 0 {
		for _, k := range []string{dk, tk} {
			if _, err := tx.Exec(ctx, `UPDATE budget_windows SET spent_nanos = spent_nanos + $3::bigint
				WHERE agent_id=$1 AND window_key=$2`, agent, k, delta); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// ---- decision log ----

// Decision is one row of the decision log.
type Decision struct {
	RequestID, AgentID, AccountID, Kind, Subject, Payee, TaskID string
	Amount                                                      money.Nanos
	Verdict                                                     policy.Verdict
	Rule, Reason, ApprovalID                                    string
	At                                                          time.Time
}

// LogDecision appends one row. Every decision is logged, allow included.
func (s *Store) LogDecision(ctx context.Context, d Decision) error {
	if d.At.IsZero() {
		d.At = s.now()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO policy_decisions (request_id, agent_id, account_id, kind, subject, payee, task_id,
		amount_nanos, decision, rule, reason, approval_id, at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		d.RequestID, d.AgentID, d.AccountID, d.Kind, d.Subject, d.Payee, d.TaskID, int64(d.Amount),
		string(d.Verdict), d.Rule, d.Reason, d.ApprovalID, d.At.UTC().Truncate(time.Microsecond))
	return err
}

// Decisions returns the logged decisions of one request, oldest first.
func (s *Store) Decisions(ctx context.Context, requestID string) ([]Decision, error) {
	rows, err := s.pool.Query(ctx, `SELECT request_id, agent_id, account_id, kind, subject, payee, task_id, amount_nanos,
		decision, rule, reason, approval_id, at FROM policy_decisions WHERE request_id=$1 ORDER BY id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		var v string
		if err := rows.Scan(&d.RequestID, &d.AgentID, &d.AccountID, &d.Kind, &d.Subject, &d.Payee, &d.TaskID, &d.Amount,
			&v, &d.Rule, &d.Reason, &d.ApprovalID, &d.At); err != nil {
			return nil, err
		}
		d.Verdict = policy.Verdict(v)
		out = append(out, d)
	}
	return out, rows.Err()
}
