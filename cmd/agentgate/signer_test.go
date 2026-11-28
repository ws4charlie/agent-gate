package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/gatecred"
	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/vaultsign/vaulttest"
	"github.com/ws4charlie/agent-gate/internal/wallet"
)

// vaultSignerEnv sets a complete QuorumVault environment against a fake Vault.
func vaultSignerEnv(t *testing.T) map[string]string {
	t.Helper()
	f := vaulttest.New(t, vaulttest.Options{})
	keyFile := filepath.Join(t.TempDir(), "cred", "gate-cred.key")
	if _, err := gatecred.NewKeyFile(keyFile); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		vaultsign.EnvURL:              f.URL,
		vaultsign.EnvWalletID:         f.WalletID,
		vaultsign.EnvCert:             f.ClientCert,
		vaultsign.EnvKey:              f.ClientKey,
		vaultsign.EnvCA:               f.CAFile,
		vaultsign.EnvTokenURL:         f.TokenURL,
		vaultsign.EnvClientID:         vaulttest.ClientID,
		vaultsign.EnvClientSecretFile: f.SecretFile,
		vaultsign.EnvTokenCA:          f.CAFile,
		gatecred.EnvKeyFile:           keyFile,
		gatecred.EnvKid:               "gate-test-1",
		EnvVaultAudience:              "qv-test",
		EnvVaultWalletAddress:         f.From.Hex(),
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	return env
}

// anvil and base-sepolia sign through QuorumVault, whatever else is set.
func TestVaultSignerFromEnv(t *testing.T) {
	env := vaultSignerEnv(t)
	for _, network := range []string{"anvil", "base-sepolia"} {
		pay, err := loadPayments(PaymentsFile{Network: network, MaxTxUSD: "0.25"})
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		v, ok := pay.Wallet.(*wallet.VaultSigner)
		if !ok || !strings.EqualFold(v.Address().Hex(), env[EnvVaultWalletAddress]) || v.Network() != "eip155:84532" || v.Audience() != "qv-test" {
			t.Fatalf("%s: signer %#v", network, pay.Wallet)
		}
	}
}

// sim signs with a throw-away key made at start: nothing to configure, a new
// address every start, and never a Vault signer even when Vault is set up.
func TestSimSignerIsEphemeral(t *testing.T) {
	for _, withVault := range []bool{false, true} {
		if withVault {
			vaultSignerEnv(t)
		}
		a, err := loadPayments(PaymentsFile{Network: "sim", MaxTxUSD: "0.25"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := loadPayments(PaymentsFile{Network: "sim"})
		if err != nil {
			t.Fatal(err)
		}
		w, ok := a.Wallet.(*wallet.Wallet)
		if !ok || w.Network() != "eip155:1337" || w.MaxTx() != 250_000_000 {
			t.Fatalf("vault set %v: signer %#v", withVault, a.Wallet)
		}
		if a.Wallet.Address() == b.Wallet.Address() {
			t.Fatal("two starts reused one sim key")
		}
	}
}

// The local-key signer's settings are gone: each one, set to anything, stops
// the gateway with its name and a pointer to the Vault settings.
func TestRetiredSignerEnvRefused(t *testing.T) {
	if len(retiredEnv) != 3 {
		t.Fatalf("retiredEnv = %v", retiredEnv)
	}
	for _, name := range retiredEnv {
		t.Setenv(name, "")
	}
	if err := refuseRetiredEnv(); err != nil {
		t.Fatalf("nothing set: %v", err)
	}
	for _, name := range retiredEnv {
		for _, val := range []string{"wallet", "vault", " ", "/tmp/w.key"} {
			t.Run(name+"="+val, func(t *testing.T) {
				t.Setenv(name, val)
				err := refuseRetiredEnv()
				if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "AG_VAULT_") {
					t.Fatalf("got %v", err)
				}
				// serve refuses before it touches the database or anything else.
				t.Setenv("DATABASE_URL", "postgres://127.0.0.1:1/none")
				if err := serve(context.Background(), nil, io.Discard); err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("serve: %v", err)
				}
			})
		}
	}
}

// AC7: any missing setting stops the gateway with its name.
func TestVaultSignerRefusesIncompleteSetup(t *testing.T) {
	for _, name := range vaultEnv {
		t.Run(name, func(t *testing.T) {
			vaultSignerEnv(t)
			t.Setenv(name, "")
			_, err := loadPayments(PaymentsFile{Network: "anvil"})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("missing %s: %v", name, err)
			}
		})
	}
	bad := []struct{ name, val, want string }{
		{EnvVaultWalletAddress, "0x1234", "not a 0x address"},
		{EnvVaultCredTTL, "5s", EnvVaultCredTTL},
		{EnvVaultCredTTL, "120s", EnvVaultCredTTL},
		{EnvVaultCredTTL, "ninety", EnvVaultCredTTL},
		{gatecred.EnvKeyFile, "/nonexistent/gate-cred.key", gatecred.EnvKeyFile},
		{vaultsign.EnvWalletID, "not-a-uuid", "wallet id"},
	}
	for _, b := range bad {
		t.Run(b.name+"="+b.val, func(t *testing.T) {
			vaultSignerEnv(t)
			t.Setenv(b.name, b.val)
			if _, err := loadPayments(PaymentsFile{Network: "anvil"}); err == nil || !strings.Contains(err.Error(), b.want) {
				t.Fatalf("got %v, want it to mention %q", err, b.want)
			}
		})
	}
	t.Run("mainnet", func(t *testing.T) {
		vaultSignerEnv(t)
		if _, err := loadPayments(PaymentsFile{Network: "base"}); err == nil {
			t.Fatal("unknown network accepted")
		}
	})
}

func TestPolicyVersion(t *testing.T) {
	if v, _ := policyVersion("", false); v != "go1" {
		t.Fatal(v)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.cedar"), []byte("permit(principal, action, resource);"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := policyVersion(dir, true)
	b, _ := policyVersion(dir, false)
	if !strings.HasPrefix(a, "go1+cedar-enforce:") || !strings.HasPrefix(b, "go1+cedar-shadow:") || len(a) > 64 || a[len(a)-12:] != b[len(b)-12:] {
		t.Fatalf("%q %q", a, b)
	}
	_ = os.WriteFile(filepath.Join(dir, "a.cedar"), []byte("forbid(principal, action, resource);"), 0o600)
	if c, _ := policyVersion(dir, true); c == a {
		t.Fatal("version did not change with the policy")
	}
}

// fix1 #4: only the Vault signer needs a policy version; the sim startup
// reads no policy files for it, so a policy dir it cannot hash changes nothing.
func TestPolicyVersionOnlyForVault(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "broken.cedar"), 0o700); err != nil { // a *.cedar that cannot be read
		t.Fatal(err)
	}
	pay, err := loadPayments(PaymentsFile{Network: "sim"})
	if err != nil {
		t.Fatal(err)
	}
	if err := setPolicyVersion(pay, dir, true); err != nil || pay.PolicyVersion != "" {
		t.Fatalf("sim: %v %q", err, pay.PolicyVersion)
	}

	vaultSignerEnv(t)
	vpay, err := loadPayments(PaymentsFile{Network: "anvil"})
	if err != nil {
		t.Fatal(err)
	}
	if err := setPolicyVersion(vpay, dir, true); err == nil {
		t.Fatal("vault mode hashed an unreadable policy")
	}
	if err := setPolicyVersion(vpay, "", false); err != nil || vpay.PolicyVersion != "go1" {
		t.Fatalf("vault mode: %v %q", err, vpay.PolicyVersion)
	}
}
