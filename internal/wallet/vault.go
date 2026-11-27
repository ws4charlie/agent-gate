package wallet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ws4charlie/agent-gate/internal/gatecred"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// Signer is what the payment path signs with: the local key (*Wallet) or
// QuorumVault (*VaultSigner). Both build the authorization in Prepare, which
// needs and spends a policy.Allowed (I10), and only the signer that prepared
// a Pending signs it.
type Signer interface {
	Address() common.Address
	Network() string
	Prepare(ctx context.Context, allowed policy.Allowed, holdID string, res x402.Resource, req x402.Requirements) (*Pending, error)
	// SignFor signs p. The Attempt is nil for the local key; for Vault it is
	// what happened, also on error, for the audit row.
	SignFor(ctx context.Context, p *Pending, d Decision) (x402.PaymentPayload, *Attempt, error)
}

var (
	_ Signer = (*Wallet)(nil)
	_ Signer = (*VaultSigner)(nil)
)

// Decision is what Vault's credential needs from the policy decision.
type Decision struct {
	PolicyVersion string // the policy Gate decided under (≤ 64 bytes)
	Reason        string // "allow" or "approved_by:<who>" (≤ 256 bytes)
}

// Outcomes of a Vault attempt.
const (
	OutcomeSigned       = "signed"        // a verified signature came back
	OutcomeRefused      = "refused"       // Vault decided not to sign (Code, Field)
	OutcomeBadSignature = "bad_signature" // a 200 that does not verify: Vault may have signed
	OutcomeUnavailable  = "unavailable"   // no decision: retries ran out, TLS/token failure, deadline
	OutcomeError        = "error"         // failed inside Gate before or around the call
)

// Attempt is one Vault signing call, for the audit row. It never holds the
// credential's sig, the token or a key.
type Attempt struct {
	CredentialID string
	WalletID     string
	Payee        string // lower hex
	Amount       string // atomic units
	Outcome      string
	Code, Field  string // Vault's refusal code and field
	Config       bool   // the failure points at configuration, not at this payment
}

// ErrWindowTooShort: the paid tool's maxTimeoutSeconds leaves too little time
// for a credential and a Vault call. Refused before Vault is asked.
var ErrWindowTooShort = errors.New("wallet: the payment's time window is too short to sign through Vault")

// MinVaultWindow is the shortest authorization window the Vault signer accepts.
const MinVaultWindow = 15 * time.Second

// VaultConfig is what NewVaultSigner needs.
type VaultConfig struct {
	Client   *vaultsign.Client
	CredKey  gatecred.Key
	Audience string         // Vault deployment, the credential's audience
	WalletID string         // the Gate-governed wallet in Vault (vaultsign config)
	From     common.Address // that wallet's address: the authorization's from
	ChainID  uint64
	// TTL of the credential and of the authorization window; 0 = gatecred.DefaultTTL.
	// Shortened to the paid tool's maxTimeoutSeconds when that is smaller.
	TTL time.Duration
}

// VaultSigner signs through QuorumVault: for each payment it mints a Gate
// credential bound to exactly that payment (internal/gatecred) and asks
// Vault to sign (internal/vaultsign). One attempt per Pending: a refusal
// burns the nonce at Vault, so a retry needs a new hold.
type VaultSigner struct {
	cfg      VaultConfig
	maxTx    money.Nanos
	now      func() time.Time
	attempts atomic.Int64
}

// NewVaultSigner checks the configuration. The chain guard is the same as the
// local wallet's.
func NewVaultSigner(cfg VaultConfig) (*VaultSigner, error) {
	if cfg.ChainID != x402.BaseSepoliaChainID && cfg.ChainID != x402.LocalChainID {
		return nil, ErrRefusedChain
	}
	if cfg.Client == nil {
		return nil, errors.New("wallet: vault signer needs a vaultsign client")
	}
	if cfg.CredKey.Kid == "" {
		return nil, errors.New("wallet: vault signer needs the Gate credential key")
	}
	if cfg.Audience == "" || cfg.WalletID == "" {
		return nil, errors.New("wallet: vault signer needs an audience and a wallet id")
	}
	if cfg.From == (common.Address{}) {
		return nil, errors.New("wallet: vault signer needs the Vault wallet's address")
	}
	if cfg.TTL == 0 {
		cfg.TTL = gatecred.DefaultTTL
	}
	if cfg.TTL < MinVaultWindow || cfg.TTL > gatecred.MaxTTL {
		return nil, fmt.Errorf("wallet: vault credential TTL %s must be between %s and %s", cfg.TTL, MinVaultWindow, gatecred.MaxTTL)
	}
	return &VaultSigner{cfg: cfg, maxTx: DefaultMaxTx, now: time.Now}, nil
}

func (v *VaultSigner) Address() common.Address { return v.cfg.From }
func (v *VaultSigner) Network() string         { return "eip155:" + strconv.FormatUint(v.cfg.ChainID, 10) }
func (v *VaultSigner) WalletID() string        { return v.cfg.WalletID }
func (v *VaultSigner) Audience() string        { return v.cfg.Audience }

