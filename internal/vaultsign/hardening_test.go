package vaultsign_test

// Edge cases of the Vault client: expiry, token refresh, certificates, status mapping, log injection.

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/vaultsign/vaulttest"
)

// Retries never run past the credential's expiry.
func TestCredentialExpiry(t *testing.T) {
	t.Run("expired credential is never sent", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		r := e.requestExp(-5 * time.Second)
		if _, err := e.sign(r); !errors.Is(err, vaultsign.ErrExpired) {
			t.Fatalf("got %v", err)
		}
		if n := len(e.fake.Bodies()); n != 0 {
			t.Fatalf("sent %d requests", n)
		}
	})
	t.Run("retries stop at expires_at even with a long ctx and RetryFor", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		for range 2000 {
			e.fake.Script(vaulttest.Step{Status: 503, Code: "retry_later"})
		}
		e.cfg.RetryFor = time.Hour
		e.cli, _ = vaultsign.New(e.cfg)
		e.cli.SetBackoff(5*time.Millisecond, 20*time.Millisecond)
		r := e.requestExp(time.Second) // expires 1-2 s from now
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		start := time.Now()
		_, err := e.signCtx(ctx, r)
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, vaultsign.ErrExpired) {
			t.Fatalf("got %v", err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("retried for %v past a credential that expired within 2s", d)
		}
	})
	t.Run("a hanging attempt is cut at expires_at, not at Timeout", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.fake.Script(vaulttest.Step{Delay: time.Minute})
		e.cfg.Timeout, e.cfg.RetryFor = time.Minute, time.Hour
		e.cli, _ = vaultsign.New(e.cfg)
		start := time.Now()
		_, err := e.sign(e.requestExp(time.Second))
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
			t.Fatalf("got %v after %v", err, time.Since(start))
		}
	})
	t.Run("signature whose validBefore has passed is not handed out", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		r := e.request()
		now := time.Now().Unix()
		r.Auth.ValidAfter, r.Auth.ValidBefore = big.NewInt(now-20), big.NewInt(now-1)
		if _, err := e.sign(r); !errors.Is(err, vaultsign.ErrExpired) {
			t.Fatalf("got %v", err)
		}
	})
}

