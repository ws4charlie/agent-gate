// Package vaulttest is a fake QuorumVault signing API for tests: an httptest
// TLS server that requires Gate's client certificate and a bearer token,
// stores answers by credential_id as Vault does, plays scripted answers, and
// signs with a test secp256k1 key. Certificates, keys and the client secret
// are generated at run time into t.TempDir(); nothing is committed.
//
// It is written from Vault's spec (docs/gate-credential.md, "The signing
// API"), independently of package vaultsign (which it does not import): it
// hashes the payload with go-ethereum's apitypes, not with x402.Digest, so the
// two cross-check.
package vaulttest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// GateSPIFFE is the URI SAN Vault requires on Gate's client certificate.
const GateSPIFFE = "spiffe://quorumvault/business/agentgate"

// ClientID is the fake identity provider's client id for Gate.
const ClientID = "agentgate-test"

// Step is one scripted answer, used by the next request that passes the
// certificate and token checks and is not answered from the store.
type Step struct {
	// Status and Code: a refusal (stored, except retry_later).
	Status      int
	Code, Field string
	// Drop aborts the connection before deciding anything.
	Drop bool
	// DropAfterSign signs and stores the answer, then aborts the connection:
	// a lost response.
	DropAfterSign bool
	// Mutate changes the 65 signature bytes of a 200 before they are sent.
	Mutate func(sig []byte) []byte
	// SignWith signs with another key (a wrong signer).
	SignWith *ecdsa.PrivateKey
	// Body replaces the whole 200 body (Status 0) or the refusal body.
	Body string
}

type stored struct {
	canon  []byte
	status int
	body   []byte
}

// Vault is the fake. All files it writes are under a t.TempDir().
type Vault struct {
	URL      string // base URL, https://127.0.0.1:port
	TokenURL string
	WalletID string
	Key      *ecdsa.PrivateKey // the wallet's key; its address is From
	From     common.Address

	// Files for the client's Config.
	CAFile, ClientCert, ClientKey, SecretFile string
	// A certificate from the same CA with another URI SAN.
	OtherCert, OtherKey string

	srv     *httptest.Server
	secret  string
	mu      sync.Mutex
	steps   []Step
	store   map[string]stored
	tokens  map[string]bool
	bodies  [][]byte
	signs   int
	fetches int
	// TokenTTL is the expires_in the fake IdP gives (default 300).
	TokenTTL int
}

// Options change how the fake starts.
type Options struct {
	// RequireCertAtTLS makes the TLS handshake itself refuse a client without
	// a certificate (a real ingress may do this) instead of an HTTP 403.
	RequireCertAtTLS bool
}

// New starts the fake. It stops with the test.
func New(t testing.TB, opt Options) *Vault {
	t.Helper()
	dir := t.TempDir()
	v := &Vault{WalletID: "6f1c2b3a-0d4e-4f5a-8b6c-7d8e9f0a1b2c", store: map[string]stored{},
		tokens: map[string]bool{}, TokenTTL: 300}
	var err error
	if v.Key, err = crypto.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	v.From = crypto.PubkeyToAddress(v.Key.PublicKey)

	ca, caKey := newCA(t)
	v.CAFile = writePEM(t, dir, "ca.crt", "CERTIFICATE", ca.Raw)
	serverCert := issue(t, ca, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "qv-core"},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	v.ClientCert, v.ClientKey = writeLeaf(t, dir, "gate", issue(t, ca, caKey, clientTemplate(GateSPIFFE)))
	v.OtherCert, v.OtherKey = writeLeaf(t, dir, "other", issue(t, ca, caKey, clientTemplate("spiffe://quorumvault/business/someone-else")))

	v.secret = randHex(t, 24)
	v.SecretFile = filepath.Join(dir, "client-secret")
	if err := os.WriteFile(v.SecretFile, []byte(v.secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/gate/signatures", v.sign)
	v.srv = httptest.NewUnstartedServer(mux)
	auth := tls.VerifyClientCertIfGiven
	if opt.RequireCertAtTLS {
		auth = tls.RequireAndVerifyClientCert
	}
	v.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert},
		ClientAuth: auth, ClientCAs: pool}
	v.srv.EnableHTTP2 = true
	v.srv.Config.ErrorLog = log.New(io.Discard, "", 0) // handshake refusals are expected
	v.srv.StartTLS()
	t.Cleanup(v.srv.Close)
	v.URL = v.srv.URL

	// The identity provider is another server (it does not ask for Gate's
	// client certificate), with a certificate from the same test CA.
	idpMux := http.NewServeMux()
	idpMux.HandleFunc("POST /token", v.token)
	idp := httptest.NewUnstartedServer(idpMux)
	idp.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}}
	idp.StartTLS()
	t.Cleanup(idp.Close)
	v.TokenURL = idp.URL + "/token"
	return v
}

