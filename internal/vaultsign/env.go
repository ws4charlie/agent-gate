package vaultsign

import (
	"fmt"
	"os"
	"strings"
)

// Environment variables. The secret is never in the environment itself: only
// the path to a file holding it.
const (
	EnvURL              = "AG_VAULT_URL"
	EnvWalletID         = "AG_VAULT_WALLET_ID"
	EnvCert             = "AG_VAULT_CERT"
	EnvKey              = "AG_VAULT_KEY"
	EnvCA               = "AG_VAULT_CA"
	EnvTokenURL         = "AG_VAULT_TOKEN_URL"
	EnvClientID         = "AG_VAULT_CLIENT_ID"
	EnvClientSecretFile = "AG_VAULT_CLIENT_SECRET_FILE"
	// EnvTokenCA is optional: the identity provider's CA bundle (empty:
	// system roots). It is not part of the all-or-nothing set.
	EnvTokenCA = "AG_VAULT_TOKEN_CA"
)

var envNames = []string{EnvURL, EnvWalletID, EnvCert, EnvKey, EnvCA, EnvTokenURL, EnvClientID, EnvClientSecretFile}

// ConfigFromEnv reads the AG_VAULT_* variables. None set: (nil, nil), Vault
// signing is off. All set: the Config, with a ClientCredentials token source.
// Some set: an error naming the missing ones, so a half-done setup is loud.
func ConfigFromEnv() (*Config, error) {
	vals := map[string]string{}
	var missing []string
	for _, n := range envNames {
		v := strings.TrimSpace(os.Getenv(n))
		vals[n] = v
		if v == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) == len(envNames) {
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("vaultsign: Vault signing is partly configured; also set %s", strings.Join(missing, ", "))
	}
	return &Config{
		BaseURL: vals[EnvURL], WalletID: vals[EnvWalletID],
		ClientCert: vals[EnvCert], ClientKey: vals[EnvKey], CAFile: vals[EnvCA],
		Token: &ClientCredentials{TokenURL: vals[EnvTokenURL], ClientID: vals[EnvClientID], SecretFile: vals[EnvClientSecretFile],
			CAFile: strings.TrimSpace(os.Getenv(EnvTokenCA))},
	}, nil
}
