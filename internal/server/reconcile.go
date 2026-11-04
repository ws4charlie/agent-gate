package server

import (
	"context"
	"time"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
)

// Reconcile runs one reconciler pass (I6) and writes one audit row per hold it
// closed: reconciled_settled when usage was found, reconciled_unmetered when
// not. For a request whose process died, that row is the only record of it.
// `serve` and the `reconcile` command both call this one function.
func Reconcile(ctx context.Context, led *ledger.Postgres, lookup ledger.UsageLookup, store *audit.Store, now time.Time) (ledger.Report, error) {
	rep, err := led.Reconcile(ctx, lookup, now)
	// Outcomes are already committed in the ledger, so audit them even if the pass ended with an error.
	for _, o := range rep.Outcomes {
		ev := audit.Event{RequestID: "reconcile:" + o.HoldID, AccountID: o.AccountID, Kind: "model", At: now}
		x := audit.Extra{HoldID: o.HoldID}
		if o.Source == "chain" {
			ev.Kind = "payment" // settled from an on-chain fact (x402)
		}
		if o.Result == "settled" {
			ev.Status, ev.Cost, ev.CostSource = audit.StatusReconciledSettled, o.Amount, o.Source
		} else {
			ev.Status, ev.Err = audit.StatusReconciledUnmetered, "unmetered"
			x.Unmetered, x.UnmeteredNanos = true, o.Amount
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if werr := store.Write(wctx, ev, x); werr != nil && err == nil {
			err = werr
		}
		cancel()
	}
	return rep, err
}
