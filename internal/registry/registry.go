// Package registry is the AI Registry: the catalogue of models, tools and
// agents that are allowed to exist, each with an owner, a purpose and a status.
//
// The registry governs when the gateway runs with --registry-enforce: a model
// or tool request is refused (rule registry_unapproved) unless its entry here
// has status approved. Status approved passes; proposed, deprecated, blocked
// and "no entry" do not. Every creation, status change and removal is written
// to registry_changes in the same transaction.
package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("registry: no such entry")
	ErrBadKind   = errors.New("registry: kind must be model, tool or agent")
	ErrBadStatus = errors.New("registry: status must be approved, proposed, deprecated or blocked")
	ErrBadEntry  = errors.New("registry: name, owner and purpose are required")
	ErrBadTier   = errors.New("registry: risk tier must be low, medium or high")
)

var (
	kinds    = map[string]bool{"model": true, "tool": true, "agent": true}
	statuses = map[string]bool{"approved": true, "proposed": true, "deprecated": true, "blocked": true}
	tiers    = map[string]bool{"low": true, "medium": true, "high": true}
)

// DefaultActor is recorded in the history when the caller names no one.
const DefaultActor = "operator"

// Approved is the one status that passes the gateway's registry check.
const Approved = "approved"

// Entry is one catalogued item. Name is exact: a model name, "server/tool", or an agent id.
type Entry struct {
	Kind, Name, Owner, Purpose, Status, Notes string
	RiskTier                                  string // low | medium | high; "" on Upsert = medium for a new entry, unchanged otherwise
	Version                                   string // free text, e.g. a model snapshot or tool release; "" on Upsert = unchanged
	CreatedAt, UpdatedAt                      time.Time
	Actor                                     string // who is making this change (Upsert only; goes to the history)
}

// Change is one row of the history.
type Change struct {
	Kind, Name, From, To, Actor, Note string
	At                                time.Time
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

const cols = `kind, name, owner, purpose, status, notes, risk_tier, version, created_at, updated_at`

func scan(r pgx.Row) (Entry, error) {
	var e Entry
	err := r.Scan(&e.Kind, &e.Name, &e.Owner, &e.Purpose, &e.Status, &e.Notes, &e.RiskTier, &e.Version, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// Upsert adds an entry, or updates the owner, purpose, notes and status of an
// existing one. An empty status means proposed for a new entry and "unchanged"
// for an existing one; an empty risk tier or version means the same (medium and
// "" for a new entry). A creation or a status change writes one history row in
// the same transaction.
func (s *Store) Upsert(ctx context.Context, e Entry) (Entry, error) {
	if !kinds[e.Kind] {
		return Entry{}, ErrBadKind
	}
	if e.Status != "" && !statuses[e.Status] {
		return Entry{}, ErrBadStatus
	}
	if e.RiskTier != "" && !tiers[e.RiskTier] {
		return Entry{}, ErrBadTier
	}
	if e.Name == "" || e.Owner == "" || e.Purpose == "" {
		return Entry{}, ErrBadEntry
	}
	var out Entry
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var from string
		err := tx.QueryRow(ctx, `SELECT status FROM registry_entries WHERE kind = $1 AND name = $2 FOR UPDATE`, e.Kind, e.Name).Scan(&from)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `
			INSERT INTO registry_entries (kind, name, owner, purpose, status, notes, risk_tier, version)
			VALUES ($1, $2, $3, $4, COALESCE(NULLIF($5, ''), 'proposed'), $6, COALESCE(NULLIF($7, ''), 'medium'), $8)
			ON CONFLICT (kind, name) DO UPDATE SET owner = EXCLUDED.owner, purpose = EXCLUDED.purpose,
				status = COALESCE(NULLIF($5, ''), registry_entries.status), notes = EXCLUDED.notes,
				risk_tier = COALESCE(NULLIF($7, ''), registry_entries.risk_tier),
				version = COALESCE(NULLIF($8, ''), registry_entries.version), updated_at = now()
			RETURNING `+cols, e.Kind, e.Name, e.Owner, e.Purpose, e.Status, e.Notes, e.RiskTier, e.Version))
		if err != nil {
			return err
		}
		if from == "" || from != out.Status {
			return logChange(ctx, tx, e.Kind, e.Name, from, out.Status, e.Actor, "")
		}
		return nil
	})
	return out, err
}

