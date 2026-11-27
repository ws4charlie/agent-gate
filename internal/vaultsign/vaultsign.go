// Package vaultsign asks QuorumVault to sign one EIP-3009
// transferWithAuthorization for a Gate-governed hot wallet:
// POST <base>/v2/gate/signatures, with mTLS and an OAuth client-credentials
// token, and turns every answer into a typed result.
//
// The contract is Vault's docs/gate-credential.md ("The signing API"); this
// package implements it from the spec and imports nothing of Vault's. It does
// not mint credentials (internal/gatecred does): it takes one already minted.
//
// Safety rules kept here:
//   - A retry sends the identical bytes, and only for answers where nothing was
//     decided (503 retry_later, a gateway 502/503/504 without a Vault code, a
//     timeout, a connection error). Vault's idempotency by credential_id makes
//     that safe: never a second job, never a second signature.
//   - A 200 is used only after it verifies: the echo matches, the signature is
//     65 bytes, low s, v 27/28, and recovers to the authorization's from over
//     Gate's own digest (x402.Digest).
//   - The token, the client key and the credential's sig never reach a log
//     line or an error string.
package vaultsign

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// Path is the signing route.
const Path = "/v2/gate/signatures"

// KindEIP3009 is the only kind this client sends.
const KindEIP3009 = "evm_eip3009"

const (
	DefaultTimeout  = 30 * time.Second // Vault waits up to 20s for the zone by default
	DefaultRetryFor = 60 * time.Second
	maxBody         = 64 << 10
)

// Config is everything the client needs. Files are paths: the client key is
// read by crypto/tls and never kept in a printable field.
type Config struct {
	BaseURL    string // https://<core passthrough host>
	WalletID   string // lower-case UUID of the Gate-governed wallet
	ClientCert string // PEM file, URI SAN spiffe://quorumvault/business/agentgate
	ClientKey  string // PEM file
	CAFile     string // Vault's business CA bundle: the only roots trusted
	Token      TokenSource
	Timeout    time.Duration // per attempt; default DefaultTimeout
	RetryFor   time.Duration // total time spent retrying; default DefaultRetryFor
	Logger     *slog.Logger  // default slog.Default()
}

// Request is one signing call.
type Request struct {
	Domain       x402.Domain
	Auth         x402.Auth
	Credential   json.RawMessage // the minted credential, a JSON object with sig
	CredentialID string          // must equal the credential's credential_id
}

// Signature is r‖s‖v with v 27 or 28, verified to recover to Auth.From.
type Signature [65]byte

func (s Signature) Hex() string { return "0x" + common.Bytes2Hex(s[:]) }

// Client is safe for concurrent use.
type Client struct {
	endpoint string
	walletID string
	hc       *http.Client
	token    TokenSource
	timeout  time.Duration
	retryFor time.Duration
	log      *slog.Logger
	// Backoff between retries: starts at backoff0, doubles, at most backoffMax.
	backoff0, backoffMax time.Duration
}

// New checks the configuration and builds the client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("vaultsign: base URL must be https://host")
	}
	if !isUUID(cfg.WalletID) {
		return nil, errors.New("vaultsign: wallet id must be a lower-case UUID")
	}
	if cfg.Token == nil {
		return nil, errors.New("vaultsign: no token source")
	}
	if cfg.ClientCert == "" || cfg.ClientKey == "" || cfg.CAFile == "" {
		return nil, errors.New("vaultsign: client certificate, key and CA file are all required")
	}
	certs := &certReloader{certFile: cfg.ClientCert, keyFile: cfg.ClientKey}
	if _, err := certs.get(); err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("vaultsign: reading CA file: %v", errors.Unwrap(err))
	}
	roots := x509.NewCertPool() // Vault's CA only: no system roots
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("vaultsign: CA file holds no PEM certificate")
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return certs.get()
		},
	}
	c := newClient(cfg, &http.Transport{
		Proxy:             nil, // TLS must reach the core: no proxy from the environment
		TLSClientConfig:   tlsCfg,
		ForceAttemptHTTP2: true,
		MaxIdleConns:      4,
		IdleConnTimeout:   90 * time.Second,
	})
	return c, nil
}

func newClient(cfg Config, rt http.RoundTripper) *Client {
	c := &Client{
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + Path,
		walletID: cfg.WalletID,
		hc:       &http.Client{Transport: rt, CheckRedirect: noRedirect},
		token:    cfg.Token,
		timeout:  cfg.Timeout, retryFor: cfg.RetryFor, log: cfg.Logger,
		backoff0: 250 * time.Millisecond, backoffMax: 5 * time.Second,
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.retryFor <= 0 {
		c.retryFor = DefaultRetryFor
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c
}

// A redirect would send the bearer token and the credential somewhere else.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// certReloader reloads the client certificate when its files change: Vault's
// business issuer gives short-lived certificates that a renewer replaces.
type certReloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	cert              *tls.Certificate
	certMod, keyMod   time.Time
}

func (r *certReloader) get() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, err1 := os.Stat(r.certFile)
	ks, err2 := os.Stat(r.keyFile)
	if err1 != nil || err2 != nil {
		if r.cert != nil {
			return r.cert, nil // mid-rotation: keep the one we have
		}
		return nil, errors.New("vaultsign: client certificate or key file not readable")
	}
	if r.cert != nil && cs.ModTime().Equal(r.certMod) && ks.ModTime().Equal(r.keyMod) {
		return r.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cert != nil {
			return r.cert, nil
		}
		// tls's own error text names the PEM problem, never the key bytes.
		return nil, fmt.Errorf("vaultsign: loading client certificate: %v", err)
	}
	r.cert, r.certMod, r.keyMod = &cert, cs.ModTime(), ks.ModTime()
	return r.cert, nil
}

