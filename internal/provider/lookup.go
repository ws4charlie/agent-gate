package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
)

// GenerationLookup is the ledger.UsageLookup for OpenRouter-style providers:
// GET <base>/generation?id=... returns the billed cost of one call (I6).
// A known provider without such an endpoint cannot say what a call cost, so
// the answer is "found, charge the whole hold" (I6): unbilled spend is worse
// than an overcharge capped at the hold.
type GenerationLookup struct {
	Client    *http.Client
	Providers map[string]ProviderCfg
	Key       func(provider string) string
}

var _ ledger.UsageLookup = (*GenerationLookup)(nil)

func (g *GenerationLookup) Lookup(ctx context.Context, name, upstreamID string) (ledger.Usage, bool, error) {
	cfg, ok := g.Providers[name]
	if !ok || upstreamID == "" {
		return ledger.Usage{}, false, nil
	}
	if !cfg.HasUsageLookup() {
		return ledger.Usage{ChargeHold: true, Source: "hold_no_lookup"}, true, nil
	}
	u := strings.TrimRight(cfg.BaseURL, "/") + "/generation?id=" + url.QueryEscape(upstreamID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ledger.Usage{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+g.Key(name))
	resp, err := g.Client.Do(req)
	if err != nil {
		return ledger.Usage{}, false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		// "Invalid generation ID": the provider says this id can never resolve
		// (e.g. a msg_ id stored by an older gateway). Retrying forever would keep
		// the credits held, so treat it as "no generation". 5xx, 429 and network
		// errors stay errors and are retried.
		return ledger.Usage{}, false, nil
	case resp.StatusCode == http.StatusNotFound:
		// Safe to read as "no generation" only because hold TTLs outlast the
		// endpoint's indexing lag (see server.HoldTTL).
		return ledger.Usage{}, false, nil
	case resp.StatusCode != http.StatusOK:
		return ledger.Usage{}, false, fmt.Errorf("generation lookup: status %d", resp.StatusCode)
	}
	var body struct {
		Data struct {
			TotalCost json.Number `json:"total_cost"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ledger.Usage{}, false, err
	}
	c, ok := costNanos(body.Data.TotalCost)
	if !ok {
		return ledger.Usage{}, false, fmt.Errorf("generation lookup: cost out of range")
	}
	return ledger.Usage{Cost: money.Nanos(c), Source: "openrouter_generation"}, true, nil
}