func (s *Store) tx(ctx context.Context, f func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func logChange(ctx context.Context, tx pgx.Tx, kind, name, from, to, actor, note string) error {
	if actor == "" {
		actor = DefaultActor
	}
	_, err := tx.Exec(ctx, `INSERT INTO registry_changes (kind, name, from_status, to_status, actor, note) VALUES ($1, $2, $3, $4, $5, $6)`,
		kind, name, from, to, actor, note)
	return err
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

// SetStatus changes the status of an entry and records who did it. Setting the
// status it already has changes nothing and writes no history row.
func (s *Store) SetStatus(ctx context.Context, kind, name, status, actor string) (Entry, error) {
	if !kinds[kind] {
		return Entry{}, ErrBadKind
	}
	if !statuses[status] {
		return Entry{}, ErrBadStatus
	}
	var out Entry
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var from string
		if err := tx.QueryRow(ctx, `SELECT status FROM registry_entries WHERE kind = $1 AND name = $2 FOR UPDATE`, kind, name).Scan(&from); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if from == status {
			var err error
			out, err = scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM registry_entries WHERE kind = $1 AND name = $2`, kind, name))
			return err
		}
		var err error
		out, err = scan(tx.QueryRow(ctx, `UPDATE registry_entries SET status = $3, updated_at = now()
			WHERE kind = $1 AND name = $2 RETURNING `+cols, kind, name, status))
		if err != nil {
			return err
		}
		return logChange(ctx, tx, kind, name, from, status, actor, "")
	})
	return out, err
}

// Delete removes an entry and records the removal.
func (s *Store) Delete(ctx context.Context, kind, name, actor string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var from string
		err := tx.QueryRow(ctx, `DELETE FROM registry_entries WHERE kind = $1 AND name = $2 RETURNING status`, kind, name).Scan(&from)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return logChange(ctx, tx, kind, name, from, "", actor, "removed")
	})
}

// History lists status changes, newest first. With kind and name it lists one
// item; with both empty, everything (up to limit rows; 0 means 200).
func (s *Store) History(ctx context.Context, kind, name string, limit int) ([]Change, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `SELECT kind, name, from_status, to_status, actor, note, at FROM registry_changes
		WHERE ($1 = '' OR kind = $1) AND ($2 = '' OR name = $2) ORDER BY at DESC, id DESC LIMIT $3`, kind, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.Kind, &c.Name, &c.From, &c.To, &c.Actor, &c.Note, &c.At); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Status is the registry status of one item; "" when it has no entry.
func (s *Store) Status(ctx context.Context, kind, name string) (string, error) {
	var st string
	err := s.pool.QueryRow(ctx, `SELECT status FROM registry_entries WHERE kind = $1 AND name = $2`, kind, name).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return st, err
}

// ToolStatuses returns the status of every registered tool of one MCP server,
// keyed by the bare tool name (the part after "server/"), in one query. A tool
// that is missing from the map has no entry.
func (s *Store) ToolStatuses(ctx context.Context, server string) (map[string]string, error) {
	prefix := server + "/"
	rows, err := s.pool.Query(ctx, `SELECT name, status FROM registry_entries WHERE kind = 'tool' AND starts_with(name, $1)`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, st string
		if err := rows.Scan(&n, &st); err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(n, prefix)] = st
	}
	return out, rows.Err()
}

// SeedItem is something to add as approved when it has no entry yet.
type SeedItem struct{ Kind, Name string }

// Seed adds every item that has no entry as approved (owner "operator",
// purpose "seeded from live config") with a history row. Existing entries are
// never touched. It returns the items it added; with dryRun it adds nothing and
// returns what it would add.
func (s *Store) Seed(ctx context.Context, items []SeedItem, actor string, dryRun bool) ([]SeedItem, error) {
	var added []SeedItem
	err := s.tx(ctx, func(tx pgx.Tx) error {
		for _, it := range items {
			if !kinds[it.Kind] || it.Name == "" {
				return ErrBadEntry
			}
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM registry_entries WHERE kind = $1 AND name = $2)`, it.Kind, it.Name).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			added = append(added, it)
			if dryRun {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO registry_entries (kind, name, owner, purpose, status)
				VALUES ($1, $2, $3, 'seeded from live config', 'approved')`, it.Kind, it.Name, DefaultActor); err != nil {
				return err
			}
			if err := logChange(ctx, tx, it.Kind, it.Name, "", "approved", actor, "seeded"); err != nil {
				return err
			}
		}
		return nil
	})
	return added, err
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
	SELECT 'tool', model, at FROM audit_events WHERE ` + toolFilter + ` AND model <> '' AND at >= $1
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
