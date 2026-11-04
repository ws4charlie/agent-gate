package x402

import (
	"context"
	"crypto/ecdsa"
	_ "embed"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

//go:embed testdata/MockUSDC.abi.json
var mockABIJSON string

//go:embed testdata/MockUSDC.bin
var mockBin string

var tokenABI = func() abi.ABI {
	a, err := abi.JSON(strings.NewReader(mockABIJSON))
	if err != nil {
		panic(err)
	}
	return a
}()

// ErrReverted: the settlement transaction was mined and failed, or the
// pre-broadcast simulation failed. Nothing moved.
var ErrReverted = errors.New("x402: transferWithAuthorization reverted")

// PendingError: the transaction was broadcast but its receipt could not be
// confirmed. Money may or may not have moved; the caller must not treat it as
// a failure (spec "settlement_pending").
type PendingError struct {
	Tx  common.Hash
	Err error
}

func (e *PendingError) Error() string {
	return "x402: settlement pending, tx " + e.Tx.Hex() + ": " + e.Err.Error()
}
func (e *PendingError) Unwrap() error { return e.Err }

// Chain is everything the server middleware needs from a blockchain.
type Chain interface {
	ChainID(ctx context.Context) (*big.Int, error)
	BalanceOf(ctx context.Context, token, who common.Address) (*big.Int, error)
	AuthorizationUsed(ctx context.Context, token, who common.Address, nonce [32]byte) (bool, error)
	// Settle simulates, then submits transferWithAuthorization and waits for the receipt.
	// The facilitator (gas payer) is the chain's own key, not the payer's.
	Settle(ctx context.Context, token common.Address, a Auth, sig []byte) (common.Hash, error)
}

// Client is the slice of an ethclient that EthChain uses; both a real
// *ethclient.Client and go-ethereum's simulated backend client satisfy it.
type Client interface {
	ethereum.ChainStateReader
	ethereum.ContractCaller
	ethereum.TransactionSender
	ethereum.GasPricer1559
	ethereum.PendingStateReader
	ethereum.TransactionReader
	ethereum.GasEstimator
	ChainID(ctx context.Context) (*big.Int, error)
	HeaderByNumber(ctx context.Context, n *big.Int) (*types.Header, error)
}

// EthChain talks to a real or simulated EVM chain.
type EthChain struct {
	Client      Client
	Facilitator *ecdsa.PrivateKey // pays gas; holds no tokens
	// AfterSend runs after a transaction is submitted. The simulated backend
	// uses it to mine a block; a real chain leaves it nil and is polled.
	AfterSend func()
	// BeforeSend runs before a transaction is submitted (the simulated backend
	// uses it to move its clock to wall time).
	BeforeSend func()
	// ReceiptTimeout bounds the wait for a receipt on a real chain.
	ReceiptTimeout time.Duration

	mu sync.Mutex // one in-flight settlement: keeps the facilitator's nonce simple
}

func (c *EthChain) ChainID(ctx context.Context) (*big.Int, error) { return c.Client.ChainID(ctx) }

func (c *EthChain) call(ctx context.Context, token common.Address, method string, args ...any) ([]any, error) {
	data, err := tokenABI.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	out, err := c.Client.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	return tokenABI.Unpack(method, out)
}

func (c *EthChain) BalanceOf(ctx context.Context, token, who common.Address) (*big.Int, error) {
	r, err := c.call(ctx, token, "balanceOf", who)
	if err != nil {
		return nil, err
	}
	return r[0].(*big.Int), nil
}

func (c *EthChain) AuthorizationUsed(ctx context.Context, token, who common.Address, nonce [32]byte) (bool, error) {
	r, err := c.call(ctx, token, "authorizationState", who, nonce)
	if err != nil {
		return false, err
	}
	return r[0].(bool), nil
}

func (c *EthChain) Settle(ctx context.Context, token common.Address, a Auth, sig []byte) (common.Hash, error) {
	if len(sig) != 65 {
		return common.Hash{}, errors.New("bad signature length")
	}
	v := sig[64]
	var r, s [32]byte
	copy(r[:], sig[:32])
	copy(s[:], sig[32:64])
	data, err := tokenABI.Pack("transferWithAuthorization", a.From, a.To, a.Value, a.ValidAfter, a.ValidBefore, a.Nonce, v, r, s)
	if err != nil {
		return common.Hash{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.BeforeSend != nil {
		c.BeforeSend()
	}
	me := crypto.PubkeyToAddress(c.Facilitator.PublicKey)
	// Simulate first (spec verify step 5): a revert here costs no gas and broadcasts nothing.
	if _, err := c.Client.CallContract(ctx, ethereum.CallMsg{From: me, To: &token, Data: data}, nil); err != nil {
		return common.Hash{}, fmt.Errorf("%w: %v", ErrReverted, err)
	}
	tx, err := c.build(ctx, me, token, data)
	if err != nil {
		return common.Hash{}, err
	}
	if err := c.Client.SendTransaction(ctx, tx); err != nil {
		// A send error does not prove the node never got the transaction (a
		// timeout can lose only the reply). Only the node's standard rejection
		// messages are definite; anything else is "pending" so the caller keeps
		// the hold and asks the chain later. The hash is the signed tx's own.
		if rejectedBySend(err) {
			return tx.Hash(), fmt.Errorf("%w: send rejected: %v", ErrReverted, err)
		}
		return tx.Hash(), &PendingError{Tx: tx.Hash(), Err: err}
	}
	if c.AfterSend != nil {
		c.AfterSend()
	}
	rc, err := c.wait(ctx, tx.Hash())
	if err != nil {
		return tx.Hash(), &PendingError{Tx: tx.Hash(), Err: err}
	}
	if rc.Status != types.ReceiptStatusSuccessful {
		return tx.Hash(), ErrReverted
	}
	return tx.Hash(), nil
}

func (c *EthChain) build(ctx context.Context, from, to common.Address, data []byte) (*types.Transaction, error) {
	nonce, err := c.Client.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, err
	}
	tip, err := c.Client.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, err
	}
	head, err := c.Client.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	base := head.BaseFee
	if base == nil {
		base = new(big.Int)
	}
	gas, err := c.Client.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Data: data})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrReverted, err)
	}
	chainID, err := c.Client.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: chainID, Nonce: nonce, GasTipCap: tip,
		GasFeeCap: new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), tip), Gas: gas + gas/5, To: &to, Data: data})
	return types.SignTx(tx, types.LatestSignerForChainID(chainID), c.Facilitator)
}

func (c *EthChain) wait(ctx context.Context, h common.Hash) (*types.Receipt, error) {
	timeout := c.ReceiptTimeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		rc, err := c.Client.TransactionReceipt(ctx, h)
		if err == nil {
			return rc, nil
		}
		if !errors.Is(err, ethereum.NotFound) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// rejectedBySend matches the standard node errors that prove the transaction
// was refused before entering the pool. "already known" is deliberately absent:
// it means the node HAS the transaction.
func rejectedBySend(err error) bool {
	m := strings.ToLower(err.Error())
	for _, k := range []string{"nonce too low", "underpriced", "insufficient funds", "intrinsic gas too low", "exceeds block gas limit", "invalid sender", "max fee per gas less than block base fee"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}
