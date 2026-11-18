package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/server"
)

// testCA is a throwaway SPIFFE trust domain: a CA that issues SVIDs.
type testCA struct {
	td   spiffeid.TrustDomain
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, td string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: td + " test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{td: spiffeid.RequireTrustDomainFromString(td), cert: cert, key: key}
}

func (ca *testCA) bundle() *x509bundle.Bundle {
	return x509bundle.FromX509Authorities(ca.td, []*x509.Certificate{ca.cert})
}

// svid issues a leaf certificate whose only URI SAN is id.
func (ca *testCA) svid(t *testing.T, id string) *x509svid.SVID {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), URIs: []*url.URL{u},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &x509svid.SVID{ID: spiffeid.RequireFromString(id), Certificates: []*x509.Certificate{leaf}, PrivateKey: key}
}

// clientTo returns an HTTP client that presents sv and trusts only ca.
func (ca *testCA) clientTo(sv *x509svid.SVID) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	cert := tls.Certificate{PrivateKey: sv.PrivateKey}
	for _, c := range sv.Certificates {
		cert.Certificate = append(cert.Certificate, c.Raw)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, Certificates: []tls.Certificate{cert},
		// The gateway's SVID has a URI SAN, not a DNS name, so the name check is
		// replaced by the chain check above; the client side is not under test.
		InsecureSkipVerify: true, //nolint:gosec
	}}}
}

const (
	spiffeRunner = "spiffe://example.org/runner"
	spiffeOther  = "spiffe://example.org/other"
)

type spiffeEnv struct {
	*env
	ca    *testCA
	mtls  *httptest.Server
	agent agents.Agent
}

// newSPIFFEEnv starts the gateway's plain listener (e.gw) and an mTLS one for
// trust domain example.org, and maps spiffeRunner to a fresh agent.
func newSPIFFEEnv(t *testing.T) *spiffeEnv {
	t.Helper()
	e := newEnv(t, 5*usd, nil)
	ca := newTestCA(t, "example.org")
	ts := httptest.NewUnstartedServer(server.SPIFFEHandler(e.srv.Handler()))
	ts.TLS = server.ServerTLSConfig(ca.svid(t, "spiffe://example.org/gateway"), ca.bundle(), ca.td)
	ts.StartTLS()
	t.Cleanup(ts.Close)

	ctx := context.Background()
	_, a, err := agents.NewStore(e.pool).Create(ctx, e.acct, "runner", policy.DefaultScope())
	if err != nil {
		t.Fatal(err)
	}
	if a, err = agents.NewStore(e.pool).SetSpiffeID(ctx, a.ID, spiffeRunner); err != nil {
		t.Fatal(err)
	}
	return &spiffeEnv{env: e, ca: ca, mtls: ts, agent: a}
}

func balanceAs(t *testing.T, c *http.Client, base, bearer string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/balance", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out.AgentID
}

// (a) a valid, mapped SVID is the agent, whatever API key it also sends.
func TestSPIFFEAuthenticatesMappedAgent(t *testing.T) {
	e := newSPIFFEEnv(t)
	c := e.ca.clientTo(e.ca.svid(t, spiffeRunner))
	for name, key := range map[string]string{"no key": "", "wrong key": "ag_wrong", "other agent's key": e.secret} {
		code, id := balanceAs(t, c, e.mtls.URL, key, nil)
		if code != 200 || id != e.agent.ID {
			t.Errorf("%s: got %d agent %q, want 200 agent %q", name, code, id, e.agent.ID)
		}
	}
}

// (b) an SVID from another trust domain never gets past the TLS handshake.
func TestSPIFFEForeignTrustDomainRejected(t *testing.T) {
	e := newSPIFFEEnv(t)
	foreign := newTestCA(t, "evil.org")
	c := foreign.clientTo(foreign.svid(t, "spiffe://evil.org/runner"))
	req, _ := http.NewRequest(http.MethodGet, e.mtls.URL+"/v1/balance", nil)
	req.Header.Set("Authorization", "Bearer "+e.secret)
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("foreign SVID got status %d, want a TLS failure", resp.StatusCode)
	}
	// Same trust domain name, different CA: the chain check fails too.
	impostor := newTestCA(t, "example.org")
	c = impostor.clientTo(impostor.svid(t, spiffeRunner))
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("SVID from an untrusted CA got status %d, want a TLS failure", resp.StatusCode)
	}
}