type signRequest struct {
	WalletID   string          `json:"wallet_id"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	Credential json.RawMessage `json:"credential"`
}

type signResponse struct {
	CredentialID string `json:"credential_id"`
	WalletID     string `json:"wallet_id"`
	Kind         string `json:"kind"`
	Signature    string `json:"signature"`
}

type refusalBody struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// call is one Sign's state, for the log line and redaction.
type call struct {
	credID   string
	secrets  []string // values that must never be printed: the sig, tokens
	attempts int
	status   int
	code     string
}

func (k *call) redact(s string) string {
	for _, v := range k.secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

// Sign asks Vault to sign the authorization. The context should carry the
// credential's expiry as its deadline: retries never run past it.
//
// Results: a verified Signature; a *Refusal (Vault decided not to sign; see
// its methods); ErrBadSignature; ErrRetryExhausted; ErrTLS or ErrToken
// (configuration); or the context's error.
func (c *Client) Sign(ctx context.Context, r Request) (sig Signature, err error) {
	start := time.Now()
	k := &call{credID: r.CredentialID}
	defer func() { c.logCall(k, start, err) }()

	body, err := c.buildBody(r, k)
	if err != nil {
		return sig, err
	}
	digest := x402.Digest(r.Domain, r.Auth)

	deadline := start.Add(c.retryFor)
	backoff := c.backoff0
	refreshed := false
	var last error
	for {
		k.attempts++
		k.code = ""
		status, resp, aerr := c.attempt(ctx, body, k)
		k.status = status
		switch {
		case aerr != nil:
			if ctx.Err() != nil {
				return sig, fmt.Errorf("vaultsign: stopped: %w", ctx.Err())
			}
			if !retryableErr(aerr) {
				return sig, aerr
			}
			last = aerr
		case status == http.StatusOK:
			return c.verify(resp, r, digest, k)
		default:
			var rb refusalBody
			jerr := json.Unmarshal(resp, &rb)
			k.code = rb.Code
			if status == http.StatusUnauthorized && !refreshed {
				// The token may have expired early or been revoked: one fresh
				// token, same body. A second 401 is final.
				refreshed = true
				c.token.Invalidate()
				continue
			}
			nothingDecided := rb.Code == CodeRetryLater ||
				(jerr != nil || rb.Code == "") && (status == 502 || status == 503 || status == 504)
			if !nothingDecided {
				ref := &Refusal{Status: status, Code: rb.Code, Field: rb.Field, Message: k.redact(rb.Message)}
				if jerr != nil || rb.Code == "" {
					ref.Message = ""
				}
				if !ref.Known() {
					raw := k.redact(string(resp))
					if len(raw) > 512 {
						raw = raw[:512] + "…"
					}
					c.log.Warn("vaultsign: unknown answer, treated as final",
						"credential_id", k.credID, "status", status, "code", rb.Code, "body", raw)
				}
				return sig, ref
			}
			last = fmt.Errorf("vault answered %d %s", status, rb.Code)
		}
		// Nothing was decided: wait, then send the same bytes again.
		wake := time.Now().Add(backoff)
		if wake.After(deadline) {
			return sig, fmt.Errorf("%w after %d attempts: %s", ErrRetryExhausted, k.attempts, k.redact(last.Error()))
		}
		if d, ok := ctx.Deadline(); ok && wake.After(d) {
			return sig, fmt.Errorf("vaultsign: stopped before the credential expires: %w", context.DeadlineExceeded)
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return sig, fmt.Errorf("vaultsign: stopped: %w", ctx.Err())
		case <-t.C:
		}
		backoff = min(backoff*2, c.backoffMax)
	}
}

func (c *Client) buildBody(r Request, k *call) ([]byte, error) {
	var cred map[string]json.RawMessage
	if err := json.Unmarshal(r.Credential, &cred); err != nil || cred == nil {
		return nil, errors.New("vaultsign: credential must be a JSON object")
	}
	var sigText, credID, walletID string
	_ = json.Unmarshal(cred["sig"], &sigText)
	_ = json.Unmarshal(cred["credential_id"], &credID)
	_ = json.Unmarshal(cred["wallet_id"], &walletID)
	k.secrets = append(k.secrets, sigText)
	if r.CredentialID == "" || credID != r.CredentialID {
		return nil, errors.New("vaultsign: CredentialID differs from the credential's credential_id")
	}
	if walletID != c.walletID {
		return nil, errors.New("vaultsign: the credential is for another wallet than this client's")
	}
	if sigText == "" {
		return nil, errors.New("vaultsign: the credential is not signed")
	}
	if r.Domain.ChainID == nil || r.Auth.Value == nil || r.Auth.ValidAfter == nil || r.Auth.ValidBefore == nil {
		return nil, errors.New("vaultsign: incomplete domain or authorization")
	}
	body, err := json.Marshal(signRequest{WalletID: c.walletID, Kind: KindEIP3009,
		Payload: Payload(r.Domain, r.Auth), Credential: r.Credential})
	if err != nil {
		return nil, errors.New("vaultsign: credential is not valid JSON")
	}
	return body, nil
}

// attempt sends body once. The returned error is a transport error (or a
// token error); a Vault answer of any status is (status, body, nil).
func (c *Client) attempt(ctx context.Context, body []byte, k *call) (int, []byte, error) {
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	tok, err := c.token.Token(actx)
	if err != nil {
		if certErr(err) {
			return 0, nil, fmt.Errorf("%w (token endpoint): %v", ErrTLS, err)
		}
		return 0, nil, err
	}
	k.secrets = append(k.secrets, tok)
	req, err := http.NewRequestWithContext(actx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("vaultsign: building request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.hc.Do(req)
	if err != nil {
		if certErr(err) {
			return 0, nil, fmt.Errorf("%w: %v", ErrTLS, err)
		}
		return 0, nil, fmt.Errorf("vaultsign: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, fmt.Errorf("vaultsign: reading answer: %v", err)
	}
	return resp.StatusCode, b, nil
}

// certErr: a certificate problem on either side. Retrying cannot fix it.
// crypto/tls reports an alert from the server (certificate required, bad
// certificate, unknown CA) as a net.OpError with Op "remote error" around an
// unexported alert type; tls.AlertError is only for QUIC.
func certErr(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" {
		return true
	}
	var alert tls.AlertError
	var verr *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	return errors.As(err, &alert) || errors.As(err, &verr) || errors.As(err, &ua) ||
		errors.As(err, &hn) || errors.As(err, &ci)
}

func retryableErr(err error) bool {
	return !errors.Is(err, ErrTLS) && !errors.Is(err, ErrToken)
}

var secp256k1HalfN = new(big.Int).Rsh(func() *big.Int {
	n, _ := new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)
	return n
}(), 1)

func (c *Client) verify(resp []byte, r Request, digest common.Hash, k *call) (Signature, error) {
	var sig Signature
	var sr signResponse
	if err := json.Unmarshal(resp, &sr); err != nil {
		return sig, fmt.Errorf("%w: answer is not JSON", ErrBadSignature)
	}
	switch {
	case sr.CredentialID != r.CredentialID:
		return sig, fmt.Errorf("%w: credential_id echo differs", ErrBadSignature)
	case sr.WalletID != c.walletID:
		return sig, fmt.Errorf("%w: wallet_id echo differs", ErrBadSignature)
	case sr.Kind != KindEIP3009:
		return sig, fmt.Errorf("%w: kind echo differs", ErrBadSignature)
	}
	if !strings.HasPrefix(sr.Signature, "0x") {
		return sig, fmt.Errorf("%w: signature is not 0x hex", ErrBadSignature)
	}
	raw := common.FromHex(sr.Signature)
	if len(raw) != 65 || len(sr.Signature) != 2+130 {
		return sig, fmt.Errorf("%w: signature is not 65 bytes", ErrBadSignature)
	}
	if v := raw[64]; v != 27 && v != 28 {
		return sig, fmt.Errorf("%w: v is %d, not 27 or 28", ErrBadSignature, v)
	}
	if new(big.Int).SetBytes(raw[32:64]).Cmp(secp256k1HalfN) > 0 {
		return sig, fmt.Errorf("%w: s is high", ErrBadSignature)
	}
	signer, err := x402.Recover(digest, raw)
	if err != nil {
		return sig, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	if signer != r.Auth.From {
		return sig, fmt.Errorf("%w: recovers to %s, not the authorization's from %s", ErrBadSignature, signer.Hex(), r.Auth.From.Hex())
	}
	copy(sig[:], raw)
	return sig, nil
}

// logCall writes the one line per call. Only identifiers, status, code and
// counts: never the token, the credential or its sig.
func (c *Client) logCall(k *call, start time.Time, err error) {
	attrs := []any{"credential_id", k.credID, "wallet_id", c.walletID,
		"status", k.status, "code", k.code, "attempts", k.attempts,
		"ms", time.Since(start).Milliseconds()}
	if err == nil {
		c.log.Info("vaultsign: signed", attrs...)
		return
	}
	var ref *Refusal
	if errors.As(err, &ref) && ref.Config() {
		attrs = append(attrs, "config", true)
	}
	c.log.Warn("vaultsign: not signed", append(attrs, "err", k.redact(err.Error()))...)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return false
			}
		}
	}
	return true
}
