package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// Config lists the upstream MCP servers the gateway proxies. A client names a
// server by its key in the URL (/mcp/{server}) and never supplies a URL, so
// the gateway cannot be pointed at an address of the caller's choosing (no SSRF).
type Config struct {
	Servers map[string]Server `json:"servers"`
	// AllowedOrigins are the Origin header values accepted besides none at all.
	// The spec requires validating Origin against DNS rebinding; a browser sends
	// one, an agent process does not.
	AllowedOrigins []string `json:"allowed_origins"`
}

// Server is one upstream.
type Server struct {
	URL string `json:"url"` // the upstream's MCP endpoint (Streamable HTTP)
	// Upstream credential (I9): the name of the environment variable that holds
	// the full header value (for example "Bearer abc"), and the header to put it
	// in (default Authorization). The value is never in the file, never sent to
	// a client, never logged. The client's own Authorization is not forwarded.
	AuthEnv    string `json:"auth_env"`
	AuthHeader string `json:"auth_header"`
	// Price per tools/call, in Nanos. Prices overrides DefaultPrice per tool.
	// A price of 0 (the default) makes the tool free: still authenticated,
	// scope-checked, screened and logged, but nothing is reserved or charged.
	DefaultPrice money.Nanos            `json:"default_price_nanos"`
	Prices       map[string]money.Nanos `json:"prices"`
}

// Price returns what one call of a tool costs.
func (s Server) Price(tool string) money.Nanos {
	if p, ok := s.Prices[tool]; ok {
		return p
	}
	return s.DefaultPrice
}

// Credential returns the upstream header name and value, or "", "" if none.
func (s Server) Credential(getenv func(string) string) (name, value string) {
	if s.AuthEnv == "" {
		return "", ""
	}
	name = s.AuthHeader
	if name == "" {
		name = "Authorization"
	}
	return name, getenv(s.AuthEnv)
}

// LoadConfig reads the file; unknown fields are rejected so a secret cannot be
// pasted into a field the loader would silently keep.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("mcp config: %w", err)
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("mcp config: parse %s: %w", path, err)
	}
	for name, s := range c.Servers {
		if s.URL == "" {
			return Config{}, fmt.Errorf("mcp config: server %q has no url", name)
		}
		if s.DefaultPrice < 0 {
			return Config{}, fmt.Errorf("mcp config: server %q has a negative price", name)
		}
		for t, p := range s.Prices {
			if p < 0 {
				return Config{}, fmt.Errorf("mcp config: server %q tool %q has a negative price", name, t)
			}
		}
	}
	return c, nil
}
