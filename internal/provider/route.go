package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Kinds of upstream.
const (
	KindOpenAICompatible = "openai-compatible" // OpenAI, OpenRouter: path passthrough
	KindAnthropic        = "anthropic"         // native Messages API
)

// ProviderCfg describes one upstream. BaseURL includes the version segment,
// e.g. https://openrouter.ai/api/v1 or https://api.anthropic.com/v1; the
// gateway appends the endpoint ("/chat/completions", "/messages"). The key is
// never in config: it comes from <NAME>_API_KEY in the environment (I9).
type ProviderCfg struct {
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	// IncludeUsage adds stream_options.include_usage to streaming chat
	// requests. Direct OpenAI needs it; OpenRouter does not.
	IncludeUsage bool `json:"include_usage,omitempty"`
	// NoUsageLookup marks an openai-compatible provider that has no per-request
	// usage endpoint (direct OpenAI). After a crash its holds are charged in
	// full instead of released (I6).
	NoUsageLookup bool `json:"no_usage_lookup,omitempty"`
}

// HasUsageLookup reports whether a call can be looked up after the fact.
func (c ProviderCfg) HasUsageLookup() bool {
	return c.Kind == KindOpenAICompatible && !c.NoUsageLookup
}

// Route maps a client model name to a provider and an upstream model name.
// First match wins; an empty Prefix matches everything.
type Route struct {
	Prefix   string `json:"prefix"`
	Provider string `json:"provider"`
	Strip    bool   `json:"strip,omitempty"` // drop Prefix from the upstream model name
}

type Config struct {
	Providers map[string]ProviderCfg `json:"providers"`
	Routes    []Route                `json:"routes"`
}

// Target is a resolved route.
type Target struct {
	Name  string
	Cfg   ProviderCfg
	Model string // upstream model name; also the price-table key
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("providers: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("providers: parse %s: %w", path, err)
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	for n, p := range c.Providers {
		if p.Kind != KindOpenAICompatible && p.Kind != KindAnthropic {
			return fmt.Errorf("providers: %s: unknown kind %q", n, p.Kind)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("providers: %s: base_url required", n)
		}
	}
	for _, r := range c.Routes {
		if _, ok := c.Providers[r.Provider]; !ok {
			return fmt.Errorf("providers: route %q: unknown provider %q", r.Prefix, r.Provider)
		}
	}
	return nil
}

func (c Config) Resolve(model string) (Target, bool) {
	for _, r := range c.Routes {
		if !strings.HasPrefix(model, r.Prefix) {
			continue
		}
		m := model
		if r.Strip {
			m = strings.TrimPrefix(model, r.Prefix)
		}
		return Target{Name: r.Provider, Cfg: c.Providers[r.Provider], Model: m}, true
	}
	return Target{}, false
}