// HTTPClient trusts the fake's CA, for the fake identity provider.
func (v *Vault) HTTPClient() *http.Client {
	ca, _ := os.ReadFile(v.CAFile)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}}}
}

// Secret is the client secret (for leak tests: it must appear nowhere).
func (v *Vault) Secret() string { return v.secret }

// Script queues answers.
func (v *Vault) Script(steps ...Step) {
	v.mu.Lock()
	v.steps = append(v.steps, steps...)
	v.mu.Unlock()
}

// RevokeTokens makes every issued token invalid (the next call gets 401).
func (v *Vault) RevokeTokens() {
	v.mu.Lock()
	v.tokens = map[string]bool{}
	v.mu.Unlock()
}

// Tokens returns every token ever issued that is still valid.
func (v *Vault) Tokens() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	for k := range v.tokens {
		out = append(out, k)
	}
	return out
}

// Bodies is every request body the signing route received, in order.
func (v *Vault) Bodies() [][]byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([][]byte(nil), v.bodies...)
}

// Signs is how many signatures the fake made.
func (v *Vault) Signs() int { v.mu.Lock(); defer v.mu.Unlock(); return v.signs }

// TokenFetches is how many tokens the fake IdP issued.
func (v *Vault) TokenFetches() int { v.mu.Lock(); defer v.mu.Unlock(); return v.fetches }

