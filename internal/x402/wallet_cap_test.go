package x402_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// I10, defence in depth: policy here approves $0.50 (its scope has no
// per-transaction limit, as if the engine were bypassed or misconfigured), and
// the wallet still refuses because its own cap is lower. Nothing is signed.
func TestWalletCapFiresWhenPolicyAllowsMore(t *testing.T) {
	r := newRig(t)
	req, res := r.challenge()
	req.Amount = "500000"                // $0.50
	a := r.allowedFor("h", 500_000*1000) // policy says yes
	r.w.SetMaxTx(100_000 * 1000)         // wallet's own cap: $0.10
	_, err := r.w.Prepare(context.Background(), a, "h", res, req)
	if !errors.Is(err, wallet.ErrOverTxCap) {
		t.Fatalf("want ErrOverTxCap, got %v", err)
	}
	if r.w.SignCount() != 0 {
		t.Fatal("signed above the cap")
	}
	// The refused Prepare must not burn the approval: a payment inside the cap
	// is not possible with this Allowed (amount differs), but the Allowed is unspent.
	r.w.SetMaxTx(500_000 * 1000)
	if _, err := r.w.Prepare(context.Background(), a, "h", res, req); err != nil {
		t.Fatalf("approval was burned by the refused attempt: %v", err)
	}
}

func TestWalletCapCannotBeSwitchedOff(t *testing.T) {
	key, _ := crypto.GenerateKey()
	w, _ := wallet.New(key, x402.LocalChainID)
	w.SetMaxTx(0)
	w.SetMaxTx(-5)
	if w.MaxTx() != wallet.DefaultMaxTx {
		t.Fatalf("cap moved to %v", w.MaxTx())
	}
}

func TestWalletRefusesEveryOtherChain(t *testing.T) {
	key, _ := crypto.GenerateKey()
	for _, id := range []uint64{0, 1, 10, 137, 8453, 84531, 11155111} {
		if _, err := wallet.New(key, id); !errors.Is(err, wallet.ErrRefusedChain) {
			t.Errorf("chain %d accepted", id)
		}
	}
}

// The key never appears in formatted output or log lines.
func TestWalletKeyNeverFormatted(t *testing.T) {
	key, _ := crypto.GenerateKey()
	w, _ := wallet.New(key, x402.LocalChainID)
	secret := common.Bytes2Hex(crypto.FromECDSA(key))
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	lg.Info("wallet", "w", w)
	out := buf.String() + fmt.Sprintf("%v %+v %#v %s", w, w, w, w)
	if strings.Contains(strings.ToLower(out), strings.ToLower(secret)) || strings.Contains(out, key.D.String()) {
		t.Fatal("key leaked into formatted output")
	}
	if !strings.Contains(out, w.Address().Hex()) {
		t.Fatalf("expected the address in %q", out)
	}
}
