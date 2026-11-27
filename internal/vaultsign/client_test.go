package vaultsign_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"

	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/vaultsign/vaulttest"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// syncBuf is a log sink safe for concurrent writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	t    *testing.T
	fake *vaulttest.Vault
	cli  *vaultsign.Client
	cfg  vaultsign.Config
	logs *syncBuf
	sigs []string // every credential sig used, for leak checks
	errs []string // every error string returned
}

func config(f *vaulttest.Vault, logs *syncBuf) vaultsign.Config {
	return vaultsign.Config{
		BaseURL: f.URL, WalletID: f.WalletID,
		ClientCert: f.ClientCert, ClientKey: f.ClientKey, CAFile: f.CAFile,
		Token: &vaultsign.ClientCredentials{TokenURL: f.TokenURL, ClientID: vaulttest.ClientID,
			SecretFile: f.SecretFile, HTTPClient: f.HTTPClient()},
		Timeout: 2 * time.Second, RetryFor: 2 * time.Second,
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func newEnv(t *testing.T, opt vaulttest.Options) *env {
	t.Helper()
	f := vaulttest.New(t, opt)
	logs := &syncBuf{}
	cfg := config(f, logs)
	cli, err := vaultsign.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cli.SetBackoff(5*time.Millisecond, 40*time.Millisecond)
	return &env{t: t, fake: f, cli: cli, cfg: cfg, logs: logs}
}

// request builds a fresh authorization from the fake's wallet and a fake
// credential (this package does not verify it; Vault does).
func (e *env) request() vaultsign.Request { return e.requestExp(120 * time.Second) }

// requestExp: the credential expires ttl from now (whole seconds, rounded up).
func (e *env) requestExp(ttl time.Duration) vaultsign.Request {
	now := time.Now().Unix()
	exp := time.Now().Add(ttl).Unix()
	if time.Now().Add(ttl).After(time.Unix(exp, 0)) {
		exp++
	}
	var nonce [32]byte
	rand.Read(nonce[:])
	id := uuid.NewString()
	sigBytes := make([]byte, 64)
	rand.Read(sigBytes)
	sig := base64.RawURLEncoding.EncodeToString(sigBytes)
	e.sigs = append(e.sigs, sig)
	cred, _ := json.Marshal(map[string]any{"v": 1, "kid": "gate-test-1", "credential_id": id, "obligation_id": "ob-1",
		"audience": "qv-test", "wallet_id": e.fake.WalletID, "chain": "eip155:84532",
		"asset": strings.ToLower(x402.USDCBaseSepolia), "payee": "0x2222222222222222222222222222222222222222",
		"amount": "1500", "issued_at": now, "expires_at": exp, "policy_version": "p1", "decision_reason": "ok", "sig": sig})
	return vaultsign.Request{
		Domain: x402.Domain{Name: "USDC", Version: "2", ChainID: big.NewInt(84532), Contract: common.HexToAddress(x402.USDCBaseSepolia)},
		Auth: x402.Auth{From: e.fake.From, To: common.HexToAddress("0x2222222222222222222222222222222222222222"),
			Value: big.NewInt(1500), ValidAfter: big.NewInt(now), ValidBefore: big.NewInt(now + 100), Nonce: nonce},
		Credential: cred, CredentialID: id,
	}
}

func (e *env) sign(r vaultsign.Request) (vaultsign.Signature, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return e.signCtx(ctx, r)
}

func (e *env) signCtx(ctx context.Context, r vaultsign.Request) (vaultsign.Signature, error) {
	s, err := e.cli.Sign(ctx, r)
	if err != nil {
		e.errs = append(e.errs, err.Error())
	}
	return s, err
}

func mustSign(t *testing.T, e *env, r vaultsign.Request) vaultsign.Signature {
	t.Helper()
	s, err := e.sign(r)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if got, _ := x402.Recover(x402.Digest(r.Domain, r.Auth), s[:]); got != e.fake.From {
		t.Fatalf("signature recovers to %s", got.Hex())
	}
	return s
}

func TestSignSuccess(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	r := e.request()
	mustSign(t, e, r)
	if e.fake.Signs() != 1 || len(e.fake.Bodies()) != 1 {
		t.Fatalf("signs %d bodies %d", e.fake.Signs(), len(e.fake.Bodies()))
	}
	// Body has exactly the four members, credential as an object.
	var body map[string]json.RawMessage
	json.Unmarshal(e.fake.Bodies()[0], &body)
	if len(body) != 4 || body["credential"][0] != '{' || string(body["kind"]) != `"evm_eip3009"` {
		t.Fatalf("body %s", e.fake.Bodies()[0])
	}
	// The same request again is answered from the store: same signature, no new job.
	s2 := mustSign(t, e, r)
	if e.fake.Signs() != 1 {
		t.Fatalf("signed twice")
	}
	_ = s2
	if !strings.Contains(e.logs.String(), `"credential_id":"`+r.CredentialID) {
		t.Fatalf("no log line: %s", e.logs.String())
	}
}

// Acceptance 2: every code of the spec's table.
func TestRefusalTable(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	for code, statuses := range vaultsign.KnownCodes {
		for _, status := range statuses {
			if code == vaultsign.CodeRetryLater || status == 401 {
				continue // retried inside Sign; tested below
			}
			t.Run(fmt.Sprintf("%d_%s", status, code), func(t *testing.T) {
				e.fake.Script(vaulttest.Step{Status: status, Code: code, Field: "payee"})
				r := e.request()
				_, err := e.sign(r)
				var ref *vaultsign.Refusal
				if !errors.As(err, &ref) {
					t.Fatalf("want *Refusal, got %v", err)
				}
				if ref.Status != status || ref.Code != code || ref.Field != "payee" || !ref.Known() {
					t.Fatalf("parsed %+v", ref)
				}
				if !ref.Final() || !ref.NeedsNewNonce() {
					t.Fatal("every refusal is final and burns the nonce")
				}
				wantCfg := code == "wallet_not_gate_governed" || code == "audience_mismatch" || code == "kid_unknown" || code == "client_not_authorized"
				if ref.Config() != wantCfg {
					t.Fatalf("Config() = %v", ref.Config())
				}
				// Stored: the same request gets the same refusal, without a new step.
				_, err2 := e.sign(r)
				var ref2 *vaultsign.Refusal
				if !errors.As(err2, &ref2) || *ref2 != *ref {
					t.Fatalf("repeat: %v", err2)
				}
			})
		}
	}
	// An unknown code is final and logged with its (redacted) body.
	e.fake.Script(vaulttest.Step{Status: 418, Code: "teapot"})
	_, err := e.sign(e.request())
	var ref *vaultsign.Refusal
	if !errors.As(err, &ref) || ref.Known() || !ref.Final() || ref.Config() {
		t.Fatalf("unknown: %v", err)
	}
	if !strings.Contains(e.logs.String(), "unknown answer") {
		t.Fatal("unknown code not logged")
	}
	if e.fake.Signs() != 0 {
		t.Fatal("signed on a refusal")
	}
}

func identical(t *testing.T, bodies [][]byte, n int) {
	t.Helper()
	if len(bodies) != n {
		t.Fatalf("fake saw %d bodies, want %d", len(bodies), n)
	}
	for _, b := range bodies[1:] {
		if !bytes.Equal(b, bodies[0]) {
			t.Fatalf("retry body differs:\n%s\n%s", bodies[0], b)
		}
	}
}

// Acceptance 3.
func TestRetries(t *testing.T) {
	t.Run("retry_later twice then 200", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.fake.Script(vaulttest.Step{Status: 503, Code: "retry_later"}, vaulttest.Step{Status: 503, Code: "retry_later"})
		mustSign(t, e, e.request())
		identical(t, e.fake.Bodies(), 3)
		if !strings.Contains(e.logs.String(), `"attempts":3`) {
			t.Fatalf("log: %s", e.logs.String())
		}
	})
	t.Run("connection reset then 200", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.fake.Script(vaulttest.Step{Drop: true})
		mustSign(t, e, e.request())
		identical(t, e.fake.Bodies(), 2)
	})
	t.Run("gateway 502 without a code then 200", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.fake.Script(vaulttest.Step{Status: 502, Body: "<html>bad gateway</html>"})
		mustSign(t, e, e.request())
		identical(t, e.fake.Bodies(), 2)
	})
	t.Run("retry_later until RetryFor", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		for range 500 {
			e.fake.Script(vaulttest.Step{Status: 503, Code: "retry_later"})
		}
		e.cfg.RetryFor = 200 * time.Millisecond
		cli, _ := vaultsign.New(e.cfg)
		cli.SetBackoff(5*time.Millisecond, 20*time.Millisecond)
		e.cli = cli
		start := time.Now()
		_, err := e.sign(e.request())
		if !errors.Is(err, vaultsign.ErrRetryExhausted) {
			t.Fatalf("want ErrRetryExhausted, got %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %v", d)
		}
		bodies := e.fake.Bodies()
		identical(t, bodies, len(bodies))
		if len(bodies) < 3 {
			t.Fatalf("only %d attempts", len(bodies))
		}
	})
	t.Run("context deadline stops retries", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		for range 500 {
			e.fake.Script(vaulttest.Step{Status: 503, Code: "retry_later"})
		}
		e.cfg.RetryFor = time.Minute
		cli, _ := vaultsign.New(e.cfg)
		cli.SetBackoff(5*time.Millisecond, 20*time.Millisecond)
		e.cli = cli
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := e.signCtx(ctx, e.request())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want deadline, got %v", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("took %v past a 150ms deadline", d)
		}
	})
	t.Run("signing_failed is stored and not retried", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.fake.Script(vaulttest.Step{Status: 503, Code: "signing_failed"})
		_, err := e.sign(e.request())
		var ref *vaultsign.Refusal
		if !errors.As(err, &ref) || ref.Code != "signing_failed" || !ref.NeedsNewNonce() {
			t.Fatalf("got %v", err)
		}
		identical(t, e.fake.Bodies(), 1)
	})
}