func (v *Vault) token(w http.ResponseWriter, r *http.Request) {
	id, sec, ok := r.BasicAuth()
	id, _ = url.QueryUnescape(id)
	sec, _ = url.QueryUnescape(sec)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "client_credentials" {
		writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	if !ok || id != ClientID || sec != v.secret {
		writeJSON(w, 401, map[string]string{"error": "invalid_client"})
		return
	}
	tok := "fake-token-" + randHexNoT(24)
	v.mu.Lock()
	v.tokens[tok] = true
	v.fetches++
	ttl := v.TokenTTL
	v.mu.Unlock()
	writeJSON(w, 200, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": ttl})
}

func refuse(w http.ResponseWriter, status int, code, field, msg string) []byte {
	m := map[string]string{"code": code, "message": msg}
	if field != "" {
		m["field"] = field
	}
	b, _ := json.Marshal(m)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
	return b
}

func (v *Vault) sign(w http.ResponseWriter, r *http.Request) {
	body, _ := readAll(r)
	v.mu.Lock()
	v.bodies = append(v.bodies, body)
	v.mu.Unlock()

	if !hasGateCert(r) {
		refuse(w, 403, "client_not_authorized", "", "client certificate is not AgentGate's")
		return
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	v.mu.Lock()
	okTok := v.tokens[tok]
	v.mu.Unlock()
	if !okTok {
		refuse(w, 401, "client_not_authorized", "", "no valid token")
		return
	}

	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil || len(req) != 4 || req["wallet_id"] == nil ||
		req["kind"] == nil || req["payload"] == nil || req["credential"] == nil {
		refuse(w, 400, "request_invalid", "", "request must have exactly wallet_id, kind, payload, credential")
		return
	}
	var walletID, kind string
	json.Unmarshal(req["wallet_id"], &walletID)
	json.Unmarshal(req["kind"], &kind)
	if kind != "evm_eip3009" {
		refuse(w, 400, "request_invalid", "kind", "kind")
		return
	}
	if walletID != v.WalletID {
		refuse(w, 403, "wallet_not_gate_governed", "", "wallet is not Gate-governed")
		return
	}
	var cred map[string]json.RawMessage
	if json.Unmarshal(req["credential"], &cred) != nil || cred == nil {
		refuse(w, 400, "credential_malformed", "", "credential must be an object")
		return
	}
	var credID string
	json.Unmarshal(cred["credential_id"], &credID)
	if credID == "" {
		refuse(w, 400, "credential_malformed", "credential_id", "missing")
		return
	}
	canon := canonical(body)

	v.mu.Lock()
	if s, ok := v.store[credID]; ok {
		v.mu.Unlock()
		if !bytes.Equal(s.canon, canon) {
			refuse(w, 409, "replayed", "", "credential_id already used with another request")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		w.Write(s.body)
		return
	}
	var step Step
	if len(v.steps) > 0 {
		step, v.steps = v.steps[0], v.steps[1:]
	}
	v.mu.Unlock()

	switch {
	case step.Drop:
		panic(http.ErrAbortHandler)
	case step.Status != 0:
		var b []byte
		if step.Body != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(step.Status)
			w.Write([]byte(step.Body))
			b = []byte(step.Body)
		} else {
			b = refuse(w, step.Status, step.Code, step.Field, "scripted "+step.Code)
		}
		// Vault stores decisions only: every 4xx and signing_failed. A
		// retry_later or a gateway 5xx decided nothing.
		if step.Status < 500 || step.Code == "signing_failed" {
			v.save(credID, canon, step.Status, b)
		}
		return
	}

	// Sign what the payload says, as Vault does: rebuild the typed data.
	var td apitypes.TypedData
	if err := json.Unmarshal(req["payload"], &td); err != nil || td.PrimaryType != "TransferWithAuthorization" {
		b := refuse(w, 422, "payload_invalid", "", "payload")
		v.save(credID, canon, 422, b)
		return
	}
	if from, _ := td.Message["from"].(string); !strings.EqualFold(from, v.From.Hex()) {
		b := refuse(w, 422, "payload_invalid", "from", "from is not the wallet's address")
		v.save(credID, canon, 422, b)
		return
	}
	hash, _, err := apitypes.TypedDataAndHash(td)
	if err != nil {
		b := refuse(w, 422, "payload_invalid", "", "typed data")
		v.save(credID, canon, 422, b)
		return
	}
	key := v.Key
	if step.SignWith != nil {
		key = step.SignWith
	}
	sig, err := crypto.Sign(hash, key)
	if err != nil {
		b := refuse(w, 503, "signing_failed", "", "signer")
		v.save(credID, canon, 503, b)
		return
	}
	sig[64] += 27
	if step.Mutate != nil {
		sig = step.Mutate(sig)
	}
	out, _ := json.Marshal(map[string]string{"credential_id": credID, "wallet_id": walletID,
		"kind": "evm_eip3009", "signature": "0x" + hex.EncodeToString(sig)})
	if step.Body != "" {
		out = []byte(step.Body)
	}
	v.mu.Lock()
	v.signs++
	v.mu.Unlock()
	v.save(credID, canon, 200, out)
	if step.DropAfterSign {
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(out)
}

func (v *Vault) save(id string, canon []byte, status int, body []byte) {
	v.mu.Lock()
	v.store[id] = stored{canon: canon, status: status, body: body}
	v.mu.Unlock()
}

// canonical: the layout of the JSON does not matter to Vault's idempotency.
func canonical(b []byte) []byte {
	var x any
	if json.Unmarshal(b, &x) != nil {
		return b
	}
	out, _ := json.Marshal(x) // maps marshal with sorted keys
	return out
}

func hasGateCert(r *http.Request) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	for _, u := range r.TLS.PeerCertificates[0].URIs {
		if u.String() == GateSPIFFE {
			return true
		}
	}
	return false
}

func readAll(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, 1<<20))
	return buf.Bytes(), err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func newCA(t testing.TB) (*x509.Certificate, *ecdsa.PrivateKey) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake business CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return ca, key
}

func clientTemplate(spiffe string) *x509.Certificate {
	u, _ := url.Parse(spiffe)
	return &x509.Certificate{Subject: pkix.Name{CommonName: "client"}, URIs: []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
}

var serial = big.NewInt(1)
var serialMu sync.Mutex

func issue(t testing.TB, ca *x509.Certificate, caKey *ecdsa.PrivateKey, tmpl *x509.Certificate) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serialMu.Lock()
	serial = new(big.Int).Add(serial, big.NewInt(1))
	tmpl.SerialNumber = serial
	serialMu.Unlock()
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writeLeaf(t testing.TB, dir, name string, c tls.Certificate) (certFile, keyFile string) {
	kder, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return writePEM(t, dir, name+".crt", "CERTIFICATE", c.Certificate[0]),
		writePEM(t, dir, name+".key", "PRIVATE KEY", kder)
}

func writePEM(t testing.TB, dir, name, typ string, der []byte) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func randHex(t testing.TB, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func randHexNoT(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
