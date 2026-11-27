package vaultsign

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenSource gives the bearer token for Vault. Invalidate(old) drops the
// cached token after Vault answered 401 to old, so the next Token fetches a new
// one; it does nothing if the cache already holds another token, so many
// concurrent 401s cost one fetch.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate(old string)
}

// ClientCredentials is an OAuth 2 client-credentials token source (RFC 6749
// §4.4). golang.org/x/oauth2 is not in go.mod and this is one form POST, so it
// is written here instead of adding a dependency.
//
// The secret is read from SecretFile on every fetch (never kept in a field
// that could be printed), so a rotated secret is picked up without a restart.
type ClientCredentials struct {
	TokenURL   string
	ClientID   string
	SecretFile string
	Scope      string       // optional
	HTTPClient *http.Client // optional; default: CAFile or system roots, TLS 1.2+
	CAFile     string       // optional PEM bundle for the identity provider; empty: system roots
	// Early is how long before expiry a cached token is replaced. Default 30s.
	Early time.Duration

	mu       sync.Mutex
	token    string
	renewAt  time.Time // expiry minus the early margin
	inflight *flight   // the one fetch running, if any
	now      func() time.Time
	fetches  int // test hook
}

type flight struct {
	done  chan struct{}
	token string
	err   error
}

const (
	fetchTimeout = 15 * time.Second
	maxTokenTTL  = 24 * time.Hour
	defTokenTTL  = 60 * time.Second
)

// String and the formatters print no token: the struct holds one.
func (c *ClientCredentials) String() string {
	return "vaultsign.ClientCredentials(" + c.ClientID + ")"
}
func (c *ClientCredentials) GoString() string           { return c.String() }
func (c *ClientCredentials) Format(f fmt.State, _ rune) { io.WriteString(f, c.String()) }

func (c *ClientCredentials) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *ClientCredentials) Invalidate(old string) {
	c.mu.Lock()
	if c.token == old {
		c.token, c.renewAt = "", time.Time{}
	}
	c.mu.Unlock()
}

// Token returns the cached token, or fetches one. One fetch runs at a time;
// every caller waits for it under its own ctx, and the mutex is never held
// across the HTTP call. A token endpoint that refuses the client (a 4xx)
// returns ErrToken; a network error or 5xx is returned as is, so the caller
// may retry.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	for {
		c.mu.Lock()
		if c.token != "" && c.clock().Before(c.renewAt) {
			tok := c.token
			c.mu.Unlock()
			return tok, nil
		}
		f := c.inflight
		if f == nil {
			f = &flight{done: make(chan struct{})}
			c.inflight = f
			c.fetches++
			// Detached from any one caller's ctx: a caller that gives up must
			// not fail the others waiting on the same fetch.
			go c.run(f)
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-f.done:
			// The fetched token itself, not the cache: a token whose life is
			// shorter than the early margin must not send us round again.
			return f.token, f.err
		}
	}
}

func (c *ClientCredentials) run(f *flight) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	tok, ttl, err := c.fetch(ctx)
	c.mu.Lock()
	if err == nil {
		early := c.Early
		if early <= 0 {
			early = 30 * time.Second
		}
		early = min(early, ttl/2) // a short-lived token is still cached for half its life
		c.token, c.renewAt = tok, c.clock().Add(ttl-early)
	}
	f.token, f.err = tok, err
	c.inflight = nil
	c.mu.Unlock()
	close(f.done)
}

// Fetches is how many token fetches were started.
func (c *ClientCredentials) Fetches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetches
}

func (c *ClientCredentials) fetch(ctx context.Context) (string, time.Duration, error) {
	u, err := url.Parse(c.TokenURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", 0, fmt.Errorf("%w: token URL must be https", ErrToken)
	}
	raw, err := os.ReadFile(c.SecretFile)
	if err != nil {
		// The path is configuration, not a secret; the error is from the OS.
		return "", 0, fmt.Errorf("%w: reading the client secret file: %v", ErrToken, errors.Unwrap(err))
	}
	secret := strings.TrimSpace(string(raw))
	if c.ClientID == "" || secret == "" {
		return "", 0, fmt.Errorf("%w: client id or secret is empty", ErrToken)
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if c.Scope != "" {
		form.Set("scope", c.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("%w: building the token request", ErrToken)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// RFC 6749 §2.3.1: both parts form-encoded before Basic.
	req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(secret))

	hc, err := c.client()
	if err != nil {
		return "", 0, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("vaultsign: token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(body, &tr)
	switch {
	case resp.StatusCode >= 500:
		return "", 0, fmt.Errorf("vaultsign: token endpoint answered %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		// Only the status and the RFC 6749 error code: the body is not ours.
		return "", 0, fmt.Errorf("%w: %d %s", ErrToken, resp.StatusCode, safeCode(tr.Error))
	case tr.AccessToken == "" || !strings.EqualFold(tr.TokenType, "bearer"):
		return "", 0, fmt.Errorf("%w: answer has no bearer access_token", ErrToken)
	}
	return tr.AccessToken, tokenTTL(tr.ExpiresIn), nil
}

// tokenTTL clamps expires_in: optional in RFC 6749 (absent or <= 0: a short
// default), and a huge value must not overflow time.Duration.
func tokenTTL(sec int64) time.Duration {
	switch {
	case sec <= 0:
		return defTokenTTL
	case sec >= int64(maxTokenTTL/time.Second):
		return maxTokenTTL
	}
	return time.Duration(sec) * time.Second
}

// client is the HTTP client for the token endpoint. Redirects are refused
// whatever client was given: following one would send the client secret to
// another URL.
func (c *ClientCredentials) client() (*http.Client, error) {
	var hc http.Client
	if c.HTTPClient != nil {
		hc = *c.HTTPClient
	} else {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if c.CAFile != "" {
			pem, err := os.ReadFile(c.CAFile)
			if err != nil {
				return nil, fmt.Errorf("%w: reading the token CA file: %v", ErrToken, errors.Unwrap(err))
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%w: token CA file holds no PEM certificate", ErrToken)
			}
			tlsCfg.RootCAs = pool
		}
		hc = http.Client{Timeout: fetchTimeout, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	}
	hc.CheckRedirect = noRedirect
	return &hc, nil
}

// safeCode keeps an RFC 6749 error code if it looks like one, so a server
// cannot put arbitrary text into our error strings.
func safeCode(s string) string {
	if len(s) > 64 {
		return "?"
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z') {
			return "?"
		}
	}
	return s
}
