// Package audit is the append-only request log: one row per outcome, failures
// and refusals included (a trigger rejects UPDATE and DELETE).
package audit

import (
	"context"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/provider"
)

type Event struct {
	RequestID, AccountID, AgentID, RunID, Kind, Model, Provider, Status, CostSource string
	Usage                                                                           provider.Usage
	Cost                                                                            money.Nanos
	LatencyMs, OverheadMs                                                           int64
	Err                                                                             string
	At                                                                              time.Time
}

// Status values written by the gateway.
const (
	StatusOK                  = "ok"
	StatusUpstreamFailed      = "upstream_failed" // stream died or upstream errored after we reserved (I8)
	StatusUpstreamError       = "upstream_error"  // upstream answered non-2xx; nothing charged
	StatusClientDisconnected  = "client_disconnected"
	StatusDuplicate           = "duplicate_request"
	StatusInsufficientFunds   = "insufficient_funds"
	StatusUnknownModelPrice   = "unknown_model_price"
	StatusUnauthorized        = "unauthorized"
	StatusBadRequest          = "bad_request"
	StatusSettleLost          = "settle_lost_to_reconciler"
	StatusAccountInactive     = "account_inactive"
	StatusIdempotencyConflict = "idempotency_conflict"
	StatusInternal            = "internal_error"
	StatusUnroutable          = "unknown_model"
	SourceProvider            = "provider"
	SourcePriceTable          = "price_table"
	SourceEstimated           = "estimated"
	SourceReconciled          = "reconciled"
)

// Extra carries what the shared Event type has no field for.
type Extra struct {
	HoldID       string
	Unrecovered  money.Nanos // I7: part of the cost the ledger could not collect
	Unmetered    bool        // the hold was released with no usage found
	RequestBody  *string     // only when body storage is on; already redacted
	ResponseBody *string
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Write appends one row. Times are cut to microseconds, Postgres' precision,
// so a value read back equals the value written on every OS.
func (s *Store) Write(ctx context.Context, e Event, x Extra) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if e.Kind == "" {
		e.Kind = "model"
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_events (request_id, account_id, agent_id, run_id, kind, model, provider,
		status, cost_source, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, upstream_id,
		cost_nanos, unrecovered_nanos, unmetered, hold_id, latency_ms, overhead_ms, err, request_body, response_body, at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`,
		e.RequestID, e.AccountID, e.AgentID, e.RunID, e.Kind, e.Model, e.Provider, e.Status, e.CostSource,
		e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.CacheReadTokens, e.Usage.CacheWriteTokens, e.Usage.UpstreamID,
		int64(e.Cost), int64(x.Unrecovered), x.Unmetered, x.HoldID, e.LatencyMs, e.OverheadMs, e.Err,
		x.RequestBody, x.ResponseBody, e.At.UTC().Truncate(time.Microsecond))
	return err
}

// Row is a stored event, for tests and the report.
type Row struct {
	Event
	Extra
}

func (s *Store) ByRequest(ctx context.Context, requestID string) ([]Row, error) {
	rows, err := s.pool.Query(ctx, `SELECT request_id, account_id, agent_id, run_id, kind, model, provider, status, cost_source,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, upstream_id, cost_nanos, unrecovered_nanos,
		unmetered, hold_id, latency_ms, overhead_ms, err, request_body, response_body, at
		FROM audit_events WHERE request_id=$1 ORDER BY id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.RequestID, &r.AccountID, &r.AgentID, &r.RunID, &r.Kind, &r.Model, &r.Provider, &r.Status, &r.CostSource,
			&r.Usage.InputTokens, &r.Usage.OutputTokens, &r.Usage.CacheReadTokens, &r.Usage.CacheWriteTokens, &r.Usage.UpstreamID,
			&r.Cost, &r.Unrecovered, &r.Unmetered, &r.HoldID, &r.LatencyMs, &r.OverheadMs, &r.Err, &r.RequestBody, &r.ResponseBody, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- redaction (I9) ----

var (
	bearer  = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)
	skKey   = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`)
	agKey   = regexp.MustCompile(`\bag_[A-Za-z0-9_-]{16,}`)
	xapikey = regexp.MustCompile(`(?i)("?x-api-key"?\s*[:=]\s*"?)[^"\s,}]+`)
)

// Redact masks bearer tokens and key-shaped strings before a body is stored.
// It is best effort by design: storing bodies is opt-in, and the default (no
// bodies) is the real protection.
func Redact(s string) string {
	s = bearer.ReplaceAllString(s, "Bearer [REDACTED]")
	s = skKey.ReplaceAllString(s, "sk-[REDACTED]")
	s = agKey.ReplaceAllString(s, "ag_[REDACTED]")
	return xapikey.ReplaceAllString(s, "${1}[REDACTED]")
}