// N concurrent 401s cost exactly one token fetch.
func TestTokenRefreshStorm(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	mustSign(t, e, e.request())
	e.fake.RevokeTokens()
	const n = 20
	reqs := make([]vaultsign.Request, n)
	for i := range reqs {
		reqs[i] = e.request()
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.cli.Sign(context.Background(), reqs[i])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := e.fake.TokenFetches(); got != 2 {
		t.Fatalf("token fetches %d, want 2 (warm-up + one refresh)", got)
	}
}

func TestInvalidateOnlyOld(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	ts := e.cfg.Token.(*vaultsign.ClientCredentials)
	a, _ := ts.Token(context.Background())
	ts.Invalidate("some-older-token")
	b, _ := ts.Token(context.Background())
	if a != b || ts.Fetches() != 1 {
		t.Fatal("Invalidate of another token dropped the cache")
	}
	ts.Invalidate(a)
	c, _ := ts.Token(context.Background())
	if c == a || ts.Fetches() != 2 {
		t.Fatal("Invalidate(current) did not drop the cache")
	}
}

func TestTokenWaiterHonoursOwnContext(t *testing.T) {
	block := make(chan struct{})
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer idp.Close()
	defer close(block)
	dir := t.TempDir()
	os.WriteFile(dir+"/s", []byte("x"), 0o600)
	ts := &vaultsign.ClientCredentials{TokenURL: idp.URL + "/token", ClientID: "c", SecretFile: dir + "/s", HTTPClient: idp.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := ts.Token(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("got %v after %v", err, time.Since(start))
	}
}

// A token that lives shorter than the early margin is still returned (and
// cached for half its life), not fetched in a loop.
func TestShortLivedToken(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	e.fake.TokenTTL = 10 // Early is 30s
	ts := e.cfg.Token.(*vaultsign.ClientCredentials)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := ts.Token(ctx); err != nil {
		t.Fatal(err)
	}
	ts.Token(ctx)
	if ts.Fetches() != 1 {
		t.Fatalf("fetches %d", ts.Fetches())
	}
}

// Redirects, token CA, expires_in clamp.
func TestTokenEndpointHardening(t *testing.T) {
	t.Run("redirect is refused, secret not sent on", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		redir := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, e.fake.TokenURL, http.StatusTemporaryRedirect)
		}))
		defer redir.Close()
		hc := redir.Client()
		hc.Transport.(*http.Transport).TLSClientConfig.RootCAs.AppendCertsFromPEM(mustRead(t, e.fake.CAFile))
		ts := &vaultsign.ClientCredentials{TokenURL: redir.URL + "/token", ClientID: vaulttest.ClientID,
			SecretFile: e.fake.SecretFile, HTTPClient: hc}
		if _, err := ts.Token(context.Background()); !errors.Is(err, vaultsign.ErrToken) {
			t.Fatalf("got %v", err)
		}
		if e.fake.TokenFetches() != 0 {
			t.Fatal("redirect was followed to the identity provider")
		}
	})
	t.Run("CAFile trusts a private identity provider", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		ts := &vaultsign.ClientCredentials{TokenURL: e.fake.TokenURL, ClientID: vaulttest.ClientID,
			SecretFile: e.fake.SecretFile, CAFile: e.fake.CAFile}
		if _, err := ts.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("non-https token URL refused", func(t *testing.T) {
		ts := &vaultsign.ClientCredentials{TokenURL: "http://idp.example.com/token", ClientID: "c", SecretFile: "/nonexistent"}
		if _, err := ts.Token(context.Background()); !errors.Is(err, vaultsign.ErrToken) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("huge expires_in is clamped to 24h, not overflowed", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.fake.TokenTTL = 1 << 62
		ts := e.cfg.Token.(*vaultsign.ClientCredentials)
		now := time.Now()
		ts.SetClock(func() time.Time { return now })
		ts.Token(context.Background())
		now = now.Add(23 * time.Hour)
		ts.Token(context.Background())
		if ts.Fetches() != 1 {
			t.Fatalf("fetches %d after 23h: TTL overflowed", ts.Fetches())
		}
		now = now.Add(2 * time.Hour)
		ts.Token(context.Background())
		if ts.Fetches() != 2 {
			t.Fatal("token kept past 24h")
		}
	})
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Certificate validation and reloads.
func TestClientCertificate(t *testing.T) {
	t.Run("New refuses another identity", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.cfg.ClientCert, e.cfg.ClientKey = e.fake.OtherCert, e.fake.OtherKey
		if _, err := vaultsign.New(e.cfg); err == nil || !strings.Contains(err.Error(), "URI SAN") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("New refuses an expired certificate", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.cfg.ClientCert, e.cfg.ClientKey, _ = e.fake.IssueClient(t, vaulttest.GateSPIFFE, time.Now().Add(-time.Minute), "", "")
		if _, err := vaultsign.New(e.cfg); !errors.Is(err, vaultsign.ErrTLS) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("New refuses a key that does not match", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.cfg.ClientKey = e.fake.OtherKey
		if _, err := vaultsign.New(e.cfg); err == nil {
			t.Fatal("mismatched key accepted")
		}
	})
	t.Run("current certificate expired: ErrTLS before sending", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		mustSign(t, e, e.request()) // a pooled connection now exists
		e.cli.SetCertClock(func() time.Time { return time.Now().Add(48 * time.Hour) })
		if _, err := e.sign(e.request()); !errors.Is(err, vaultsign.ErrTLS) {
			t.Fatalf("got %v", err)
		}
		if n := len(e.fake.Bodies()); n != 1 {
			t.Fatalf("sent %d requests, want only the first", n)
		}
	})
	t.Run("bad reload is logged and the current certificate kept; good reload picked up", func(t *testing.T) {
		f := vaulttest.New(t, vaulttest.Options{})
		dir := t.TempDir()
		cf, kf := dir+"/tls.crt", dir+"/tls.key"
		_, _, serial1 := f.IssueClient(t, vaulttest.GateSPIFFE, time.Time{}, cf, kf)
		logs := &syncBuf{}
		cfg := config(f, logs)
		cfg.ClientCert, cfg.ClientKey = cf, kf
		cli, err := vaultsign.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		e := &env{t: t, fake: f, cli: cli, cfg: cfg, logs: logs}
		if cli.CertSerial() != serial1.String() {
			t.Fatal("serial")
		}
		os.WriteFile(cf, []byte("not a certificate"), 0o600)
		mustSign(t, e, e.request())
		if !strings.Contains(logs.String(), "reload failed") || cli.CertSerial() != serial1.String() {
			t.Fatalf("bad reload not logged or cert dropped: %s", logs.String())
		}
		_, _, serial2 := f.IssueClient(t, vaulttest.GateSPIFFE, time.Time{}, cf, kf)
		mustSign(t, e, e.request())
		if cli.CertSerial() != serial2.String() {
			t.Fatal("rotated certificate not picked up")
		}
	})
}

// Status/code mapping.
func TestStatusMapping(t *testing.T) {
	retried := map[string]vaulttest.Step{
		"429":                   {Status: 429, Body: `{"message":"slow down"}`},
		"500 html":              {Status: 500, Body: "<html>oops</html>"},
		"500 json without code": {Status: 500, Body: `{"message":"x"}`},
	}
	for name, step := range retried {
		t.Run(name+" is retried", func(t *testing.T) {
			e := newEnv(t, vaulttest.Options{})
			e.fake.Script(step)
			mustSign(t, e, e.request())
			identical(t, e.fake.Bodies(), 2)
		})
	}
	final := map[string]vaulttest.Step{
		"retry_later with 400":  {Status: 400, Code: "retry_later"},
		"500 with unknown code": {Status: 500, Code: "internal"},
	}
	for name, step := range final {
		t.Run(name+" is final", func(t *testing.T) {
			e := newEnv(t, vaulttest.Options{})
			e.fake.Script(step)
			_, err := e.sign(e.request())
			var ref *vaultsign.Refusal
			if !errors.As(err, &ref) || ref.Known() || !ref.Final() {
				t.Fatalf("got %v", err)
			}
			identical(t, e.fake.Bodies(), 1)
		})
	}
}

// uint256 range and window.
func TestAuthRange(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	over := new(big.Int).Lsh(big.NewInt(1), 256)
	cases := map[string]func(r *vaultsign.Request){
		"negative value":    func(r *vaultsign.Request) { r.Auth.Value = big.NewInt(-1) },
		"value 2^256":       func(r *vaultsign.Request) { r.Auth.Value = over },
		"validBefore 2^256": func(r *vaultsign.Request) { r.Auth.ValidBefore = over },
		"negative chain":    func(r *vaultsign.Request) { r.Domain.ChainID = big.NewInt(-1) },
		"after > before":    func(r *vaultsign.Request) { r.Auth.ValidAfter = new(big.Int).Add(r.Auth.ValidBefore, big.NewInt(1)) },
		"after == before":   func(r *vaultsign.Request) { r.Auth.ValidAfter = new(big.Int).Set(r.Auth.ValidBefore) },
		"nil value":         func(r *vaultsign.Request) { r.Auth.Value = nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := e.request()
			mut(&r)
			if _, err := e.sign(r); err == nil || !strings.HasPrefix(err.Error(), "vaultsign: ") {
				t.Fatalf("got %v", err)
			}
		})
	}
	if n := len(e.fake.Bodies()); n != 0 {
		t.Fatalf("sent %d bad requests", n)
	}
}

// Vault-supplied text cannot forge log lines or error lines.
func TestNoLogInjection(t *testing.T) {
	f := vaulttest.New(t, vaulttest.Options{})
	logs := &syncBuf{}
	cfg := config(f, logs)
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	cli, _ := vaultsign.New(cfg)
	e := &env{t: t, fake: f, cli: cli, cfg: cfg, logs: logs}
	forged := "\ntime=x level=ERROR msg=forged\r\n"
	f.Script(vaulttest.Step{Status: 403, Body: `{"code":"weird` + `\n","message":"a` + `\n` + `level=ERROR msg=forged"}`})
	_, err := e.sign(e.request())
	f.Script(vaulttest.Step{Status: 403, Code: "fast_lane_cap", Body: `{"code":"fast_lane_cap","message":` + quoteJSON(forged) + `}`})
	_, err2 := e.sign(e.request())
	for _, s := range []string{err.Error(), err2.Error()} {
		if strings.ContainsAny(s, "\r\n") {
			t.Fatalf("error string has a line break: %q", s)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.HasPrefix(line, "time=x") || strings.Contains(line, "level=ERROR") && !strings.Contains(line, "vaultsign") {
			t.Fatalf("forged log line: %q", line)
		}
	}
}

func quoteJSON(s string) string {
	return `"` + strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(s) + `"`
}
