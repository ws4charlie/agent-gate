package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

func balanceWith(t *testing.T, base, key string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/balance", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestExpiredKeyIs401KeyExpired(t *testing.T) {
	e := newEnv(t, 5*usd, nil)
	ctx := context.Background()
	st := agents.NewStore(e.pool)
	secret, _, err := st.CreateWithTTL(ctx, e.acct, "exp", policy.DefaultScope(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := balanceWith(t, e.gw.URL, secret); resp.StatusCode != 200 {
		t.Fatalf("fresh key: %d", resp.StatusCode)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE agents SET key_expires_at = now() - interval '1 minute' WHERE name='exp'`); err != nil {
		t.Fatal(err)
	}
	resp, body := balanceWith(t, e.gw.URL, secret)
	var out struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if resp.StatusCode != 401 || out.Error.Code != "key_expired" {
		t.Fatalf("expired key: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, secret) {
		t.Fatal("response echoes the key")
	}
	// An unknown key is still the plain 401.
	resp, body = balanceWith(t, e.gw.URL, "ag_nope")
	if resp.StatusCode != 401 || !strings.Contains(body, `"unauthorized"`) {
		t.Fatalf("unknown key: %d %s", resp.StatusCode, body)
	}
}

func TestKeyExpiryWarningHeader(t *testing.T) {
	e := newEnv(t, 5*usd, nil)
	ctx := context.Background()
	st := agents.NewStore(e.pool)
	soon, _, err := st.CreateWithTTL(ctx, e.acct, "soon", policy.DefaultScope(), 3*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	far, _, err := st.CreateWithTTL(ctx, e.acct, "far", policy.DefaultScope(), 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	never, _ := mustKey(t, e, "never")

	resp, _ := balanceWith(t, e.gw.URL, soon)
	h := resp.Header.Get("X-AgentGate-Key-Expires")
	if resp.StatusCode != 200 || h == "" {
		t.Fatalf("soon: %d header %q", resp.StatusCode, h)
	}
	if ts, err := time.Parse(time.RFC3339, h); err != nil || time.Until(ts) > 3*24*time.Hour {
		t.Fatalf("header %q: %v", h, err)
	}
	for name, k := range map[string]string{"far": far, "never": never} {
		if resp, _ := balanceWith(t, e.gw.URL, k); resp.Header.Get("X-AgentGate-Key-Expires") != "" {
			t.Fatalf("%s got a header", name)
		}
	}
	// The log warns once per agent per hour and never holds the key.
	balanceWith(t, e.gw.URL, soon)
	logs := e.logs.String()
	if n := strings.Count(logs, "api key expires soon"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, logs)
	}
	for _, k := range []string{soon, far, never} {
		if strings.Contains(logs, k) {
			t.Fatal("the log holds a key")
		}
	}
}

func TestPreviousKeyDuringGraceGetsHeader(t *testing.T) {
	e := newEnv(t, 5*usd, nil)
	ctx := context.Background()
	st := agents.NewStore(e.pool)
	old, _, err := st.CreateWithTTL(ctx, e.acct, "rotating", policy.DefaultScope(), 0)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _, err := st.RotateKey(ctx, "rotating", 2*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := balanceWith(t, e.gw.URL, old)
	h := resp.Header.Get("X-AgentGate-Key-Expires")
	ts, perr := time.Parse(time.RFC3339, h)
	if resp.StatusCode != 200 || perr != nil || time.Until(ts) > 2*time.Hour {
		t.Fatalf("old key: %d header %q", resp.StatusCode, h)
	}
	if resp, _ := balanceWith(t, e.gw.URL, fresh); resp.StatusCode != 200 || resp.Header.Get("X-AgentGate-Key-Expires") != "" {
		t.Fatalf("new key: %d %q", resp.StatusCode, resp.Header.Get("X-AgentGate-Key-Expires"))
	}
	if _, err := e.pool.Exec(ctx, `UPDATE agents SET prev_key_expires_at = now() - interval '1 second' WHERE name='rotating'`); err != nil {
		t.Fatal(err)
	}
	if resp, body := balanceWith(t, e.gw.URL, old); resp.StatusCode != 401 || !strings.Contains(body, "key_expired") {
		t.Fatalf("old key after grace: %d %s", resp.StatusCode, body)
	}
}

func mustKey(t *testing.T, e *env, name string) (string, agents.Agent) {
	t.Helper()
	s, a, err := agents.NewStore(e.pool).Create(context.Background(), e.acct, name, policy.DefaultScope())
	if err != nil {
		t.Fatal(err)
	}
	return s, a
}