// Acceptance 4: the first 200 is lost; the retry gets the stored signature.
func TestLostResponse(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	e.fake.Script(vaulttest.Step{DropAfterSign: true})
	mustSign(t, e, e.request())
	identical(t, e.fake.Bodies(), 2)
	if e.fake.Signs() != 1 {
		t.Fatalf("fake signed %d times", e.fake.Signs())
	}
}

// Acceptance 5.
func TestBadSignaturesRefused(t *testing.T) {
	other, _ := crypto.GenerateKey()
	n, _ := new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)
	cases := map[string]func(e *env, r vaultsign.Request) vaulttest.Step{
		"wrong signer": func(*env, vaultsign.Request) vaulttest.Step { return vaulttest.Step{SignWith: other} },
		"high s": func(*env, vaultsign.Request) vaulttest.Step {
			return vaulttest.Step{Mutate: func(s []byte) []byte {
				hs := new(big.Int).Sub(n, new(big.Int).SetBytes(s[32:64]))
				out := append([]byte(nil), s...)
				hs.FillBytes(out[32:64])
				out[64] = 55 - out[64] // 27<->28: (r, n-s, other v) recovers the same signer
				return out
			}}
		},
		"v 0/1": func(*env, vaultsign.Request) vaulttest.Step {
			return vaulttest.Step{Mutate: func(s []byte) []byte { s[64] -= 27; return s }}
		},
		"64 bytes": func(*env, vaultsign.Request) vaulttest.Step {
			return vaulttest.Step{Mutate: func(s []byte) []byte { return s[:64] }}
		},
		"credential_id echo": func(e *env, r vaultsign.Request) vaulttest.Step {
			return vaulttest.Step{Body: fmt.Sprintf(`{"credential_id":%q,"wallet_id":%q,"kind":"evm_eip3009","signature":"0x%s"}`,
				uuid.NewString(), e.fake.WalletID, strings.Repeat("00", 65))}
		},
		"wallet_id echo": func(e *env, r vaultsign.Request) vaulttest.Step {
			return vaulttest.Step{Body: fmt.Sprintf(`{"credential_id":%q,"wallet_id":%q,"kind":"evm_eip3009","signature":"0x%s"}`,
				r.CredentialID, uuid.NewString(), strings.Repeat("00", 65))}
		},
	}
	// Each case must fail for its own reason: a mutation that breaks the
	// signature in some other way (as a wrong v once hid a high-s case) would
	// otherwise pass while testing nothing.
	why := map[string]string{"wrong signer": "recovers to", "high s": "s is high", "v 0/1": "v is ",
		"64 bytes": "not 65 bytes", "credential_id echo": "credential_id echo", "wallet_id echo": "wallet_id echo"}
	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, vaulttest.Options{})
			r := e.request()
			e.fake.Script(step(e, r))
			_, err := e.sign(r)
			if !errors.Is(err, vaultsign.ErrBadSignature) || !strings.Contains(err.Error(), why[name]) {
				t.Fatalf("want ErrBadSignature for %q, got %v", why[name], err)
			}
		})
	}
	// Control: the high-s mutation does recover to the wallet (so the check
	// above is the low-s rule, not a broken signature).
	e := newEnv(t, vaulttest.Options{})
	r := e.request()
	good := mustSign(t, e, r)
	hs := new(big.Int).Sub(n, new(big.Int).SetBytes(good[32:64]))
	m := append([]byte(nil), good[:]...)
	hs.FillBytes(m[32:64])
	m[64] = 55 - m[64] - 27
	pub, err := crypto.SigToPub(x402.Digest(r.Domain, r.Auth).Bytes(), m)
	if err != nil || crypto.PubkeyToAddress(*pub) != e.fake.From {
		t.Fatal("high-s control does not recover to the wallet")
	}
}

