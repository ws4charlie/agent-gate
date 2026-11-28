package wallet_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// allowedOn is allowed(), approved for network.
func allowedOn(t *testing.T, hold, network string) policy.Allowed {
	t.Helper()
	d := policy.Evaluate(
		policy.Request{AgentID: "a", Kind: policy.KindPayment, Tool: "t", Payee: payTo.Hex(), Network: network, Amount: 10_000_000, HoldID: hold},
		policy.Snapshot{AgentID: "a", Status: policy.StatusActive, Scope: policy.Scope{Tools: []string{"*"}, Payees: []string{payTo.Hex()}}})
	a, ok := d.Allowed()
	if !ok {
		t.Fatal(d.Rule)
	}
	return a
}

// The Vault signer takes Base Sepolia, the local anvil chain and the
// simulated chain, and nothing else.
func TestVaultSignerChains(t *testing.T) {
	cfg, _ := vaultCfg(t)
	for _, id := range []uint64{x402.BaseSepoliaChainID, x402.AnvilChainID, x402.LocalChainID} {
		c := cfg
		c.ChainID = id
		v, err := wallet.NewVaultSigner(c)
		if err != nil || v.Network() != "eip155:"+strconv.FormatUint(id, 10) {
			t.Errorf("chain %d: %v", id, err)
		}
	}
	for _, id := range []uint64{0, 1, 10, 8453, 31336, 31338, 84531, 84533, 11155111} {
		c := cfg
		c.ChainID = id
		if _, err := wallet.NewVaultSigner(c); !errors.Is(err, wallet.ErrRefusedChain) {
			t.Errorf("chain %d: %v", id, err)
		}
	}
}

// A signer is bound to its one chain: built for 31337 it refuses a request
// or an approval naming 84532, and the other way round, before the approval
// is spent and before Vault is called. On its own chain, the credential it
// sends Vault names that chain.
func TestVaultSignerRefusesOtherChain(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ signer, req, approved uint64 }{
		{x402.AnvilChainID, x402.BaseSepoliaChainID, x402.BaseSepoliaChainID},
		{x402.BaseSepoliaChainID, x402.AnvilChainID, x402.AnvilChainID},
		{x402.AnvilChainID, x402.AnvilChainID, x402.BaseSepoliaChainID},
		{x402.AnvilChainID, x402.BaseSepoliaChainID, x402.AnvilChainID},
		{x402.BaseSepoliaChainID, x402.BaseSepoliaChainID, x402.AnvilChainID},
	} {
		name := func(id uint64) string { return "eip155:" + strconv.FormatUint(id, 10) }
		cfg, f := vaultCfg(t)
		cfg.ChainID = c.signer
		v, err := wallet.NewVaultSigner(cfg)
		if err != nil {
			t.Fatal(err)
		}
		req := requirements(60)
		req.Network = name(c.req)
		a := allowedOn(t, "h", name(c.approved))
		if _, err := v.Prepare(ctx, a, "h", x402.Resource{}, req); err == nil || !strings.Contains(err.Error(), "network") {
			t.Fatalf("signer %d, request %d, approved %d: %v", c.signer, c.req, c.approved, err)
		}
		if !a.Valid() || v.Attempts() != 0 || f.Signs() != 0 {
			t.Fatalf("signer %d, request %d, approved %d: the approval was spent or Vault was called", c.signer, c.req, c.approved)
		}
	}
	for _, id := range []uint64{x402.AnvilChainID, x402.BaseSepoliaChainID} {
		cfg, f := vaultCfg(t)
		cfg.ChainID = id
		v, _ := wallet.NewVaultSigner(cfg)
		req := requirements(60)
		req.Network = v.Network()
		p, err := v.Prepare(ctx, allowedOn(t, "h", v.Network()), "h", x402.Resource{}, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := v.SignFor(ctx, p, wallet.Decision{PolicyVersion: "go1", Reason: "allow"}); err != nil {
			t.Fatal(err)
		}
		b := f.Bodies()
		if len(b) != 1 || !strings.Contains(string(b[0]), `"chain":"`+v.Network()+`"`) || !strings.Contains(string(b[0]), `"chainId":`+strconv.FormatUint(id, 10)+`,`) {
			t.Fatalf("chain %d: Vault got %s", id, b)
		}
	}
}
