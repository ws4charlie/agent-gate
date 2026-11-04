package server_test

import (
	"net/http"

	"github.com/ws4charlie/agent-gate/internal/config"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/pricing"
	"github.com/ws4charlie/agent-gate/internal/provider"
)

type pricing_Price = pricing.Price

type provider_lookup struct{ e *env }

func (p *provider_lookup) l() ledger.UsageLookup {
	_ = config.ProviderKey
	return &provider.GenerationLookup{Client: http.DefaultClient, Providers: p.e.srv.Providers.Providers,
		Key: func(string) string { return upstreamKey }}
}
