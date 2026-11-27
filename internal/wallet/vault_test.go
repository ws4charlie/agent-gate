package wallet_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ws4charlie/agent-gate/internal/gatecred"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/vaultsign/vaulttest"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

var payTo = common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")

func vaultCfg(t *testing.T) (wallet.VaultConfig, *vaulttest.Vault) {
	t.Helper()
	f := vaulttest.New(t, vaulttest.Options{})
	cli, err := vaultsign.New(vaultsign.Config{BaseURL: f.URL, WalletID: f.WalletID, ClientCert: f.ClientCert, ClientKey: f.ClientKey, CAFile: f.CAFile,
		Token: &vaultsign.ClientCredentials{TokenURL: f.TokenURL, ClientID: vaulttest.ClientID, SecretFile: f.SecretFile, HTTPClient: f.HTTPClient()}})
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, err := gatecred.NewKey("gate-test-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	return wallet.VaultConfig{Client: cli, CredKey: key, Audience: "qv-test", WalletID: f.WalletID, From: f.From, ChainID: x402.LocalChainID}, f
}

func allowed(t *testing.T, hold string) policy.Allowed {
	t.Helper()
	d := policy.Evaluate(
		policy.Request{AgentID: "a", Kind: policy.KindPayment, Tool: "t", Payee: payTo.Hex(), Network: x402.NetworkLocal, Amount: 10_000_000, HoldID: hold},
		policy.Snapshot{AgentID: "a", Status: policy.StatusActive, Scope: policy.Scope{Tools: []string{"*"}, Payees: []string{payTo.Hex()}}})
	a, ok := d.Allowed()
	if !ok {
		t.Fatal(d.Rule)
	}
	return a
}

func requirements(timeout int) x402.Requirements {
	return x402.Requirements{Network: x402.NetworkLocal, Amount: "10000", Asset: "0x5FbDB2315678afecb367f032d93F642f64180aa3",
		PayTo: payTo.Hex(), MaxTimeoutSeconds: timeout, Extra: x402.Extra{Name: "USDC", Version: "2"}}
}

func TestVaultSignerConfigGuards(t *testing.T) {
	cfg, _ := vaultCfg(t)
	for _, id := range []uint64{1, 8453, 11155111} {
		c := cfg
		c.ChainID = id
		if _, err := wallet.NewVaultSigner(c); !errors.Is(err, wallet.ErrRefusedChain) {
			t.Errorf("chain %d: %v", id, err)
		}
	}
	for _, ttl := range []time.Duration{time.Second, wallet.MinVaultTTL - time.Second, gatecred.MaxTTL + time.Second} {
		c := cfg
		c.TTL = ttl
		if _, err := wallet.NewVaultSigner(c); err == nil {
			t.Errorf("TTL %s accepted", ttl)
		}
	}
	c := cfg
	c.From = common.Address{}
	if _, err := wallet.NewVaultSigner(c); err == nil {
		t.Error("no from address accepted")
	}
	v, err := wallet.NewVaultSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s := fmt.Sprintf("%v %+v %#v", v, v, v); strings.Contains(s, "PRIVATE") || !strings.Contains(s, cfg.WalletID) {
		t.Fatalf("prints %s", s)
	}
}

// I10 holds for both signers: no Pending without an Allowed, the cap applies,
// and a Pending is signed only by the signer that prepared it.
func TestVaultSignerPrepareGuards(t *testing.T) {
	cfg, f := vaultCfg(t)
	v, _ := wallet.NewVaultSigner(cfg)
	ctx := context.Background()
	if _, err := v.Prepare(ctx, policy.Allowed{}, "h", x402.Resource{}, requirements(60)); err == nil {
		t.Fatal("prepared without an Allowed")
	}
	v.SetMaxTx(5_000_000)
	if _, err := v.Prepare(ctx, allowed(t, "h"), "h", x402.Resource{}, requirements(60)); !errors.Is(err, wallet.ErrOverTxCap) {
		t.Fatalf("cap: %v", err)
	}
	v.SetMaxTx(wallet.DefaultMaxTx)

	key, _ := crypto.GenerateKey()
	w, _ := wallet.New(key, x402.LocalChainID)
	wp, err := w.Prepare(ctx, allowed(t, "h1"), "h1", x402.Resource{}, requirements(60))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.SignFor(ctx, wp, wallet.Decision{PolicyVersion: "go1", Reason: "allow"}); err == nil || f.Signs() != 0 {
		t.Fatal("vault signed a Pending the local wallet prepared")
	}
	vp, err := v.Prepare(ctx, allowed(t, "h2"), "h2", x402.Resource{}, requirements(60))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Sign(vp); err == nil || w.SignCount() != 0 {
		t.Fatal("local wallet signed a Pending the vault signer prepared")
	}
	if v.Attempts() != 0 {
		t.Fatal("vault was called")
	}
}

// The window: starts 10 s back, ends at the earlier of issued_at + TTL and now
// + maxTimeoutSeconds; less than 15 s left is refused without spending the approval.
func TestVaultSignerWindow(t *testing.T) {
	cfg, _ := vaultCfg(t)
	cfg.TTL = 40 * time.Second
	v, _ := wallet.NewVaultSigner(cfg)
	now := time.Unix(1_800_000_000, 0)
	v.SetClock(func() time.Time { return now })
	ctx := context.Background()
	for _, tc := range []struct {
		timeout      int
		before, life int64
	}{
		{60, now.Unix() + 30, 40}, // TTL wins: issued_at + 40
		{20, now.Unix() + 20, 30}, // the tool's window wins
	} {
		hold := fmt.Sprint("h", tc.timeout)
		p, err := v.Prepare(ctx, allowed(t, hold), hold, x402.Resource{}, requirements(tc.timeout))
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := x402.DecodeAuthRef(p.Ref())
		if ref.ValidBefore != tc.before || ref.From != cfg.From || ref.Nonce != x402.PaymentNonce(hold) {
			t.Fatalf("timeout %d: ref %+v", tc.timeout, ref)
		}
	}
	a := allowed(t, "short")
	if _, err := v.Prepare(ctx, a, "short", x402.Resource{}, requirements(14)); !errors.Is(err, wallet.ErrWindowTooShort) {
		t.Fatalf("14 s: %v", err)
	}
	if _, err := v.Prepare(ctx, a, "short", x402.Resource{}, requirements(15)); err != nil {
		t.Fatalf("approval burned by the refusal: %v", err)
	}
}

// fix1 #6: the windows as Vault receives them, read from the minted
// credential and the typed data: the authorization inside the credential,
// and the credential no longer than Vault's maximum.
func TestVaultSignerWindowOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		ttl     time.Duration
		timeout int
	}{{0, 60}, {0, 300}, {40 * time.Second, 600}, {90 * time.Second, 20}} {
		cfg, f := vaultCfg(t)
		cfg.TTL = tc.ttl
		v, _ := wallet.NewVaultSigner(cfg)
		hold := fmt.Sprint("w", tc.timeout)
		p, err := v.Prepare(context.Background(), allowed(t, hold), hold, x402.Resource{}, requirements(tc.timeout))
		if err != nil {
			t.Fatal(err)
		}
		// The fake checks nothing of this; the call's result does not matter here.
		_, _, _ = v.SignFor(context.Background(), p, wallet.Decision{PolicyVersion: "go1", Reason: "allow"})
		bodies := f.Bodies()
		if len(bodies) != 1 {
			t.Fatalf("bodies %d", len(bodies))
		}
		var req struct {
			Payload struct {
				Message map[string]string `json:"message"`
			} `json:"payload"`
			Credential struct {
				IssuedAt  int64 `json:"issued_at"`
				ExpiresAt int64 `json:"expires_at"`
			} `json:"credential"`
		}
		if err := json.Unmarshal(bodies[0], &req); err != nil {
			t.Fatal(err)
		}
		after, _ := strconv.ParseInt(req.Payload.Message["validAfter"], 10, 64)
		before, _ := strconv.ParseInt(req.Payload.Message["validBefore"], 10, 64)
		c := req.Credential
		if after < c.IssuedAt || before > c.ExpiresAt || c.ExpiresAt-c.IssuedAt > 600 || after >= before ||
			after >= time.Now().Unix() || before > time.Now().Unix()+int64(tc.timeout) {
			t.Fatalf("ttl %s timeout %d: validAfter %d validBefore %d, credential %d..%d", tc.ttl, tc.timeout, after, before, c.IssuedAt, c.ExpiresAt)
		}
	}
}

