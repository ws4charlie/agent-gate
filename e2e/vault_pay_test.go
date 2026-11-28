package e2e

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
	"github.com/ws4charlie/agent-gate/internal/gatecred"
	"github.com/ws4charlie/agent-gate/internal/vaultsign/vaulttest"
)

// The gateway binary on anvil: it signs through QuorumVault with a complete
// setup, refuses to start, naming the setting, when one is missing, and
// refuses to start when a retired local-key setting is still set. On sim it
// starts with nothing to configure and a throw-away key.
func TestGatewayVaultSignerStartup(t *testing.T) {
	pool := dbtest.New(t)
	rpc := anvilURL(t)
	f := vaulttest.New(t, vaulttest.Options{})
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "cred", "gate-cred.key")
	if _, err := gatecred.NewKeyFile(keyFile); err != nil {
		t.Fatal(err)
	}
	pay := filepath.Join(dir, "payments.json")
	if err := os.WriteFile(pay, []byte(`{"network":"anvil","tools":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	providers := mockProviders(t, "http://127.0.0.1:9")
	prices, _ := filepath.Abs(filepath.Join(repoRoot, "configs", "prices.mock.json"))
	vaultEnv := []string{"AG_PAYMENTS=" + pay, "AG_RPC_URL=" + rpc,
		"AG_VAULT_URL=" + f.URL, "AG_VAULT_WALLET_ID=" + f.WalletID, "AG_VAULT_CERT=" + f.ClientCert, "AG_VAULT_KEY=" + f.ClientKey,
		"AG_VAULT_CA=" + f.CAFile, "AG_VAULT_TOKEN_URL=" + f.TokenURL, "AG_VAULT_CLIENT_ID=" + vaulttest.ClientID,
		"AG_VAULT_CLIENT_SECRET_FILE=" + f.SecretFile, "AG_VAULT_TOKEN_CA=" + f.CAFile,
		"AG_GATE_CRED_KEY_FILE=" + keyFile, "AG_GATE_CRED_KID=gate-e2e",
		"AG_VAULT_AUDIENCE=qv-e2e", "AG_VAULT_WALLET_ADDRESS=" + f.From.Hex()}

	listen := freeAddr(t)
	gw := startGateway(t, append(gatewayEnv(dbURL(pool), listen, providers, prices), vaultEnv...), listen)
	want := "x402 signer: vault (wallet_id " + f.WalletID + ", audience qv-e2e, from " + f.From.Hex() + ")"
	// Stop it first: Cmd.Wait returns only after the output is copied, so the
	// log buffer is no longer written while it is read.
	_ = gw.cmd.Process.Kill()
	_ = gw.cmd.Wait()
	if !strings.Contains(gw.log.String(), want) {
		t.Fatalf("no signer line %q in:\n%s", want, gw.log.String())
	}

	// refused runs serve with env and wants it to exit, naming name.
	refused := func(env []string, name string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary(t), "serve")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil || ctx.Err() != nil || !strings.Contains(string(out), name) {
			t.Fatalf("%s: err %v, output:\n%s", name, err, out)
		}
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
		refused(without(name), name)
	}
	// An old deployment's settings stop the gateway even beside a complete
	// Vault setup: it must never start with a signer it did not ask for.
	for _, kv := range []string{"AG_SIGNER=wallet", "AG_SIGNER=vault", "AG_WALLET_KEY=not-a-key", "AG_WALLET_KEY_FILE=" + filepath.Join(dir, "w.key")} {
		refused(append(without(""), kv), strings.SplitN(kv, "=", 2)[0])
	}

	// sim: no Vault, no key file; one log line with the throw-away address,
	// and the key is never written: the gateway's working directory and temp
	// directory (both empty at start) stay empty.
	simPay := filepath.Join(dir, "sim.json")
	if err := os.WriteFile(simPay, []byte(`{"network":"sim","tools":{},"chain_query_url":"http://127.0.0.1:9"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	work, tmp := t.TempDir(), t.TempDir()
	listen = freeAddr(t)
	gw = startGatewayIn(t, work, append(gatewayEnv(dbURL(pool), listen, providers, prices), "AG_PAYMENTS="+simPay, "TMPDIR="+tmp), listen)
	_ = gw.cmd.Process.Kill()
	_ = gw.cmd.Wait()
	if l := gw.log.String(); !strings.Contains(l, "x402 signer: ephemeral sim key (no on-chain value)") || !strings.Contains(l, "network=eip155:1337") {
		t.Fatalf("no sim signer line in:\n%s", l)
	}
	for _, d := range []string{work, tmp} {
		_ = filepath.WalkDir(d, func(p string, _ fs.DirEntry, err error) error {
			if err == nil && p != d {
				t.Errorf("the sim gateway wrote %s", p)
			}
			return nil
		})
	}
}