// Acceptance 6.
func TestAuth(t *testing.T) {
	t.Run("no client certificate: 403 is a Config refusal", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		cli, err := vaultsign.NewWithoutClientCert(e.cfg)
		if err != nil {
			t.Fatal(err)
		}
		e.cli = cli
		_, err = e.sign(e.request())
		var ref *vaultsign.Refusal
		if !errors.As(err, &ref) || ref.Code != "client_not_authorized" || !ref.Config() {
			t.Fatalf("got %v", err)
		}
		identical(t, e.fake.Bodies(), 1)
	})
	t.Run("another identity's certificate: Config refusal", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.cfg.ClientCert, e.cfg.ClientKey = e.fake.OtherCert, e.fake.OtherKey
		e.cli, _ = vaultsign.NewUnchecked(e.cfg)
		_, err := e.sign(e.request())
		var ref *vaultsign.Refusal
		if !errors.As(err, &ref) || ref.Status != 403 || !ref.Config() {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no client certificate refused at TLS: ErrTLS, not retried", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{RequireCertAtTLS: true})
		e.cli, _ = vaultsign.NewWithoutClientCert(e.cfg)
		_, err := e.sign(e.request())
		if !errors.Is(err, vaultsign.ErrTLS) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("server not signed by the CA: ErrTLS", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		other := vaulttest.New(t, vaulttest.Options{})
		e.cfg.CAFile = other.CAFile
		cli, err := vaultsign.New(e.cfg)
		if err != nil {
			t.Fatal(err)
		}
		e.cli = cli
		if _, err := e.sign(e.request()); !errors.Is(err, vaultsign.ErrTLS) {
			t.Fatalf("got %v", err)
		}
		if len(e.fake.Bodies()) != 0 {
			t.Fatal("request reached an untrusted server")
		}
	})
	t.Run("401 once: token refreshed, call succeeds", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		mustSign(t, e, e.request()) // caches a token
		e.fake.RevokeTokens()
		mustSign(t, e, e.request())
		if e.fake.TokenFetches() != 2 {
			t.Fatalf("token fetches %d", e.fake.TokenFetches())
		}
	})
	t.Run("401 twice: final", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.fake.Script(vaulttest.Step{Status: 401, Code: "client_not_authorized"}, vaulttest.Step{Status: 401, Code: "client_not_authorized"})
		_, err := e.sign(e.request())
		var ref *vaultsign.Refusal
		if !errors.As(err, &ref) || ref.Status != 401 || !ref.Config() || !ref.Known() {
			t.Fatalf("got %v", err)
		}
		if len(e.fake.Bodies()) != 2 || e.fake.TokenFetches() != 2 {
			t.Fatalf("bodies %d fetches %d", len(e.fake.Bodies()), e.fake.TokenFetches())
		}
	})
	t.Run("wrong client secret: ErrToken, nothing sent", func(t *testing.T) {
		e := newEnv(t, vaulttest.Options{})
		e.cfg.Token.(*vaultsign.ClientCredentials).ClientID = "someone-else"
		e.cli, _ = vaultsign.New(e.cfg)
		if _, err := e.sign(e.request()); !errors.Is(err, vaultsign.ErrToken) {
			t.Fatalf("got %v", err)
		}
		if len(e.fake.Bodies()) != 0 {
			t.Fatal("sent without a token")
		}
	})
}

