package x402_test

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// TokenDomain reads the deployed mock's domain ("USDC", "2", the sim chain)
// and refuses an address with no code.
func TestTokenDomain(t *testing.T) {
	sim, err := x402.NewSim()
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	ctx := context.Background()
	d, err := sim.Chain.TokenDomain(ctx, sim.Token)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "USDC" || d.Version != "2" || d.Contract != sim.Token || d.ChainID.Uint64() != x402.LocalChainID {
		t.Fatalf("domain = %+v", d)
	}
	for _, bad := range []common.Address{
		common.HexToAddress("0x00000000000000000000000000000000000000ff"), // nothing there
		crypto.PubkeyToAddress(sim.Deployer.PublicKey),                     // an account, not a contract
	} {
		if _, err := sim.Chain.TokenDomain(ctx, bad); err == nil || !strings.Contains(err.Error(), "no contract") {
			t.Errorf("%s: err = %v, want no contract", bad.Hex(), err)
		}
	}
}

// A contract that is not a token (its code is a single STOP) is refused.
func TestTokenDomainRefusesNonToken(t *testing.T) {
	sim, err := x402.NewSim()
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	ctx := context.Background()
	from := crypto.PubkeyToAddress(sim.Deployer.PublicKey)
	nonce, err := sim.Chain.Client.PendingNonceAt(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	// init code: copy the 1-byte runtime (0x00, STOP) to memory and return it.
	init := common.FromHex("6001600c60003960016000f300")
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{ChainID: new(big.Int).SetUint64(x402.LocalChainID), Nonce: nonce,
		GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(1e11), Gas: 100_000, Data: init}),
		types.LatestSignerForChainID(new(big.Int).SetUint64(x402.LocalChainID)), sim.Deployer)
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.Chain.Client.SendTransaction(ctx, tx); err != nil {
		t.Fatal(err)
	}
	sim.Backend.Commit()
	rc, err := sim.Chain.Client.TransactionReceipt(ctx, tx.Hash())
	if err != nil || rc.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := sim.Chain.TokenDomain(ctx, rc.ContractAddress); err == nil || !strings.Contains(err.Error(), "name()") {
		t.Fatalf("err = %v, want a name() failure", err)
	}
}
