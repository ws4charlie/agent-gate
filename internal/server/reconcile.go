package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
)

// ChargeCloser is the part of agents.Store the reconciler needs.
type ChargeCloser interface {
	CloseCharge(ctx context.Context, holdID string, actual money.Nanos) error
}

// Reconcile runs one reconciler pass (I6) and writes one audit row per hold it
// closed: reconciled_settled when usage was found, reconciled_unmetered when
// not. For a request whose process died, that row is the only record of it.
// `serve` and the `reconcile` command both call this one function.
//
// The audit row's kind is the hold's own kind (model, tool, payment), read from
// the holds row: ledger.Outcome does not carry it and internal/ledger is not ours to change.
//
// charges (may be nil) is where the budget windows get corrected: the reconciler
// closes a hold the request handler never closed, and without this the agent's
// daily and task windows would keep the estimate for a hold that was released
// or settled for less. It is called for every outcome, with what the ledger
// really charged (0 for a release).
func Reconcile(ctx context.Context, pool *pgxpool.Pool, led *ledger.Postgres, lookup ledger.UsageLookup, store *audit.Store, charges ChargeCloser, now time.Time) (ledger.Report, error) {
	rep, err := led.Reconcile(ctx, lookup, now)
	// Outcomes are already committed in the ledger, so audit them even if the pass ended with an error.
	for _, o := range rep.Outcomes {
		ev := audit.Event{RequestID: "reconcile:" + o.HoldID, AccountID: o.AccountID, Kind: holdAuditKind(ctx, pool, o.HoldID), At: now}
		x := audit.Extra{HoldID: o.HoldID}
		if o.Result == "settled" {
			ev.Status, ev.Cost, ev.CostSource = audit.StatusReconciledSettled, o.Amount, o.Source
		} else {
			ev.Status, ev.Err = audit.StatusReconciledUnmetered, "unmetered"
			x.Unmetered, x.UnmeteredNanos = true, o.Amount
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if charges != nil {
			actual := money.Nanos(0)
			if o.Result == "settled" {
				actual = o.Amount
			}
			if cerr := charges.CloseCharge(wctx, o.HoldID, actual); cerr != nil && err == nil {
				err = cerr
			}
		}
		if werr := store.Write(wctx, ev, x); werr != nil && err == nil {
			err = werr
		}
		cancel()
	}
	return rep, err
}

// holdAuditKind maps a hold's ledger kind to the audit kind. A tool hold is
// "mcp" like the audit rows of the MCP route; an unreadable row falls back to
// "model", the original behaviour.
func holdAuditKind(ctx context.Context, pool *pgxpool.Pool, holdID string) string {
	var k string
	if pool == nil || pool.QueryRow(ctx, `SELECT kind FROM holds WHERE id=$1`, holdID).Scan(&k) != nil {
		return "model"
	}
	switch ledger.Kind(k) {
	case ledger.KindTool:
		return "mcp"
	case ledger.KindPayment:
		return "payment"
	}
	return "model"
}
