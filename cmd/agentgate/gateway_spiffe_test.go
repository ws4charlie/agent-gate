package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func testSVID(t *testing.T, id string) *x509svid.SVID {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse(id)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"}, URIs: []*url.URL{u},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &x509svid.SVID{ID: spiffeid.RequireFromString(id), Certificates: []*x509.Certificate{cert}, PrivateKey: key}
}

func TestPickSVIDChoosesByID(t *testing.T) {
	gw := testSVID(t, "spiffe://example.org/gateway")
	rn := testSVID(t, "spiffe://example.org/runner")
	all := []*x509svid.SVID{rn, gw}

	if pickSVID(spiffeid.ID{}) != nil {
		t.Fatal("no id must keep the default picker (first SVID)")
	}
	if got := pickSVID(gw.ID)(all); got != gw {
		t.Fatalf("picked %v, want the gateway SVID", got.ID)
	}
	if got := pickSVID(rn.ID)(all); got != rn {
		t.Fatalf("picked %v, want the runner SVID", got.ID)
	}
	if got := pickSVID(spiffeid.RequireFromString("spiffe://example.org/other"))(all); got != nil {
		t.Fatalf("picked %v for an ID nobody has, want none", got.ID)
	}
}

type fakeSource struct {
	svid   *x509svid.SVID
	closed atomic.Bool
}

func (f *fakeSource) GetX509SVID() (*x509svid.SVID, error) { return f.svid, nil }
func (f *fakeSource) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return x509bundle.FromX509Authorities(td, f.svid.Certificates), nil
}
func (f *fakeSource) Close() error { f.closed.Store(true); return nil }

// The listener retries until SPIRE has an SVID, then serves; stopping closes it.
func TestSPIFFEListenerRetriesThenServes(t *testing.T) {
	var tries atomic.Int32
	src := &fakeSource{svid: testSVID(t, "spiffe://example.org/gateway")}
	addr := freeAddr(t)
	l := &spiffeListener{
		open: func(context.Context) (spiffeSource, error) {
			if tries.Add(1) < 3 {
				return nil, errors.New("no SVID yet")
			}
			return src, nil
		},
		addr: addr, tds: []spiffeid.TrustDomain{spiffeid.RequireTrustDomainFromString("example.org")},
		handler: http.NotFoundHandler(), log: slog.New(slog.NewTextHandler(io.Discard, nil)), stdout: io.Discard,
		minWait: 5 * time.Millisecond, maxWait: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener never opened (tries=%d): %v", tries.Load(), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := tries.Load(); n != 3 {
		t.Errorf("opened after %d tries, want 3", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if !src.closed.Load() {
		t.Error("source not closed")
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Error("listener still open after stop")
	}
}

// The wait between tries doubles up to maxWait and never beyond.
func TestSPIFFEListenerBackoffIsCapped(t *testing.T) {
	var stamps []time.Time
	l := &spiffeListener{
		open: func(context.Context) (spiffeSource, error) {
			stamps = append(stamps, time.Now())
			return nil, errors.New("down")
		},
		tds: []spiffeid.TrustDomain{spiffeid.RequireTrustDomainFromString("example.org")}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		stdout: io.Discard, minWait: 5 * time.Millisecond, maxWait: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	l.run(ctx)
	if len(stamps) < 8 {
		t.Fatalf("only %d tries in 300ms with a 20ms cap", len(stamps))
	}
	for i := 5; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap > 120*time.Millisecond {
			t.Errorf("gap %v between tries exceeds the cap", gap)
		}
	}
}

// With --spiffe-listen set and no Workload API at all, serve starts and the
// plain listener answers; cancelling the context stops it cleanly.
func TestServeSurvivesUnreachableSPIRE(t *testing.T) {
	t.Setenv("DATABASE_URL", dbtest.New(t).Config().ConnString())
	plain := freeAddr(t)
	t.Setenv("AG_LISTEN", plain)
	t.Setenv("AG_CONFIG", "")
	args := []string{"--providers", "../../configs/providers.mock.json", "--prices", "../../configs/prices.mock.json",
		"--spiffe-listen", freeAddr(t), "--spiffe-trust-domain", "example.org",
		"--spiffe-socket", "unix:///nonexistent/spire/agent.sock", "--spiffe-id", "spiffe://example.org/gateway"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	res := make(chan error, 1)
	go func() { res <- serve(ctx, args, &out) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://" + plain + "/v1/balance")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("plain listener status %d, want 401 (no key)", resp.StatusCode)
			}
			break
		}
		select {
		case err := <-res:
			t.Fatalf("serve returned early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("plain listener never answered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal(fmt.Sprint("serve did not stop; output: ", out.String()))
	}
}

func TestParseTrustDomains(t *testing.T) {
	got, err := parseTrustDomains("agentgate.internal, acme.example,agentgate.internal")
	if err != nil || len(got) != 2 || got[0].Name() != "agentgate.internal" || got[1].Name() != "acme.example" {
		t.Fatalf("got %v, %v", got, err)
	}
	one, err := parseTrustDomains("example.org")
	if err != nil || len(one) != 1 {
		t.Fatalf("single: %v, %v", one, err)
	}
	for _, bad := range []string{"", "a.example,", ",a.example", "not a domain"} {
		if _, err := parseTrustDomains(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
