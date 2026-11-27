package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
	"github.com/ws4charlie/agent-gate/internal/gatecred"
	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/vaultsign/vaulttest"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// AG_SIGNER=vault on a real EVM: the fake QuorumVault signs for its wallet,
// the mock USDC contract accepts that authorization on anvil, and the ledger
// charges exactly what moved. The local wallet never signs.
func TestAnvilVaultPaymentSettles(t *testing.T) {
	f := vaulttest.New(t, vaulttest.Options{})
	var vs *wallet.VaultSigner
	r := newRigWith(t, func(r *rig) wallet.Signer {
		if err := r.chain.Mint(context.Background(), r.token, f.From, big.NewInt(5_000_000)); err != nil {
			t.Fatal(err)
		}
		cli, err := vaultsign.New(vaultsign.Config{BaseURL: f.URL, WalletID: f.WalletID, ClientCert: f.ClientCert, ClientKey: f.ClientKey, CAFile: f.CAFile,
			Token: &vaultsign.ClientCredentials{TokenURL: f.TokenURL, ClientID: vaulttest.ClientID, SecretFile: f.SecretFile, HTTPClient: f.HTTPClient()}})
		if err != nil {
			t.Fatal(err)
		}
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		key, err := gatecred.NewKey("gate-e2e", priv)
		if err != nil {
			t.Fatal(err)
		}
		if vs, err = wallet.NewVaultSigner(wallet.VaultConfig{Client: cli, CredKey: key, Audience: "qv-e2e", WalletID: f.WalletID,
			From: f.From, ChainID: x402.BaseSepoliaChainID}); err != nil {
			t.Fatal(err)
		}
		vs.SetClock(r.clock.Now)
		return vs
	})
	code, m := r.pay("echo", "vault-1")
	if code != 200 {
		t.Fatalf("%d %v", code, m)
	}
	rc, err := r.node.Client.TransactionReceipt(context.Background(), common.HexToHash(m["transaction"].(string)))
	if err != nil || rc.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("receipt %v %v", rc, err)
	}
	if r.tokenBalance(r.payTo) != 10_000 || r.tokenBalance(f.From) != 5_000_000-10_000 || r.tokenBalance(r.w.Address()) != 5_000_000 {
		t.Fatalf("payee %d vault %d local %d", r.tokenBalance(r.payTo), r.tokenBalance(f.From), r.tokenBalance(r.w.Address()))
	}
	if r.w.SignCount() != 0 || f.Signs() != 1 || vs.Attempts() != 1 || len(f.Bodies()) != 1 {
		t.Fatalf("local signs %d, vault signs %d, attempts %d, bodies %d", r.w.SignCount(), f.Signs(), vs.Attempts(), len(f.Bodies()))
	}
	if got := r.entryKinds(); got != "hold,settle" {
		t.Fatalf("entries %s", got)
	}
	r.noActiveHolds()
}

// The gateway binary with AG_SIGNER=vault: starts with a complete setup (and
// an unusable AG_WALLET_KEY it must not load), and refuses to start, naming
// the setting, when one is missing.
func TestGatewayVaultSignerStartup(t *testing.T) {
	pool := dbtest.New(t)
	f := vaulttest.New(t, vaulttest.Options{})
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "cred", "gate-cred.key")
	if _, err := gatecred.NewKeyFile(keyFile); err != nil {
		t.Fatal(err)
	}
	pay := filepath.Join(dir, "payments.json")
	if err := os.WriteFile(pay, []byte(`{"network":"sim","tools":{},"chain_query_url":"http://127.0.0.1:9"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	providers := mockProviders(t, "http://127.0.0.1:9")
	prices, _ := filepath.Abs(filepath.Join(repoRoot, "configs", "prices.mock.json"))
	vaultEnv := []string{"AG_SIGNER=vault", "AG_PAYMENTS=" + pay,
		"AG_VAULT_URL=" + f.URL, "AG_VAULT_WALLET_ID=" + f.WalletID, "AG_VAULT_CERT=" + f.ClientCert, "AG_VAULT_KEY=" + f.ClientKey,
		"AG_VAULT_CA=" + f.CAFile, "AG_VAULT_TOKEN_URL=" + f.TokenURL, "AG_VAULT_CLIENT_ID=" + vaulttest.ClientID,
		"AG_VAULT_CLIENT_SECRET_FILE=" + f.SecretFile, "AG_VAULT_TOKEN_CA=" + f.CAFile,
		"AG_GATE_CRED_KEY_FILE=" + keyFile, "AG_GATE_CRED_KID=gate-e2e",
		"AG_VAULT_AUDIENCE=qv-e2e", "AG_VAULT_WALLET_ADDRESS=" + f.From.Hex(),
		"AG_WALLET_KEY=not-a-key", "AG_WALLET_KEY_FILE=" + filepath.Join(dir, "missing.key")}

	listen := freeAddr(t)
	gw := startGateway(t, append(gatewayEnv(dbURL(pool), listen, providers, prices), vaultEnv...), listen)
	want := "x402 signer: vault (wallet_id " + f.WalletID + ", audience qv-e2e, from " + f.From.Hex() + ")"
	if !strings.Contains(gw.log.String(), want) {
		t.Fatalf("no signer line %q in:\n%s", want, gw.log.String())
	}

	without := func(name string) []string {
		var env []string
		for _, kv := range append(gatewayEnv(dbURL(pool), freeAddr(t), providers, prices), vaultEnv...) {
			if !strings.HasPrefix(kv, name+"=") {
				env = append(env, kv)
			}
		}
		return env
	}
	for _, name := range []string{"AG_VAULT_AUDIENCE", "AG_VAULT_WALLET_ADDRESS", "AG_VAULT_URL", "AG_GATE_CRED_KID"} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, binary(t), "serve")
		cmd.Env = without(name)
		out, err := cmd.CombinedOutput()
		timedOut := ctx.Err() != nil
		cancel()
		if err == nil || timedOut || !strings.Contains(string(out), name) {
			t.Fatalf("without %s: err %v, output:\n%s", name, err, out)
		}
	}
}