func TestTokenCache(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	ts := e.cfg.Token.(*vaultsign.ClientCredentials)
	now := time.Now()
	ts.SetClock(func() time.Time { return now })
	ctx := context.Background()
	a, err := ts.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ts.Token(ctx)
	if a != b || e.fake.TokenFetches() != 1 {
		t.Fatal("token not cached")
	}
	now = now.Add(300*time.Second - 29*time.Second) // inside the 30s early window
	c, _ := ts.Token(ctx)
	if c == a || e.fake.TokenFetches() != 2 {
		t.Fatal("token not replaced before expiry")
	}
}

// Acceptance 7: token, client secret and sig appear in no log line or error.
func TestNoLeaks(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	r := e.request()
	mustSign(t, e, r)
	e.fake.RevokeTokens()
	mustSign(t, e, e.request())
	for _, code := range []string{"credential_malformed", "signature_invalid", "replayed"} {
		e.fake.Script(vaulttest.Step{Status: map[string]int{"credential_malformed": 400, "signature_invalid": 403, "replayed": 409}[code], Code: code})
		e.sign(e.request())
	}
	// A Vault answer that echoes the sig and a token back in its message and
	// in an unknown code's raw body: both must be redacted.
	r2 := e.request()
	tok := e.fake.Tokens()[0]
	sig := e.sigs[len(e.sigs)-1]
	e.fake.Script(vaulttest.Step{Status: 403, Body: fmt.Sprintf(`{"code":"weird","message":"sig %s token %s"}`, sig, tok)})
	e.sign(r2)
	r3 := e.request()
	e.fake.Script(vaulttest.Step{Status: 400, Code: "credential_malformed", Body: fmt.Sprintf(`{"code":"credential_malformed","field":"sig","message":"bad sig %s"}`, e.sigs[len(e.sigs)-1])})
	e.sign(r3)
	e.fake.Script(vaulttest.Step{Status: 401, Code: "client_not_authorized"}, vaulttest.Step{Status: 401, Code: "client_not_authorized"})
	e.sign(e.request())
	// Formatting the token source prints no secret either.
	e.logs.Write([]byte(fmt.Sprintf("%v %+v %#v %s", e.cfg.Token, e.cfg.Token, e.cfg.Token, e.cfg.Token)))

	hay := e.logs.String() + "\n" + strings.Join(e.errs, "\n")
	if len(e.errs) < 5 {
		t.Fatalf("expected errors to check, got %d", len(e.errs))
	}
	secrets := append([]string{e.fake.Secret()}, e.sigs...)
	secrets = append(secrets, e.fake.Tokens()...)
	secrets = append(secrets, "fake-token-")
	for _, s := range secrets {
		if strings.Contains(hay, s) {
			t.Fatalf("secret %q leaked:\n%s", s[:8], hay)
		}
	}
	if !strings.Contains(hay, "[redacted]") {
		t.Fatal("echoed secrets were not redacted (the test did not exercise redaction)")
	}
}

