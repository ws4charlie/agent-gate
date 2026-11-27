package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/gatecred"
	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/vaultsign/vaulttest"
	"github.com/ws4charlie/agent-gate/internal/wallet"
)

// vaultSignerEnv sets a complete AG_SIGNER=vault environment against a fake
// Vault, plus a local wallet key that must never be read: an unreadable key
// file and a garbage AG_WALLET_KEY.
func vaultSignerEnv(t *testing.T) map[string]string {
	t.Helper()
	f := vaulttest.New(t, vaulttest.Options{})
	keyFile := filepath.Join(t.TempDir(), "cred", "gate-cred.key")
	if _, err := gatecred.NewKeyFile(keyFile); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		EnvSigner:                     "vault",
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
		wallet.EnvKey:                 "not-a-key",
		wallet.EnvKeyFile:             filepath.Join(t.TempDir(), "missing.key"),
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	return env
}

func TestVaultSignerFromEnv(t *testing.T) {
	env := vaultSignerEnv(t)
	pay, err := loadPayments(PaymentsFile{Network: "sim", MaxTxUSD: "0.25"})
	if err != nil {
		t.Fatalf("vault mode must not need AG_WALLET_KEY: %v", err)
	}
	v, ok := pay.Wallet.(*wallet.VaultSigner)
	if !ok || !strings.EqualFold(v.Address().Hex(), env[EnvVaultWalletAddress]) || v.Network() != "eip155:1337" || v.Audience() != "qv-test" {
		t.Fatalf("signer %#v", pay.Wallet)
	}
	// The same environment in wallet mode does read the (bad) key: proof that
	// vault mode skipped it rather than tolerating it.
	t.Setenv(EnvSigner, "wallet")
	if _, err := loadPayments(PaymentsFile{Network: "sim"}); err == nil {
		t.Fatal("wallet mode accepted a garbage AG_WALLET_KEY")
	}
}

// AC7: any missing setting stops the gateway with its name.
func TestVaultSignerRefusesIncompleteSetup(t *testing.T) {
	for _, name := range vaultEnv {
		t.Run(name, func(t *testing.T) {
			vaultSignerEnv(t)
			t.Setenv(name, "")
			_, err := loadPayments(PaymentsFile{Network: "sim"})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("missing %s: %v", name, err)
			}
		})
	}
	bad := []struct{ name, val, want string }{
		{EnvSigner, "hsm", "want wallet or vault"},
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
			if _, err := loadPayments(PaymentsFile{Network: "sim"}); err == nil || !strings.Contains(err.Error(), b.want) {
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