// (c) a valid SVID that no agent owns is a plain 401, even with a good key.
func TestSPIFFEUnmappedIsUnauthorized(t *testing.T) {
	e := newSPIFFEEnv(t)
	c := e.ca.clientTo(e.ca.svid(t, spiffeOther))
	if code, _ := balanceAs(t, c, e.mtls.URL, e.secret, nil); code != 401 {
		t.Fatalf("unmapped SVID got %d, want 401", code)
	}
}

// (d) on the plain listener, only the API key counts; nothing else can name
// an identity.
func TestSPIFFEPlainListenerCannotFakeIdentity(t *testing.T) {
	e := newSPIFFEEnv(t)
	fake := map[string]string{"X-Spiffe-Id": spiffeRunner, "X-Forwarded-Client-Cert": "URI=" + spiffeRunner,
		"X-SPIFFE-ID": spiffeRunner, "X-Client-Cert": spiffeRunner}
	if code, _ := balanceAs(t, http.DefaultClient, e.gw.URL, "", fake); code != 401 {
		t.Fatalf("spoofed headers without a key got %d, want 401", code)
	}
	// With a real key of another agent, the request is that agent, not the runner.
	code, id := balanceAs(t, http.DefaultClient, e.gw.URL, e.secret, fake)
	if code != 200 || id == e.agent.ID {
		t.Fatalf("got %d agent %q: headers must not change who the caller is", code, id)
	}
}

// With the key off, the key is refused (401) and the SVID still works.
func TestAPIKeyDisabledRefusesKeyKeepsSPIFFE(t *testing.T) {
	e := newSPIFFEEnv(t)
	ctx := context.Background()
	st := agents.NewStore(e.pool)
	secret, a, err := st.Create(ctx, e.acct, "both", policy.DefaultScope())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetSpiffeID(ctx, a.ID, spiffeOther); err != nil {
		t.Fatal(err)
	}
	if code, id := balanceAs(t, http.DefaultClient, e.gw.URL, secret, nil); code != 200 || id != a.ID {
		t.Fatalf("key on: got %d %q", code, id)
	}
	if _, err := st.SetAPIKeyEnabled(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	if code, _ := balanceAs(t, http.DefaultClient, e.gw.URL, secret, nil); code != 401 {
		t.Fatalf("key off: got %d, want 401", code)
	}
	c := e.ca.clientTo(e.ca.svid(t, spiffeOther))
	if code, id := balanceAs(t, c, e.mtls.URL, secret, nil); code != 200 || id != a.ID {
		t.Fatalf("key off, SVID: got %d %q, want 200 %q", code, id, a.ID)
	}
	if _, err := st.SetAPIKeyEnabled(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if code, _ := balanceAs(t, http.DefaultClient, e.gw.URL, secret, nil); code != 200 {
		t.Fatalf("key back on: got %d, want 200", code)
	}
}

// The span says how the caller proved who it is, and carries no credential.
func TestSPIFFESpanRecordsAuthMethod(t *testing.T) {
	e := newSPIFFEEnv(t)
	post := func(c *http.Client, base, key string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(chatBody("oa/m", false)))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	post(e.ca.clientTo(e.ca.svid(t, spiffeRunner)), e.mtls.URL, "")
	if got := attrs(rootSpan(t, e.env, "chat oa/m"))["agentgate.auth"].AsString(); got != "spiffe" {
		t.Errorf("agentgate.auth over mTLS = %q, want spiffe", got)
	}
	e.spans.Reset()
	post(http.DefaultClient, e.gw.URL, e.secret)
	if got := attrs(rootSpan(t, e.env, "chat oa/m"))["agentgate.auth"].AsString(); got != "api_key" {
		t.Errorf("agentgate.auth on the plain listener = %q, want api_key", got)
	}
	noContent(t, e.env, e.secret, "BEGIN CERTIFICATE")
}

// Handler and WatchKills both fill in the server's defaults; serve starts them
// together. Under -race this fails if defaults() is not safe to call twice at once.
func TestDefaultsSafeFromTwoGoroutines(t *testing.T) {
	e := newEnv(t, 0, nil)
	s := &server.Server{Pool: e.pool, Ledger: e.led, Audit: e.audit}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.WatchKills(ctx) }()
	_ = s.Handler()
	cancel()
	<-done
}
