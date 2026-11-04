package e2e

import (
	"context"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// anvilURL returns the node's URL, or skips (fails under AG_REQUIRE_ANVIL=1).
// The host is checked before any connection: a remote URL is refused without
// sending it a single request.
func anvilURL(t testing.TB) string {
	t.Helper()
	raw := os.Getenv("AG_ANVIL_URL")
	if raw == "" {
		if os.Getenv("AG_REQUIRE_ANVIL") == "1" {
			t.Fatal("AG_REQUIRE_ANVIL=1 but AG_ANVIL_URL is not set")
		}
		t.Skip("AG_ANVIL_URL not set: start `anvil --chain-id 84532` and set it to run the chain tests")
	}
	if err := checkLoopback(raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func checkLoopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" {
		return &refusal{"scheme " + u.Scheme + ": only plain http to a local node"}
	}
	h := strings.ToLower(u.Hostname())
	if h != "localhost" {
		if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
			return &refusal{"host " + h + ": only localhost, 127.0.0.1 or ::1"}
		}
	}
	return nil
}

type refusal struct{ why string }

func (r *refusal) Error() string { return "e2e: refusing AG_ANVIL_URL: " + r.why }

// dialAnvil connects through the same guard the binary uses (anvil, chain 84532).
func dialAnvil(t testing.TB) *x402.Node {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := x402.DialAnvil(ctx, anvilURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}

func TestLoopbackGuard(t *testing.T) {
	for _, bad := range []string{"https://127.0.0.1:8545", "http://sepolia.base.org", "http://10.0.0.5:8545", "http://anvil.example.com:8545"} {
		if err := checkLoopback(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	for _, ok := range []string{"http://127.0.0.1:8545", "http://localhost:8545", "http://[::1]:8545"} {
		if err := checkLoopback(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}
