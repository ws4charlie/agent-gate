package x402_test

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

const price = 10_000 // $0.01 in USDC atomic units

type rig struct {
	t      *testing.T
	sim    *x402.Sim
	ver    *x402.Verifier
	srv    *httptest.Server
	key    *ecdsa.PrivateKey
	w      *wallet.Wallet
	payTo  common.Address
	fetch  x402.Fetcher
	policy *policy.Policy
}

func newRig(t *testing.T) *rig {
	t.Helper()
	sim, err := x402.NewSim()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sim.Close() })
	key, _ := crypto.GenerateKey()
	w, err := wallet.New(key, x402.LocalChainID)
	if err != nil {
		t.Fatal(err)
	}
	payTo := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	if err := sim.Mint(context.Background(), w.Address(), big.NewInt(1_000_000)); err != nil {
		t.Fatal(err)
	}
	v := x402.NewVerifier(x402.Config{Network: x402.NetworkLocal, Token: sim.Token, TokenName: "USDC", TokenVersion: "2",
		PayTo: payTo, Chain: sim.Chain})
	h := http.NewServeMux()
	h.Handle("POST /tool", v.Protect(big.NewInt(price), "test tool", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p := policy.New(policy.Config{Agents: map[string]policy.AgentRule{"a": {Account: "acct-1", Budget: 1_000_000_000}}, Payees: []string{payTo.Hex()}})
	return &rig{t: t, sim: sim, ver: v, srv: srv, key: key, w: w, payTo: payTo, policy: p}
}

func (r *rig) bal(who common.Address) int64 {
	b, err := r.sim.Chain.BalanceOf(context.Background(), r.sim.Token, who)
	if err != nil {
		r.t.Fatal(err)
	}
	return b.Int64()
}

// challenge probes the tool and returns its requirements.
func (r *rig) challenge() (x402.Requirements, x402.Resource) {
	r.t.Helper()
	ch, free, err := r.fetch.Probe(context.Background(), "POST", r.srv.URL+"/tool", []byte(`{}`))
	if err != nil || free != nil {
		r.t.Fatalf("probe: %v %v", err, free)
	}
	req, err := ch.Pick()
	if err != nil {
		r.t.Fatal(err)
	}
	return req, ch.Required.Resource
}

// signed builds a payload by hand so tests can sign things the wallet never would.
func (r *rig) signed(req x402.Requirements, a x402.Auth, chainID int64) x402.PaymentPayload {
	dom := x402.Domain{Name: "USDC", Version: "2", ChainID: big.NewInt(chainID), Contract: r.sim.Token}
	sig, err := x402.Sign(r.key, x402.Digest(dom, a))
	if err != nil {
		r.t.Fatal(err)
	}
	return x402.PaymentPayload{X402Version: x402.Version, Accepted: req,
		Payload: x402.Payload{Signature: "0x" + common.Bytes2Hex(sig), Authorization: a.Wire()}}
}

func (r *rig) goodAuth(nonce byte) x402.Auth {
	now := time.Now().Unix()
	a := x402.Auth{From: r.w.Address(), To: r.payTo, Value: big.NewInt(price),
		ValidAfter: big.NewInt(now - 10), ValidBefore: big.NewInt(now + 60)}
	a.Nonce[0] = nonce
	return a
}

func (r *rig) send(p x402.PaymentPayload) (*http.Response, x402.SettleResponse, string) {
	r.t.Helper()
	resp, err := r.fetch.Pay(context.Background(), "POST", r.srv.URL+"/tool", []byte(`{}`), p)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var sr x402.SettleResponse
	_ = x402.DecodeHeader(resp.Header.Get(x402.HeaderResponse), &sr)
	var pr x402.PaymentRequired
	_ = x402.DecodeHeader(resp.Header.Get(x402.HeaderRequired), &pr)
	return resp, sr, pr.Error
}

func (r *rig) allowed() policy.Allowed {
	a, d := r.policy.Check(policy.Request{AccountID: "acct-1", AgentID: "a", Payee: r.payTo.Hex(), Amount: price * 1000, Network: x402.NetworkLocal})
	if d != nil {
		r.t.Fatal(d)
	}
	return a
}

func TestSettleEndToEnd(t *testing.T) {
	r := newRig(t)
	req, res := r.challenge()
	if req.Network != x402.NetworkLocal || req.Amount != "10000" || req.Extra.Name != "USDC" {
		t.Fatalf("requirements: %+v", req)
	}
	p, err := r.w.Pay(context.Background(), r.allowed(), "hold-1", res, req)
	if err != nil {
		t.Fatal(err)
	}
	resp, sr, _ := r.send(p)
	if resp.StatusCode != 200 || !sr.Success || sr.Transaction == "" {
		t.Fatalf("status %d settle %+v", resp.StatusCode, sr)
	}
	if r.bal(r.payTo) != price || r.bal(r.w.Address()) != 1_000_000-price {
		t.Fatalf("balances payee=%d payer=%d", r.bal(r.payTo), r.bal(r.w.Address()))
	}
}

// I11: the same payment submitted twice moves money once. Run it against a
// fresh Verifier too, so the guard is the token contract and not our memory.
func TestReplayMovesMoneyOnce(t *testing.T) {
	r := newRig(t)
	req, res := r.challenge()
	p, _ := r.w.Pay(context.Background(), r.allowed(), "hold-replay", res, req)
	if resp, _, _ := r.send(p); resp.StatusCode != 200 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp, _, why := r.send(p)
	if resp.StatusCode != 402 || !strings.HasPrefix(why, x402.ReasonReplay) {
		t.Fatalf("replay: %d %q", resp.StatusCode, why)
	}
	// Same hold id again re-signs the same nonce with a new window: still one transfer.
	p2, _ := r.w.Pay(context.Background(), r.allowed(), "hold-replay", res, req)
	if resp, _, _ := r.send(p2); resp.StatusCode != 402 {
		t.Fatalf("re-signed replay: %d", resp.StatusCode)
	}
	// Forget everything the server remembers: the contract still refuses.
	r.ver.ResetSeenForTest()
	if resp, _, _ := r.send(p2); resp.StatusCode != 402 {
		t.Fatalf("after memory loss: %d", resp.StatusCode)
	}
	if got := r.bal(r.payTo); got != price {
		t.Fatalf("payee got %d, want one payment of %d", got, price)
	}
}

func TestNonceIsPerHold(t *testing.T) {
	if x402.PaymentNonce("h1") != x402.PaymentNonce("h1") || x402.PaymentNonce("h1") == x402.PaymentNonce("h2") {
		t.Fatal("nonce must be a pure function of the hold id and differ across holds")
	}
}

func TestServerRejects(t *testing.T) {
	r := newRig(t)
	req, _ := r.challenge()
	now := time.Now().Unix()
	other := common.HexToAddress("0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC")

	cases := []struct {
		name string
		mk   func() x402.PaymentPayload
		want string
	}{
		{"tampered signature", func() x402.PaymentPayload {
			p := r.signed(req, r.goodAuth(1), x402.LocalChainID)
			sig := []byte(p.Payload.Signature)
			sig[10] ^= 1 // flip one hex digit
			p.Payload.Signature = string(sig)
			return p
		}, x402.ReasonBadSignature},
		{"wrong payee", func() x402.PaymentPayload {
			a := r.goodAuth(2)
			a.To = other
			return r.signed(req, a, x402.LocalChainID)
		}, x402.ReasonRecipient},
		{"amount too low", func() x402.PaymentPayload {
			a := r.goodAuth(3)
			a.Value = big.NewInt(price - 1)
			return r.signed(req, a, x402.LocalChainID)
		}, x402.ReasonValueMismatch},
		{"amount too high", func() x402.PaymentPayload {
			a := r.goodAuth(4)
			a.Value = big.NewInt(price + 1)
			return r.signed(req, a, x402.LocalChainID)
		}, x402.ReasonValueMismatch},
		{"expired window", func() x402.PaymentPayload {
			a := r.goodAuth(5)
			a.ValidAfter, a.ValidBefore = big.NewInt(now-120), big.NewInt(now-60)
			return r.signed(req, a, x402.LocalChainID)
		}, x402.ReasonValidBefore},
		{"not yet valid", func() x402.PaymentPayload {
			a := r.goodAuth(6)
			a.ValidAfter, a.ValidBefore = big.NewInt(now+30), big.NewInt(now+60)
			return r.signed(req, a, x402.LocalChainID)
		}, x402.ReasonValidAfter},
		{"wrong network", func() x402.PaymentPayload {
			bad := req
			bad.Network = x402.NetworkBaseSepolia
			return r.signed(bad, r.goodAuth(7), x402.BaseSepoliaChainID)
		}, x402.ReasonNetwork},
		{"signed for another chain's domain", func() x402.PaymentPayload {
			return r.signed(req, r.goodAuth(8), x402.BaseSepoliaChainID) // accepted says local, signature says 84532
		}, x402.ReasonBadSignature},
		{"cheaper requirements than issued", func() x402.PaymentPayload {
			cheap := req
			cheap.Amount = "1"
			a := r.goodAuth(9)
			a.Value = big.NewInt(1)
			return r.signed(cheap, a, x402.LocalChainID)
		}, x402.ReasonRequirements},
		{"signer is not from", func() x402.PaymentPayload {
			a := r.goodAuth(10)
			a.From = other
			return r.signed(req, a, x402.LocalChainID)
		}, x402.ReasonBadSignature},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, sr, why := r.send(c.mk())
			if resp.StatusCode != 402 || sr.Success || !strings.HasPrefix(why, c.want) {
				t.Fatalf("status %d error %q, want 402 %q", resp.StatusCode, why, c.want)
			}
		})
	}
	if r.bal(r.payTo) != 0 || r.bal(r.w.Address()) != 1_000_000 {
		t.Fatal("a rejected payment moved money")
	}
}

