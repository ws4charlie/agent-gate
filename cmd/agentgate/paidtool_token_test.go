package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	ecommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// --token is checked before anything starts: anvil only, and a real address.
func TestPaidtoolTokenFlagRefusals(t *testing.T) {
	payTo := "--pay-to=0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--network=sim", "--token=0x00000000000000000000000000000000000000ff", payTo}, "for --network anvil only"},
		{[]string{"--network=base-sepolia", "--token=0x00000000000000000000000000000000000000ff", payTo}, "for --network anvil only"},
		{[]string{"--network=anvil", "--token=usdc", payTo}, "not a 0x address"},
	} {
		err := paidtoolCmd(context.Background(), c.args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

// localAnvil returns $AG_ANVIL_URL (a loopback node, chain id 84532) or skips,
// as the e2e chain tests do; AG_REQUIRE_ANVIL=1 turns the skip into a failure.
func localAnvil(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("AG_ANVIL_URL")
	if raw == "" {
		if os.Getenv("AG_REQUIRE_ANVIL") == "1" {
			t.Fatal("AG_REQUIRE_ANVIL=1 but AG_ANVIL_URL is not set")
		}
		t.Skip("AG_ANVIL_URL not set: start `anvil --chain-id 84532` and set it to run this test")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		t.Fatalf("AG_ANVIL_URL %q: want http to a local node", raw)
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatalf("AG_ANVIL_URL %q: only a loopback node", raw)
	}
	return raw
}

// With --token the paid tool accepts a token someone else deployed (as
// QuorumVault's lane does), deploys nothing, and a payment in that token
// settles. A --token without a contract stops the tool at start.
func TestPaidtoolUsesExistingToken(t *testing.T) {
	rpc := localAnvil(t)
	t.Setenv("AG_RPC_URL", rpc)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	node, err := x402.DialAnvil(ctx, rpc)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	other, err := node.NewChain(ctx) // stands for the party that deployed and funds the token
	if err != nil {
		t.Fatal(err)
	}
	token, err := other.DeployMockUSDC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payer, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payerAddr := crypto.PubkeyToAddress(payer.PublicKey)
	if err := other.Mint(ctx, token, payerAddr, big.NewInt(1_000_000)); err != nil {
		t.Fatal(err)
	}
	payTo := crypto.PubkeyToAddress(mustKey(t).PublicKey)

	// A token address with no contract: refused at start.
	err = paidtoolCmd(ctx, []string{"--network=anvil", "--listen=127.0.0.1:0", "--token=" + payerAddr.Hex(), "--pay-to=" + payTo.Hex()}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no contract") {
		t.Fatalf("token without code: err = %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	toolCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- paidtoolCmd(toolCtx, []string{"--network=anvil", "--listen=" + addr, "--token=" + token.Hex(), "--pay-to=" + payTo.Hex()}, io.Discard)
	}()
	defer func() { stop(); <-done }()
	base := "http://" + addr
	for i := 0; ; i++ {
		if r, err := http.Get(base + "/healthz"); err == nil {
			r.Body.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("paid tool stopped: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if i > 100 {
			t.Fatal("paid tool did not start")
		}
	}

	f := x402.Fetcher{}
	body := []byte(`{"text":"hi"}`)
	ch, _, err := f.Probe(ctx, http.MethodPost, base+"/echo", body)
	if err != nil || ch == nil {
		t.Fatalf("probe: challenge %v, err %v", ch, err)
	}
	req, err := ch.Pick("eip155:84532")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(req.Asset, token.Hex()) {
		t.Fatalf("402 asks for asset %s, want the given token %s", req.Asset, token.Hex())
	}
	amount, _ := new(big.Int).SetString(req.Amount, 10)
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	a := x402.Auth{From: payerAddr, To: payTo, Value: amount, ValidAfter: big.NewInt(0),
		ValidBefore: big.NewInt(time.Now().Unix() + 30), Nonce: nonce}
	dom := x402.Domain{Name: req.Extra.Name, Version: req.Extra.Version, ChainID: big.NewInt(int64(x402.BaseSepoliaChainID)), Contract: ecommon.HexToAddress(req.Asset)}
	sig, err := x402.Sign(payer, x402.Digest(dom, a))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Pay(ctx, http.MethodPost, base+"/echo", body, x402.PaymentPayload{X402Version: x402.Version, Accepted: req,
		Payload: x402.Payload{Signature: "0x" + ecommon.Bytes2Hex(sig), Authorization: a.Wire()}})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("paid call: HTTP %d %s", resp.StatusCode, out)
	}
	got, err := other.BalanceOf(ctx, token, payTo)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(amount) != 0 {
		t.Fatalf("payee holds %s of the given token, want %s", got, amount)
	}
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
