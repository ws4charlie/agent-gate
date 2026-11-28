package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// The chain each network signs on. anvil takes the id its node reports, and
// only 31337 or 84532; base-sepolia is 84532 and nothing else; sim is 1337.
func TestChainIDFor(t *testing.T) {
	for _, c := range []struct {
		network  string
		reported uint64
		want     uint64 // 0: refused
	}{
		{"anvil", 31337, 31337},
		{"anvil", 84532, 84532},
		{"anvil", 1, 0},
		{"anvil", 1337, 0},
		{"anvil", 8453, 0},
		{"anvil", 11155111, 0},
		{"anvil", 0, 0},
		{"anvil", 31338, 0},
		{"base-sepolia", 0, 84532}, // no node asked here: the reconciler's RPC check covers it
		{"base-sepolia", 84532, 84532},
		{"base-sepolia", 31337, 0},
		{"base-sepolia", 1, 0},
		{"sim", 0, 1337},
		{"base", 84532, 0},
	} {
		got, err := chainIDFor(c.network, c.reported)
		if c.want == 0 {
			if err == nil {
				t.Errorf("%s at %d: accepted as %d", c.network, c.reported, got)
			} else if c.network == "anvil" && (!strings.Contains(err.Error(), "31337") || !strings.Contains(err.Error(), "84532")) {
				t.Errorf("%s at %d: %v does not name both allowed ids", c.network, c.reported, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s at %d: got %d, %v; want %d", c.network, c.reported, got, err, c.want)
		}
	}
}

// fakeRPC answers web3_clientVersion with client and eth_chainId with id.
func fakeRPC(t *testing.T, client string, id uint64) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var res any
		switch req.Method {
		case "web3_clientVersion":
			res = client
		case "eth_chainId":
			res = "0x" + strconv.FormatUint(id, 16)
		default:
			http.Error(w, "no", http.StatusBadRequest)
			return
		}
		b, _ := json.Marshal(res)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, b)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// Start-up reads the chain id from the node: anvil at 31337 or 84532 signs
// on that id; any other anvil, or a node that is not anvil, is refused before
// a signer exists. base-sepolia's RPC must report 84532, as before.
func TestStartupChainIDFromNode(t *testing.T) {
	ctx := context.Background()
	for _, id := range []uint64{31337, 84532, 1, 1337, 8453, 11155111} {
		t.Setenv("AG_RPC_URL", fakeRPC(t, "anvil/v1.0.0", id))
		reported, err := anvilChainID(ctx)
		if err == nil {
			_, err = chainIDFor("anvil", reported)
		}
		if ok := id == 31337 || id == 84532; ok != (err == nil) || (ok && reported != id) {
			t.Errorf("anvil at %d: reported %d, %v", id, reported, err)
		}
	}
	t.Setenv("AG_RPC_URL", fakeRPC(t, "Geth/v1.14", 31337))
	if _, err := anvilChainID(ctx); err == nil || !strings.Contains(err.Error(), "not anvil") {
		t.Errorf("a node that is not anvil: %v", err)
	}
	for _, id := range []uint64{84532, 31337} {
		t.Setenv("AG_RPC_URL", fakeRPC(t, "Geth/v1.14", id))
		_, err := chainReader(ctx, PaymentsFile{Network: "base-sepolia"})
		if (id == x402.BaseSepoliaChainID) != (err == nil) {
			t.Errorf("base-sepolia RPC at %d: %v", id, err)
		}
	}
}