func TestInsufficientFunds(t *testing.T) {
	r := newRig(t)
	req, _ := r.challenge()
	a := r.goodAuth(11)
	r.key, _ = crypto.GenerateKey() // a payer with no tokens
	a.From = crypto.PubkeyToAddress(r.key.PublicKey)
	_, _, why := r.send(r.signed(req, a, x402.LocalChainID))
	if !strings.HasPrefix(why, x402.ReasonInsufficientFunds) {
		t.Fatalf("got %q", why)
	}
}

func TestWalletRefusals(t *testing.T) {
	key, _ := crypto.GenerateKey()
	if _, err := wallet.New(key, 1); err == nil {
		t.Fatal("wallet must refuse Ethereum mainnet")
	}
	if _, err := wallet.New(key, 8453); err == nil {
		t.Fatal("wallet must refuse Base mainnet")
	}
	if _, err := wallet.New(key, x402.BaseSepoliaChainID); err != nil {
		t.Fatal(err)
	}
	w, _ := wallet.New(key, x402.LocalChainID)
	if _, err := w.Pay(context.Background(), policy.Allowed{}, "h", x402.Resource{}, x402.Requirements{}); err == nil {
		t.Fatal("a zero Allowed must not sign")
	}
	if w.SignCount() != 0 {
		t.Fatal("signed anyway")
	}
	_ = money.Nanos(0)
}

