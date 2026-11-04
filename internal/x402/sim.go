package x402

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/params"
)

// Sim is an in-process EVM (go-ethereum's simulated backend) with MockUSDC
// deployed. It is the "offline chain" of PLAN: real EVM execution, real
// ecrecover, real EIP-712, no network.
type Sim struct {
	Backend *simulated.Backend
	Chain   *EthChain
	Token   common.Address
	// Deployer deployed the token and is the gas payer ("facilitator") for settlements.
	Deployer *ecdsa.PrivateKey
}

// NewSim starts a simulated chain and deploys MockUSDC.
func NewSim() (*Sim, error) {
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	eth := new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
	b := simulated.NewBackend(types.GenesisAlloc{addr: {Balance: eth}})
	s := &Sim{Backend: b, Deployer: key}
	s.Chain = &EthChain{Client: b.Client(), Facilitator: key, AfterSend: func() { b.Commit() }, BeforeSend: s.syncClock}
	s.syncClock()

	ctx := context.Background()
	init := common.FromHex(mockBin)
	tx, err := s.Chain.build(ctx, addr, common.Address{}, init)
	if err != nil {
		return nil, fmt.Errorf("sim deploy: %w", err)
	}
	// build() targets a call address; a deployment has To == nil.
	tx = types.NewTx(&types.DynamicFeeTx{ChainID: tx.ChainId(), Nonce: tx.Nonce(), GasTipCap: tx.GasTipCap(),
		GasFeeCap: tx.GasFeeCap(), Gas: 2_000_000, Data: init})
	tx, err = types.SignTx(tx, types.LatestSignerForChainID(tx.ChainId()), key)
	if err != nil {
		return nil, err
	}
	if err := s.Chain.Client.SendTransaction(ctx, tx); err != nil {
		return nil, err
	}
	b.Commit()
	rc, err := s.Chain.Client.TransactionReceipt(ctx, tx.Hash())
	if err != nil || rc.Status != types.ReceiptStatusSuccessful {
		return nil, fmt.Errorf("sim deploy failed: %v", err)
	}
	s.Token = rc.ContractAddress
	return s, nil
}

// syncClock moves block time to wall time so authorization windows (which are
// wall-clock seconds in the payload) mean the same on the sim as on a real chain.
func (s *Sim) syncClock() {
	h, err := s.Chain.Client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		return
	}
	now := uint64(time.Now().Unix())
	if now > h.Time+1 {
		_ = s.Backend.AdjustTime(time.Duration(now-h.Time-1) * time.Second)
	}
}

// Mint gives who `amount` atomic units of the mock token.
func (s *Sim) Mint(ctx context.Context, who common.Address, amount *big.Int) error {
	data, err := tokenABI.Pack("mint", who, amount)
	if err != nil {
		return err
	}
	s.Chain.mu.Lock()
	defer s.Chain.mu.Unlock()
	s.syncClock()
	tx, err := s.Chain.build(ctx, crypto.PubkeyToAddress(s.Deployer.PublicKey), s.Token, data)
	if err != nil {
		return err
	}
	if err := s.Chain.Client.SendTransaction(ctx, tx); err != nil {
		return err
	}
	s.Backend.Commit()
	return nil
}

func (s *Sim) Close() error { return s.Backend.Close() }
