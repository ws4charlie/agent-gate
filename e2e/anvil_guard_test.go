package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// fakeNode answers web3_clientVersion and eth_chainId with fixed values and
// counts any other method, so a test can prove nothing else was sent.
func fakeNode(t *testing.T, client, chainIDHex string, other *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "web3_clientVersion":
			res["result"] = client
		case "eth_chainId":
			res["result"] = chainIDHex
		default:
			*other++
			res["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The guard that stops a mint or anvil_setBalance reaching a real network. It
// runs without anvil: a stub node stands in.
func TestDialAnvilRefusesWhatIsNotAnvil(t *testing.T) {
	var other int
	if _, err := x402.DialAnvil(context.Background(), fakeNode(t, "Geth/v1.14.0-stable", "0x14a34", &other)); err == nil || !strings.Contains(err.Error(), "not anvil") {
		t.Fatalf("a non-anvil node was accepted: %v", err)
	}
	if _, err := x402.DialAnvil(context.Background(), fakeNode(t, "anvil/v1.9.0", "0x7a69", &other)); err == nil || !strings.Contains(err.Error(), "chain id") {
		t.Fatalf("anvil's default chain id was accepted: %v", err)
	}
	if other != 0 {
		t.Fatalf("%d calls beyond the two identity checks reached the node", other)
	}
	n, err := x402.DialAnvil(context.Background(), fakeNode(t, "anvil/v1.9.0", "0x14a34", &other))
	if err != nil {
		t.Fatal(err)
	}
	n.Close()
}
