package vaultsign

import (
	"context"
	"crypto/tls"
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

// TokenSource gives the bearer token for Vault. Invalidate drops a cached
// token after Vault answered 401, so the next Token fetches a new one.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
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
	HTTPClient *http.Client // optional; default: system roots, TLS 1.2+
	// Early is how long before expiry a cached token is replaced. Default 30s.
	Early time.Duration

	mu     sync.Mutex
	token  string
	expiry time.Time
	now    func() time.Time
}

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

func (c *ClientCredentials) Invalidate() {
	c.mu.Lock()
	c.token, c.expiry = "", time.Time{}
	c.mu.Unlock()
}

// Token returns the cached token, or fetches one. A token endpoint that
// refuses the client (a 4xx) returns ErrToken; a network error or 5xx is
// returned as is, so the caller may retry.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	early := c.Early
	if early <= 0 {
		early = 30 * time.Second
	}
	if c.token != "" && c.clock().Add(early).Before(c.expiry) {
		return c.token, nil
	}
	tok, ttl, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	c.token, c.expiry = tok, c.clock().Add(ttl)
	return tok, nil
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

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		}}
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
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if tr.ExpiresIn <= 0 {
		ttl = time.Minute // RFC 6749 makes expires_in optional; do not cache long
	}
	return tr.AccessToken, ttl, nil
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
