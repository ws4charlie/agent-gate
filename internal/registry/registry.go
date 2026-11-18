// Package registry is the AI Registry: the catalogue of models, tools and
// agents that are allowed to exist, each with an owner, a purpose and a status.
// It records and reports. Nothing here refuses a request.
package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("registry: no such entry")
	ErrBadKind   = errors.New("registry: kind must be model, tool or agent")
	ErrBadStatus = errors.New("registry: status must be approved, proposed, deprecated or blocked")
	ErrBadEntry  = errors.New("registry: name, owner and purpose are required")
)

var (
	kinds    = map[string]bool{"model": true, "tool": true, "agent": true}
	statuses = map[string]bool{"approved": true, "proposed": true, "deprecated": true, "blocked": true}
)

// Entry is one catalogued item. Name is exact: a model name, "server/tool", or an agent id.
type Entry struct {
	Kind, Name, Owner, Purpose, Status, Notes string
	CreatedAt, UpdatedAt                      time.Time
}

// InUse is something seen in the audit log since a point in time.
type InUse struct {
	Kind     string
	Name     string
	LastSeen time.Time
	Calls    int64
	Status   string // registry status; "" when not registered
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const cols = `kind, name, owner, purpose, status, notes, created_at, updated_at`

func scan(r pgx.Row) (Entry, error) {
	var e Entry
	err := r.Scan(&e.Kind, &e.Name, &e.Owner, &e.Purpose, &e.Status, &e.Notes, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// Upsert adds an entry, or updates the owner, purpose, status and notes of an
// existing one. An empty status means proposed for a new entry and "unchanged"
// for an existing one.
func (s *Store) Upsert(ctx context.Context, e Entry) (Entry, error) {
	if !kinds[e.Kind] {
		return Entry{}, ErrBadKind
	}
	if e.Status != "" && !statuses[e.Status] {
		return Entry{}, ErrBadStatus
	}
	if e.Name == "" || e.Owner == "" || e.Purpose == "" {
		return Entry{}, ErrBadEntry
	}
	return scan(s.pool.QueryRow(ctx, `
		INSERT INTO registry_entries (kind, name, owner, purpose, status, notes)
		VALUES ($1, $2, $3, $4, COALESCE(NULLIF($5, ''), 'proposed'), $6)
		ON CONFLICT (kind, name) DO UPDATE SET owner = EXCLUDED.owner, purpose = EXCLUDED.purpose,
			status = COALESCE(NULLIF($5, ''), registry_entries.status), notes = EXCLUDED.notes, updated_at = now()
		RETURNING `+cols, e.Kind, e.Name, e.Owner, e.Purpose, e.Status, e.Notes))
}

func (s *Store) Get(ctx context.Context, kind, name string) (Entry, error) {
	e, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM registry_entries WHERE kind = $1 AND name = $2`, kind, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	return e, err
}

// List returns entries, optionally of one kind ("" for all), by kind then name.
func (s *Store) List(ctx context.Context, kind string) ([]Entry, error) {
	if kind != "" && !kinds[kind] {
		return nil, ErrBadKind
	}
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM registry_entries WHERE $1 = '' OR kind = $1 ORDER BY kind, name`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) SetStatus(ctx context.Context, kind, name, status string) (Entry, error) {
	if !kinds[kind] {
		return Entry{}, ErrBadKind
	}
	if !statuses[status] {
		return Entry{}, ErrBadStatus
	}
	e, err := scan(s.pool.QueryRow(ctx, `UPDATE registry_entries SET status = $3, updated_at = now()
		WHERE kind = $1 AND name = $2 RETURNING `+cols, kind, name, status))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	return e, err
}

func (s *Store) Delete(ctx context.Context, kind, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM registry_entries WHERE kind = $1 AND name = $2`, kind, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// toolFilter is the console's definition of a real tool call (console/overview.go
// toolFilter): a paid call, or an MCP request that is not protocol traffic such
// as initialize, ping or tools/list. Keep the two in step.
const toolFilter = `(kind = 'payment' OR (kind = 'mcp' AND model <> '' AND model <> 'initialize' AND model <> 'ping'
	AND split_part(model, '/', 1) NOT IN ('tools', 'resources', 'prompts', 'notifications', 'server', 'completion',
		'logging', 'sampling', 'roots', 'elicitation', 'tasks')))`

// inUse is every distinct model, tool and agent seen in the audit log since a
// time, with its registry status ("" when absent). Models come from model
// calls, tools from tool calls, agents from agent_id.
const inUse = `
WITH seen AS (
	SELECT 'model' AS kind, model AS name, at FROM audit_events WHERE kind = 'model' AND model <> '' AND at >= $1
	UNION ALL
	SELECT 'tool', model, at FROM audit_events WHERE ` + toolFilter + ` AND at >= $1
	UNION ALL
	SELECT 'agent', agent_id, at FROM audit_events WHERE agent_id <> '' AND at >= $1
), agg AS (
	SELECT kind, name, max(at) AS last_seen, count(*) AS calls FROM seen GROUP BY kind, name
)
SELECT agg.kind, agg.name, agg.last_seen, agg.calls, COALESCE(r.status, '')
FROM agg LEFT JOIN registry_entries r ON r.kind = agg.kind AND r.name = agg.name
WHERE %s ORDER BY agg.kind, agg.calls DESC, agg.name`

func (s *Store) inUse(ctx context.Context, since time.Time, where string) ([]InUse, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(inUse, where), since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InUse{}
	for rows.Next() {
		var u InUse
		if err := rows.Scan(&u.Kind, &u.Name, &u.LastSeen, &u.Calls, &u.Status); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Unregistered lists what the audit log shows in use since a time that has no
// registry entry at all.
func (s *Store) Unregistered(ctx context.Context, since time.Time) ([]InUse, error) {
	return s.inUse(ctx, since, `r.name IS NULL`)
}

// InUseButNotApproved lists what is in use and registered as deprecated or blocked.
func (s *Store) InUseButNotApproved(ctx context.Context, since time.Time) ([]InUse, error) {
	return s.inUse(ctx, since, `r.status IN ('deprecated', 'blocked')`)
}