// SetMaxTx sets the per-transaction cap; <= 0 is ignored (see Wallet.SetMaxTx).
func (v *VaultSigner) SetMaxTx(n money.Nanos) {
	if n > 0 {
		v.maxTx = n
	}
}

// SetClock is for tests.
func (v *VaultSigner) SetClock(f func() time.Time) { v.now = f }

// Attempts is how many times this signer called Vault (test hook for I10).
func (v *VaultSigner) Attempts() int64 { return v.attempts.Load() }

func (v *VaultSigner) String() string       { return "vault-signer(" + v.cfg.WalletID + ")" }
func (v *VaultSigner) GoString() string     { return v.String() }
func (v *VaultSigner) LogValue() slog.Value { return slog.StringValue(v.String()) }

// Prepare runs the same checks as the local wallet and builds the
// authorization for the Vault wallet. Its window is the credential's:
// validAfter = issued_at and validBefore = expires_at (Vault requires the
// authorization window inside the credential window), no longer than the
// paid tool's maxTimeoutSeconds.
func (v *VaultSigner) Prepare(ctx context.Context, allowed policy.Allowed, holdID string, res x402.Resource, req x402.Requirements) (*Pending, error) {
	ttl := v.cfg.TTL
	if tool := time.Duration(req.MaxTimeoutSeconds) * time.Second; tool < ttl {
		ttl = tool
	}
	amt, err := checkAllowed(ctx, allowed, holdID, req, v.Network(), v.maxTx, func() error {
		if ttl < MinVaultWindow {
			return fmt.Errorf("%w (maxTimeoutSeconds %d < %d)", ErrWindowTooShort, req.MaxTimeoutSeconds, int(MinVaultWindow.Seconds()))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	issued := v.now().Unix()
	a := x402.Auth{From: v.cfg.From, To: common.HexToAddress(req.PayTo), Value: amt,
		ValidAfter: big.NewInt(issued), ValidBefore: big.NewInt(issued + int64(ttl/time.Second)),
		Nonce: x402.PaymentNonce(holdID)}
	dom := x402.Domain{Name: req.Extra.Name, Version: req.Extra.Version, ChainID: new(big.Int).SetUint64(v.cfg.ChainID),
		Contract: common.HexToAddress(req.Asset)}
	return &Pending{auth: a, dom: dom, req: req, res: res, holdID: holdID, by: v}, nil
}

// SignFor mints the credential for exactly this authorization and asks Vault
// to sign it, with the credential's expiry as the deadline.
func (v *VaultSigner) SignFor(ctx context.Context, p *Pending, d Decision) (x402.PaymentPayload, *Attempt, error) {
	var zero x402.PaymentPayload
	if p == nil || p.by != v {
		return zero, nil, errors.New("wallet: nothing prepared by this vault signer")
	}
	at := &Attempt{WalletID: v.cfg.WalletID, Payee: strings.ToLower(p.auth.To.Hex()), Amount: p.auth.Value.String(), Outcome: OutcomeError}
	signed, err := gatecred.Mint(v.cfg.CredKey, gatecred.Credential{
		ObligationID:   p.holdID,
		Audience:       v.cfg.Audience,
		WalletID:       v.cfg.WalletID,
		Chain:          v.Network(),
		Asset:          strings.ToLower(p.dom.Contract.Hex()),
		Payee:          at.Payee,
		Amount:         at.Amount,
		IssuedAt:       p.auth.ValidAfter.Int64(),
		ExpiresAt:      p.auth.ValidBefore.Int64(),
		PolicyVersion:  d.PolicyVersion,
		DecisionReason: d.Reason,
	})
	if err != nil {
		return zero, at, fmt.Errorf("wallet: minting the gate credential: %w", err)
	}
	at.CredentialID = signed.CredentialID
	sctx, cancel := context.WithDeadline(ctx, time.Unix(signed.ExpiresAt, 0))
	defer cancel()
	v.attempts.Add(1)
	sig, err := v.cfg.Client.Sign(sctx, vaultsign.Request{Domain: p.dom, Auth: p.auth, Credential: signed.JSON, CredentialID: signed.CredentialID})
	if err != nil {
		classify(at, err)
		return zero, at, err
	}
	at.Outcome = OutcomeSigned
	return p.payload(sig.Hex()), at, nil
}

func classify(at *Attempt, err error) {
	var ref *vaultsign.Refusal
	switch {
	case errors.As(err, &ref):
		at.Outcome, at.Code, at.Field, at.Config = OutcomeRefused, ref.Code, ref.Field, ref.Config()
	case errors.Is(err, vaultsign.ErrBadSignature):
		at.Outcome = OutcomeBadSignature
	case errors.Is(err, vaultsign.ErrTLS), errors.Is(err, vaultsign.ErrToken):
		at.Outcome, at.Config = OutcomeUnavailable, true
	case errors.Is(err, vaultsign.ErrRetryExhausted), errors.Is(err, vaultsign.ErrExpired),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		at.Outcome = OutcomeUnavailable
	default:
		at.Outcome = OutcomeError
	}
}
