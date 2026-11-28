package wallet_test

import (
	"errors"
	"math"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// "No Vault, nothing moves on-chain": a local-key signer can be built for the
// simulated chain and for nothing else. Every chain id the gateway knows
// (anvil runs with Base Sepolia's 84532) and the common real ones are refused.
func TestLocalKeySignsOnlyOnSim(t *testing.T) {
	key, err := crypto.GenerateKey() // run-time key, never stored
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wallet.New(key, x402.LocalChainID); err != nil {
		t.Fatalf("sim chain refused: %v", err)
	}
	for _, id := range []uint64{
		x402.BaseSepoliaChainID, // base-sepolia and anvil
		0, 1, 10, 56, 137, 8453, 42161, 31337, 84531, 11155111, math.MaxUint64,
		x402.LocalChainID + 1, x402.LocalChainID - 1,
	} {
		if _, err := wallet.New(key, id); !errors.Is(err, wallet.ErrRefusedChain) {
			t.Errorf("chain id %d: a local-key signer was built (err %v)", id, err)
		}
	}
}
