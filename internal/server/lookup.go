package server

import (
	"context"
	"errors"

	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// RouteLookup sends payment holds (provider "x402") to the chain and every
// other hold to the model-provider lookup.
type RouteLookup struct {
	Payments ledger.UsageLookup // may be nil when payments are not configured
	Default  ledger.UsageLookup
}

var _ ledger.UsageLookup = (*RouteLookup)(nil)

func (r *RouteLookup) Lookup(ctx context.Context, provider, id string) (ledger.Usage, bool, error) {
	if provider == x402.Provider {
		if r.Payments == nil {
			// Releasing would refund a possible transfer: stay active and count an error.
			return ledger.Usage{}, false, errors.New("payment hold found but no chain access configured")
		}
		return r.Payments.Lookup(ctx, provider, id)
	}
	return r.Default.Lookup(ctx, provider, id)
}
