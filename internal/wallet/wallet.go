// Package wallet holds the agent's key. Pay is the only code path that signs,
// and it needs a policy.Allowed (I10).
package wallet

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

const (
	EnvKey     = "AG_WALLET_KEY"      // hex private key
	EnvKeyFile = "AG_WALLET_KEY_FILE" // path to a gitignored file holding it
	// DefaultKeyFile is under data/, which is gitignored; *.key is too.
	DefaultKeyFile = "data/wallet.key"
)

// ErrRefusedChain: the wallet only signs for Base Sepolia or the local simulated chain.
var ErrRefusedChain = errors.New("wallet: refusing to start, chain id must be 84532 (Base Sepolia) or 1337 (local simulated chain)")

type Wallet struct {
	key     *ecdsa.PrivateKey
	chainID uint64
	signs   atomic.Int64 // test hook for I10: how many signatures were produced
	now     func() time.Time
}

// New builds a wallet. The chain id check is the guard against a mainnet
// config ever reaching a signer.
func New(key *ecdsa.PrivateKey, chainID uint64) (*Wallet, error) {
	if chainID != x402.BaseSepoliaChainID && chainID != x402.LocalChainID {
		return nil, ErrRefusedChain
	}
	if key == nil {
		return nil, errors.New("wallet: no key")
	}
	return &Wallet{key: key, chainID: chainID, now: time.Now}, nil
}

// SetClock is for tests.
func (w *Wallet) SetClock(f func() time.Time) { w.now = f }

func (w *Wallet) Address() common.Address { return crypto.PubkeyToAddress(w.key.PublicKey) }
func (w *Wallet) Network() string         { return "eip155:" + strconv.FormatUint(w.chainID, 10) }

// SignCount is how many signatures this wallet ever produced.
func (w *Wallet) SignCount() int64 { return w.signs.Load() }

// Pending is an authorization that has been built and approved but not signed.
// Splitting Prepare from Sign lets the gateway record the authorization on the
// ledger hold BEFORE any signature exists: once signed, the chain decides
// whether money moved, and the reconciler needs the handle to ask it. Its
// fields are unexported, so a Pending only comes from Prepare (which needs an
// Allowed), keeping I10.
type Pending struct {
	auth x402.Auth
	dom  x402.Domain
	req  x402.Requirements
	res  x402.Resource
}

// Ref is the opaque string to store on the hold before signing.
func (p *Pending) Ref() string { return x402.NewAuthRef(p.auth, p.dom.Contract).Encode() }

// Prepare checks the request against what policy approved (payee, amount,
// network) and builds the authorization, with the nonce derived from the hold
// id (I11). It does not sign.
func (w *Wallet) Prepare(ctx context.Context, allowed policy.Allowed, holdID string, res x402.Resource, req x402.Requirements) (*Pending, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !allowed.Valid() {
		return nil, errors.New("wallet: payment was not approved by policy")
	}
	ar := allowed.Request()
	if req.Network != w.Network() || ar.Network != req.Network {
		return nil, fmt.Errorf("wallet: network %q is not this wallet's %q", req.Network, w.Network())
	}
	if !common.IsHexAddress(req.PayTo) || !strings.EqualFold(ar.Payee, req.PayTo) {
		return nil, errors.New("wallet: payee differs from the one policy approved")
	}
	amt, ok := new(big.Int).SetString(req.Amount, 10)
	if !ok || amt.Sign() <= 0 {
		return nil, errors.New("wallet: bad amount")
	}
	if n, ok := x402.NanosFromAtomic(amt); !ok || n != ar.Amount { // checked: no overflow
		return nil, errors.New("wallet: amount differs from the one policy approved")
	}
	if !common.IsHexAddress(req.Asset) || holdID == "" {
		return nil, errors.New("wallet: bad asset or hold id")
	}
	// Last check, so a Prepare that failed above does not burn the approval:
	// spend it, tied to this hold. A second Prepare with the same Allowed, or
	// with another hold id, stops here (I10).
	if err := allowed.Use(holdID); err != nil {
		return nil, fmt.Errorf("wallet: %w", err)
	}
	now := w.now().Unix()
	a := x402.Auth{From: w.Address(), To: common.HexToAddress(req.PayTo), Value: amt,
		ValidAfter: big.NewInt(now - 10), ValidBefore: big.NewInt(now + int64(req.MaxTimeoutSeconds)),
		Nonce: x402.PaymentNonce(holdID)}
	dom := x402.Domain{Name: req.Extra.Name, Version: req.Extra.Version, ChainID: new(big.Int).SetUint64(w.chainID),
		Contract: common.HexToAddress(req.Asset)}
	return &Pending{auth: a, dom: dom, req: req, res: res}, nil
}

// Sign signs exactly the values Prepare built and Ref reported.
func (w *Wallet) Sign(p *Pending) (x402.PaymentPayload, error) {
	var zero x402.PaymentPayload
	if p == nil {
		return zero, errors.New("wallet: nothing prepared")
	}
	w.signs.Add(1)
	sig, err := x402.Sign(w.key, x402.Digest(p.dom, p.auth))
	if err != nil {
		return zero, err
	}
	r := p.res
	return x402.PaymentPayload{X402Version: x402.Version, Resource: &r, Accepted: p.req,
		Payload: x402.Payload{Signature: "0x" + common.Bytes2Hex(sig), Authorization: p.auth.Wire()}}, nil
}

// Pay is Prepare then Sign, for callers that do not need to record the
// authorization first.
func (w *Wallet) Pay(ctx context.Context, allowed policy.Allowed, holdID string, res x402.Resource, req x402.Requirements) (x402.PaymentPayload, error) {
	p, err := w.Prepare(ctx, allowed, holdID, res, req)
	if err != nil {
		return x402.PaymentPayload{}, err
	}
	return w.Sign(p)
}

// Atomic converts Nanos to USDC atomic units, rejecting fractions of a micro-USDC.
func Atomic(n money.Nanos) (*big.Int, bool) { return x402.AtomicFromNanos(n) }

// Nanos converts USDC atomic units to Nanos (overflow checked).
func Nanos(atomic *big.Int) (money.Nanos, bool) { return x402.NanosFromAtomic(atomic) }

// LoadKey reads the key from $AG_WALLET_KEY, else the file named by
// $AG_WALLET_KEY_FILE (default data/wallet.key).
func LoadKey() (*ecdsa.PrivateKey, error) {
	hexKey := strings.TrimSpace(os.Getenv(EnvKey))
	if hexKey == "" {
		path := os.Getenv(EnvKeyFile)
		if path == "" {
			path = DefaultKeyFile
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("wallet: no key: set %s or put one in %s (agentgate wallet new): %w", EnvKey, path, err)
		}
		hexKey = strings.TrimSpace(string(b))
	}
	return crypto.HexToECDSA(strings.TrimPrefix(hexKey, "0x"))
}

// NewKeyFile creates a testnet key at path (mode 0600, refuses to overwrite)
// and returns only the address. The key is never printed.
func NewKeyFile(path string) (common.Address, error) {
	k, err := ecdsa.GenerateKey(crypto.S256(), rand.Reader)
	if err != nil {
		return common.Address{}, err
	}
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return common.Address{}, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return common.Address{}, fmt.Errorf("wallet: %w (refusing to overwrite an existing key)", err)
	}
	defer f.Close()
	if _, err := f.WriteString(common.Bytes2Hex(crypto.FromECDSA(k)) + "\n"); err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(k.PublicKey), nil
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i+1]
	}
	return "."
}