func TestWalletBindsPayeeAndAmountToWhatPolicyApproved(t *testing.T) {
	r := newRig(t)
	req, res := r.challenge()
	a := r.allowed()
	evil := req
	evil.PayTo = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
	if _, err := r.w.Pay(context.Background(), a, "h", res, evil); err == nil {
		t.Fatal("signed for a payee the policy never approved")
	}
	pricey := req
	pricey.Amount = "20000"
	if _, err := r.w.Pay(context.Background(), a, "h", res, pricey); err == nil {
		t.Fatal("signed for an amount the policy never approved")
	}
	if r.w.SignCount() != 0 {
		t.Fatal("sign counter moved")
	}
}

func TestAuthLookupThreeOutcomes(t *testing.T) {
	r := newRig(t)
	req, res := r.challenge()
	ctx := context.Background()
	// Prepared and signed, not yet sent.
	pend, err := r.w.Prepare(ctx, r.allowed(), "hold-lookup", res, req)
	if err != nil {
		t.Fatal(err)
	}
	ref := pend.Ref()
	now := time.Now()
	open := &x402.AuthLookup{Chain: r.sim.Chain, Now: func() time.Time { return now }}
	closed := &x402.AuthLookup{Chain: r.sim.Chain, Now: func() time.Time { return now.Add(10 * time.Minute) }}

	if _, _, err := open.Lookup(ctx, "x402", ref); !errors.Is(err, x402.ErrWindowOpen) {
		t.Fatalf("unused + window open must be an error, got %v", err)
	}
	if _, found, err := closed.Lookup(ctx, "x402", ref); err != nil || found {
		t.Fatalf("unused + window closed must be not-found: %v %v", found, err)
	}
	// Send it for real.
	p, _ := r.w.Sign(pend)
	if resp, _, _ := r.send(p); resp.StatusCode != 200 {
		t.Fatalf("settle: %d", resp.StatusCode)
	}
	for _, l := range []*x402.AuthLookup{open, closed} {
		u, found, err := l.Lookup(ctx, "x402", ref)
		if err != nil || !found || u.Cost != price*1000 || u.Source != "chain" {
			t.Fatalf("used: %+v %v %v", u, found, err)
		}
	}
	if _, _, err := open.Lookup(ctx, "x402", "not json"); err == nil {
		t.Fatal("an unreadable ref must be an error, never a release")
	}
}

