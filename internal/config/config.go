// Package config loads the JSON config file and applies env overrides.
// Provider keys come only from the environment (hard rule 1): the file format
// has no key field, and unknown fields are rejected so one cannot sneak in.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ws4charlie/agent-gate/internal/money"
)

type Config struct {
	Listen           string      `json:"listen"`
	DatabaseURL      string      `json:"database_url"`
	MaxHoldNanos     money.Nanos `json:"max_hold_nanos"`
	DefaultMaxTokens int64       `json:"default_max_tokens"`
}

// Env var names. AG_* override the file; DATABASE_URL is the conventional name.
const (
	EnvConfig      = "AG_CONFIG"
	EnvListen      = "AG_LISTEN"
	EnvDatabaseURL = "DATABASE_URL"
)

func defaults() Config {
	return Config{
		Listen:           "127.0.0.1:8080",
		MaxHoldNanos:     5_000_000_000, // $5
		DefaultMaxTokens: 1024,
	}
}

// Load reads the file at path (empty path: use $AG_CONFIG; still empty: defaults
// only), then applies env overrides.
func Load(path string) (Config, error) {
	cfg := defaults()
	if path == "" {
		path = os.Getenv(EnvConfig)
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("config: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	if v := os.Getenv(EnvListen); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv(EnvDatabaseURL); v != "" {
		cfg.DatabaseURL = v
	}
	return cfg, nil
}

// ProviderKey returns the API key for a provider from env, e.g. "openrouter" ->
// OPENROUTER_API_KEY. Empty means unset. Keys never enter Config, so they cannot
// be marshalled into a log line (I9).
func ProviderKey(name string) string {
	return os.Getenv(strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name)) + "_API_KEY")
}
