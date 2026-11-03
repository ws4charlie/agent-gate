package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndFile(t *testing.T) {
	t.Setenv(EnvListen, "")
	t.Setenv(EnvDatabaseURL, "")
	t.Setenv(EnvConfig, "")
	c, err := Load("")
	if err != nil || c.Listen != "127.0.0.1:8080" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = Load(write(t, `{"listen":":9","providers":{"openrouter":{"base_url":"https://x/v1"}}}`))
	if err != nil || c.Listen != ":9" || c.Providers["openrouter"].BaseURL != "https://x/v1" || c.DefaultMaxTokens != 1024 {
		t.Fatalf("file: %+v %v", c, err)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv(EnvListen, ":1")
	t.Setenv(EnvDatabaseURL, "postgres:///x")
	c, err := Load(write(t, `{"listen":":9"}`))
	if err != nil || c.Listen != ":1" || c.DatabaseURL != "postgres:///x" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestRejectsKeysInFile(t *testing.T) {
	_, err := Load(write(t, `{"providers":{"openrouter":{"api_key":"sk-x"}}}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown field error, got %v", err)
	}
}

func TestProviderKeyFromEnv(t *testing.T) {
	t.Setenv("OPEN_ROUTER_API_KEY", "k")
	if ProviderKey("open-router") != "k" || ProviderKey("nope") != "" {
		t.Fatal("ProviderKey")
	}
}
