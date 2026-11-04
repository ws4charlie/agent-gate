package x402

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Network name for a local anvil node started with --chain-id 84532. The
// signed domain carries the Base Sepolia chain id, so the wallet, the verifier
// and the 402 payload are byte-for-byte what they are on the testnet; only the
// node and the token contract are local.
const NetworkAnvil = "anvil"

// DefaultAnvilURL is where `anvil` listens unless told otherwise.
const DefaultAnvilURL = "http://127.0.0.1:8545"

// Node is a JSON-RPC connection to a local anvil node. Every entry point that
// can mint tokens or call anvil_setBalance goes through DialAnvil, which
// refuses any node that does not identify itself as anvil, so those calls can
// never reach a real network.
type Node struct {
	RPC    *rpc.Client
	Client *ethclient.Client
	URL    string
}

// DialAnvil connects and checks, before returning anything usable:
// web3_clientVersion names anvil, and eth_chainId is 84532.
func DialAnvil(ctx context.Context, url string) (*Node, error) {
	rc, err := rpc.DialContext(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("anvil: dial %s: %w", url, err)
	}
	var ver string
	if err := rc.CallContext(ctx, &ver, "web3_clientVersion"); err != nil {
		rc.Close()
		return nil, fmt.Errorf("anvil: web3_clientVersion: %w", err)
	}
	if !strings.Contains(strings.ToLower(ver), "anvil") {
		rc.Close()
		return nil, fmt.Errorf("anvil: refusing node %q: it is not anvil (mint and anvil_setBalance must never reach a real network)", ver)
	}
	var id hexutil.Big
	if err := rc.CallContext(ctx, &id, "eth_chainId"); err != nil {
		rc.Close()
		return nil, fmt.Errorf("anvil: eth_chainId: %w", err)
	}
	if id.ToInt().Cmp(big.NewInt(BaseSepoliaChainID)) != 0 {
		rc.Close()
		return nil, fmt.Errorf("anvil: node has chain id %s, want %d (start it with --chain-id %d)", id.ToInt(), BaseSepoliaChainID, BaseSepoliaChainID)
	}
	return &Node{RPC: rc, Client: ethclient.NewClient(rc), URL: url}, nil
}

func (n *Node) Close() { n.Client.Close() }

// SetBalance gives addr `wei` of the native coin (anvil_setBalance): the
// faucet-free way to pay gas.
func (n *Node) SetBalance(ctx context.Context, addr common.Address, wei *big.Int) error {
	return n.RPC.CallContext(ctx, nil, "anvil_setBalance", addr, (*hexutil.Big)(wei))
}

// IncreaseTime moves the node's clock forward and mines a block, so the new
// time is visible to contracts and to HeaderByNumber. No sleeping.
func (n *Node) IncreaseTime(ctx context.Context, d time.Duration) error {
	if err := n.RPC.CallContext(ctx, nil, "evm_increaseTime", int64(d/time.Second)); err != nil {
		return err
	}
	return n.RPC.CallContext(ctx, nil, "evm_mine")
}

// Head is the latest block's number and timestamp.
func (n *Node) Head(ctx context.Context) (number uint64, at time.Time, err error) {
	h, err := n.Client.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, time.Time{}, err
	}
	return h.Number.Uint64(), time.Unix(int64(h.Time), 0), nil
}

// Clock is "now" as the chain sees it: wall time plus the amount the node's
// clock was moved by. An authorization window is a chain-time fact (the
// contract compares validBefore to block.timestamp), so the wallet, the
// verifier and the reconciler's window check all take this clock. Tests move it
// with Advance and never sleep.
type Clock struct {
	node *Node
	off  atomic.Int64 // nanoseconds, chain minus wall
}

// NewClock measures the offset from a fresh block, so a node that an earlier
// run already advanced is handled.
func (n *Node) NewClock(ctx context.Context) (*Clock, error) {
	c := &Clock{node: n}
	return c, c.measure(ctx)
}