func TestRequestChecks(t *testing.T) {
	e := newEnv(t, vaulttest.Options{})
	r := e.request()
	r.CredentialID = uuid.NewString()
	if _, err := e.sign(r); err == nil || len(e.fake.Bodies()) != 0 {
		t.Fatal("credential id mismatch was sent")
	}
	r = e.request()
	r.Credential = json.RawMessage(`"a string"`)
	if _, err := e.sign(r); err == nil {
		t.Fatal("string credential accepted")
	}
	for _, bad := range []vaultsign.Config{
		{BaseURL: "http://vault.example.com", WalletID: e.cfg.WalletID},
		{BaseURL: e.cfg.BaseURL, WalletID: "NOT-A-UUID"},
	} {
		bad.Token, bad.ClientCert, bad.ClientKey, bad.CAFile = e.cfg.Token, e.cfg.ClientCert, e.cfg.ClientKey, e.cfg.CAFile
		if _, err := vaultsign.New(bad); err == nil {
			t.Fatalf("accepted %+v", bad.BaseURL)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	names := []string{"AG_VAULT_URL", "AG_VAULT_WALLET_ID", "AG_VAULT_CERT", "AG_VAULT_KEY", "AG_VAULT_CA",
		"AG_VAULT_TOKEN_URL", "AG_VAULT_CLIENT_ID", "AG_VAULT_CLIENT_SECRET_FILE"}
	for _, n := range names {
		t.Setenv(n, "")
	}
	if c, err := vaultsign.ConfigFromEnv(); c != nil || err != nil {
		t.Fatalf("unset: %v %v", c, err)
	}
	t.Setenv("AG_VAULT_URL", "https://vault.example.com")
	t.Setenv("AG_VAULT_CA", "/x/ca.crt")
	_, err := vaultsign.ConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "AG_VAULT_CLIENT_SECRET_FILE") || strings.Contains(err.Error(), "AG_VAULT_URL,") {
		t.Fatalf("partial: %v", err)
	}
	for _, n := range names {
		t.Setenv(n, "v-"+n)
	}
	c, err := vaultsign.ConfigFromEnv()
	if err != nil || c.BaseURL != "v-AG_VAULT_URL" || c.Token.(*vaultsign.ClientCredentials).SecretFile != "v-AG_VAULT_CLIENT_SECRET_FILE" {
		t.Fatalf("full: %+v %v", c, err)
	}
}
