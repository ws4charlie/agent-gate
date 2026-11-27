package vaultsign

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"time"
)

// SetBackoff shortens the retry backoff for tests.
func (c *Client) SetBackoff(first, max time.Duration) { c.backoff0, c.backoffMax = first, max }

// NewWithoutClientCert builds a client that trusts cfg.CAFile but presents no
// client certificate, to test that Vault's refusal comes back as Config().
func NewWithoutClientCert(cfg Config) (*Client, error) {
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return newClient(cfg, &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}}), nil
}

// SetClock is for token cache tests.
func (c *ClientCredentials) SetClock(f func() time.Time) { c.now = f }