func (c *Clock) measure(ctx context.Context) error {
	if err := c.node.RPC.CallContext(ctx, nil, "evm_mine"); err != nil {
		return err
	}
	_, at, err := c.node.Head(ctx)
	if err != nil {
		return err
	}
	c.off.Store(int64(at.Sub(time.Now())))
	return nil
}

// Now is the chain's current time.
func (c *Clock) Now() time.Time { return time.Now().Add(time.Duration(c.off.Load())) }

// Advance moves the node's clock forward by d (evm_increaseTime + evm_mine).
func (c *Clock) Advance(ctx context.Context, d time.Duration) error {
	if err := c.node.IncreaseTime(ctx, d); err != nil {
		return err
	}
	return c.measure(ctx)
}

// Restore puts the node's clock back on wall time (anvil_setTime) if Advance
// moved it, so whatever runs next on a shared node (a demo, another test)
// signs authorization windows against the time it expects. Blocks mined after
// a backward move carry earlier timestamps than their parents; anvil allows it.
func (c *Clock) Restore(ctx context.Context) error {
	if time.Duration(c.off.Load()).Abs() < 5*time.Second {
		return nil
	}
	if err := c.node.RPC.CallContext(ctx, nil, "anvil_setTime", time.Now().Unix()); err != nil {
		return err
	}
	return c.measure(ctx)
}

// NewChain returns an EthChain whose gas payer is a fresh random key funded
// with anvil_setBalance (I: no key lives in the repo, not even anvil's
// well-known dev keys).
func (n *Node) NewChain(ctx context.Context) (*EthChain, error) {
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	hundred := new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18))
	if err := n.SetBalance(ctx, crypto.PubkeyToAddress(key.PublicKey), hundred); err != nil {
		return nil, fmt.Errorf("anvil: fund facilitator: %w", err)
	}
	return &EthChain{Client: n.Client, Facilitator: key, ReceiptTimeout: 30 * time.Second}, nil
}

// DeployMockUSDC deploys the mock token (EIP-712 domain "USDC" / "2", the real
// token's config) from the chain's own key and returns its address.
func (c *EthChain) DeployMockUSDC(ctx context.Context) (common.Address, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rc, err := c.sendAndWait(ctx, nil, common.FromHex(mockBin), 2_000_000)
	if err != nil {
		return common.Address{}, fmt.Errorf("deploy MockUSDC: %w", err)
	}
	return rc.ContractAddress, nil
}

// Mint gives who `amount` atomic units of the mock token (anyone may mint it).
func (c *EthChain) Mint(ctx context.Context, token, who common.Address, amount *big.Int) error {
	data, err := tokenABI.Pack("mint", who, amount)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.sendAndWait(ctx, &token, data, 0)
	return err
}

func (c *EthChain) sendAndWait(ctx context.Context, to *common.Address, data []byte, gas uint64) (*types.Receipt, error) {
	me := crypto.PubkeyToAddress(c.Facilitator.PublicKey)
	tx, err := c.buildTo(ctx, me, to, data)
	if err != nil {
		return nil, err
	}
	if gas > 0 { // a deployment's estimate is exact but cheap to pad further
		tx, err = types.SignTx(types.NewTx(&types.DynamicFeeTx{ChainID: tx.ChainId(), Nonce: tx.Nonce(), GasTipCap: tx.GasTipCap(),
			GasFeeCap: tx.GasFeeCap(), Gas: max(gas, tx.Gas()), To: to, Data: data}), types.LatestSignerForChainID(tx.ChainId()), c.Facilitator)
		if err != nil {
			return nil, err
		}
	}
	if err := c.Client.SendTransaction(ctx, tx); err != nil {
		return nil, err
	}
	rc, err := c.wait(ctx, tx.Hash())
	if err != nil {
		return nil, err
	}
	if rc.Status != types.ReceiptStatusSuccessful {
		return nil, errors.New("transaction reverted")
	}
	return rc, nil
}