// fix1 #5: one signature per Pending. A second Sign/SignFor refuses locally:
// never a second signature, never a second credential for the same nonce.
func TestPendingSignsOnce(t *testing.T) {
	cfg, f := vaultCfg(t)
	v, _ := wallet.NewVaultSigner(cfg)
	ctx := context.Background()
	d := wallet.Decision{PolicyVersion: "go1", Reason: "allow"}
	p, err := v.Prepare(ctx, allowed(t, "o1"), "o1", x402.Resource{}, requirements(60))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.SignFor(ctx, p, d); err != nil {
		t.Fatal(err)
	}
	if _, at, err := v.SignFor(ctx, p, d); !errors.Is(err, wallet.ErrAlreadySigned) || at != nil {
		t.Fatalf("second SignFor: %v %+v", err, at)
	}
	if len(f.Bodies()) != 1 || v.Attempts() != 1 {
		t.Fatalf("vault asked %d times", len(f.Bodies()))
	}

	key, _ := crypto.GenerateKey()
	w, _ := wallet.New(key, x402.LocalChainID)
	wp, err := w.Prepare(ctx, allowed(t, "o2"), "o2", x402.Resource{}, requirements(60))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Sign(wp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.SignFor(ctx, wp, d); !errors.Is(err, wallet.ErrAlreadySigned) || w.SignCount() != 1 {
		t.Fatalf("second local sign: %v, count %d", err, w.SignCount())
	}
}