// The simulated chain lives inside the paid-tool process, so the gateway asks
// it over HTTP.
func TestRemoteChainReader(t *testing.T) {
	r := newRig(t)
	srv := httptest.NewServer(x402.AuthStateHandler(r.sim.Chain))
	defer srv.Close()
	rc := &x402.RemoteChain{BaseURL: srv.URL}
	req, res := r.challenge()
	pend, _ := r.w.Prepare(context.Background(), r.allowed(), "hold-remote", res, req)
	ref, _ := x402.DecodeAuthRef(pend.Ref())
	used, err := rc.AuthorizationUsed(context.Background(), ref.Token, ref.From, ref.Nonce)
	if err != nil || used {
		t.Fatalf("before: %v %v", used, err)
	}
	p, _ := r.w.Sign(pend)
	r.send(p)
	if used, err = rc.AuthorizationUsed(context.Background(), ref.Token, ref.From, ref.Nonce); err != nil || !used {
		t.Fatalf("after: %v %v", used, err)
	}
}

// The wallet converts with a checked helper: a value that overflows Nanos is refused.
func TestWalletRefusesOverflowingAmount(t *testing.T) {
	r := newRig(t)
	req, res := r.challenge()
	req.Amount = "9223372036854775807" // MaxInt64 atomic units
	a, d := r.policy.Check(policy.Request{AccountID: "acct-1", AgentID: "a", Payee: r.payTo.Hex(), Amount: money.Nanos(1), Network: x402.NetworkLocal})
	if d != nil {
		t.Fatal(d)
	}
	if _, err := r.w.Prepare(context.Background(), a, "h", res, req); err == nil {
		t.Fatal("overflowing amount accepted")
	}
	if r.w.SignCount() != 0 {
		t.Fatal("signed")
	}
}
