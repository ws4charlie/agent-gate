// Package run is the run layer: every request can carry a run id
// (X-AgentGate-Run), and everything with the same id is one unit of agent
// work. A run lists its steps (model calls, tool calls, payments, in order,
// with cost and status), shows what it cost, and can be killed as a whole.
//
// A run adds no new money path. Its spend is a sum over the ledger's own
// entries, reached through the hold id of each step, so the number can never
// disagree with ledger.Audit (I2).
//
// Kill by run is I12 for a smaller unit than an agent: the next request that
// names the run is refused, and the calls it has in flight are cancelled (the
// server keeps the cancel funcs, as it does for agents).
package run

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// Status of a run.
const (
	Active = "active"
	Killed = "killed"
)

// Step kinds.
const (
	KindModel   = "model"
	KindTool    = "tool"
	KindPayment = "payment"
	KindMCP     = "mcp" // any other MCP request that is passed through (resources, prompts, ...)
)

var (
	ErrNotFound  = errors.New("run: not found")
	ErrForbidden = errors.New("run: belongs to another agent")
	ErrBadID     = errors.New("run: id must be 1-64 characters of letters, digits, . _ : -")
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

type Run struct {
	ID, AgentID, AccountID, Status string
	CreatedAt                      time.Time
	KilledAt                       *time.Time
}

type Step struct {
	ID                                           int64
	RunID, Kind, Name, RequestID, HoldID, Status string
	Cost                                         money.Nanos
	StartedAt                                    time.Time
	FinishedAt                                   *time.Time
}

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// NewID makes a run id for a request that came without one.
func NewID() string { return "run_" + uuid.NewString() }

// Open returns the run for a request. An id the client supplied that does not
// exist yet is created for this agent; one that exists must belong to this
// agent, or another agent's work could be steered by guessing its id.
func (s *Store) Open(ctx context.Context, id, agentID, accountID string) (Run, error) {
	if !idRe.MatchString(id) {
		return Run{}, ErrBadID
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO runs (id, agent_id, account_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
		id, agentID, accountID); err != nil {
		return Run{}, err
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if r.AgentID != agentID {
		return Run{}, ErrForbidden
	}
	return r, nil
}

func (s *Store) Get(ctx context.Context, id string) (Run, error) {
	var r Run
	err := s.pool.QueryRow(ctx, `SELECT id, agent_id, account_id, status, created_at, killed_at FROM runs WHERE id=$1`, id).
		Scan(&r.ID, &r.AgentID, &r.AccountID, &r.Status, &r.CreatedAt, &r.KilledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// List returns the newest runs first.
func (s *Store) List(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, agent_id, account_id, status, created_at, killed_at FROM runs
		ORDER BY created_at DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.AgentID, &r.AccountID, &r.Status, &r.CreatedAt, &r.KilledAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Begin records a step when its request is admitted. started_at is cut to
// microseconds, Postgres' precision, so a value read back equals the value
// written on every OS.
func (s *Store) Begin(ctx context.Context, runID, kind, requestID string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO run_steps (run_id, kind, request_id, started_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT (request_id) DO NOTHING`, runID, kind, requestID, at.UTC().Truncate(time.Microsecond))
	return err
}

// End closes the step of a request with what is known at the end: its name
// (model or tool), the hold that funded it, its final audit status and cost.
func (s *Store) End(ctx context.Context, requestID, name, holdID, status string, cost money.Nanos, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE run_steps SET name=$2, hold_id=$3, status=$4, cost_nanos=$5, finished_at=$6
		WHERE request_id=$1`, requestID, name, holdID, status, int64(cost), at.UTC().Truncate(time.Microsecond))
	return err
}

// Steps returns a run's steps in the order they were admitted.
func (s *Store) Steps(ctx context.Context, runID string) ([]Step, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, run_id, kind, name, request_id, hold_id, status, cost_nanos, started_at, finished_at
		FROM run_steps WHERE run_id=$1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.ID, &st.RunID, &st.Kind, &st.Name, &st.RequestID, &st.HoldID, &st.Status, &st.Cost,
			&st.StartedAt, &st.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Spend is what the run has cost, from the ledger: the sum of the settle
// entries of the holds its steps used. Reserved-but-open money is not spend.
func (s *Store) Spend(ctx context.Context, runID string) (money.Nanos, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(e.amount_nanos), 0)::bigint FROM ledger_entries e
		WHERE e.kind = 'settle' AND e.hold_id IN (SELECT hold_id FROM run_steps WHERE run_id=$1 AND hold_id <> '')`, runID).Scan(&n)
	return money.Nanos(n), err
}

// Kill marks a run killed. It is idempotent. The next request that names the
// run is refused, and a gateway process cancels the run's in-flight calls on
// its next kill poll.
func (s *Store) Kill(ctx context.Context, id string) (Run, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET status='killed', killed_at=now() WHERE id=$1 AND status='active'`, id); err != nil {
		return Run{}, err
	}
	return s.Get(ctx, id)
}

// NotActive returns those of ids whose run is killed.
func (s *Store) NotActive(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM runs WHERE id = ANY($1) AND status <> 'active'`, ids)
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

// Summary is one line per run for `run list`.
func (r Run) String() string { return fmt.Sprintf("%s  %s  agent=%s", r.ID, r.Status, r.AgentID) }
