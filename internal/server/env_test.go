package server_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/pricing"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/server"
)

const (
	upstreamKey = "UPSTREAM-KEY-0123456789abcdef"
	usd         = money.Nanos(1_000_000_000)
)

// Test prices, Nanos per 1M tokens: in $1, out $2, cache read $0.10, cache write $1.25.
var testPrice = pricing.Price{Input: 1_000_000_000, Output: 2_000_000_000, CacheRead: 100_000_000, CacheWrite: 1_250_000_000}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	led    *ledger.Postgres
	audit  *audit.Store
	fake   *fakeupstream.Server
	srv    *server.Server
	gw     *httptest.Server
	acct   string
	secret string
	logs   *syncBuf
	spans  *tracetest.InMemoryExporter
}

func newEnv(t *testing.T, deposit money.Nanos, tweak func(*server.Server)) *env {
	t.Helper()
	pool := dbtest.New(t)
	e := &env{t: t, pool: pool, led: ledger.New(pool), audit: audit.NewStore(pool), fake: fakeupstream.New(),
		logs: &syncBuf{}, spans: tracetest.NewInMemoryExporter()}
	t.Cleanup(e.fake.Close)
	e.fake.Key = upstreamKey

	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(e.spans))
	e.srv = &server.Server{
		Pool: pool, Ledger: e.led, Audit: e.audit,
		Providers: provider.Config{
			Providers: map[string]provider.ProviderCfg{
				"fakeoa": {Kind: provider.KindOpenAICompatible, BaseURL: e.fake.URL + "/v1"},
				"fakean": {Kind: provider.KindAnthropic, BaseURL: e.fake.URL + "/v1"},
			},
			Routes: []provider.Route{{Prefix: "oa/", Provider: "fakeoa", Strip: true}, {Prefix: "an/", Provider: "fakean", Strip: true}},
		},
		Prices:  &pricing.Table{Models: map[string]pricing.Price{"m": testPrice}},
		Keys:    func(string) string { return upstreamKey },
		Tracer:  tp.Tracer("test"),
		Log:     slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		MaxHold: 5 * usd, DefaultMaxTokens: 100,
	}
	if tweak != nil {
		tweak(e.srv)
	}
	e.gw = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.gw.Close)

	ctx := context.Background()
	var err error
	if e.acct, err = e.led.CreateAccount(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if deposit > 0 {
		if err := e.led.Deposit(ctx, e.acct, deposit, "dep", ""); err != nil {
			t.Fatal(err)
		}
	}
	if e.secret, _, err = server.CreateKey(ctx, pool, e.acct, "t"); err != nil {
		t.Fatal(err)
	}
	return e
}

// do posts to the gateway. path is /v1/chat/completions or /v1/messages.
func (e *env) do(path, body string, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	resp, err := e.doCtx(context.Background(), path, body, hdr)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func (e *env) doCtx(ctx context.Context, path, body string, hdr map[string]string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.gw.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if _, ok := hdr["Authorization"]; !ok {
		req.Header.Set("Authorization", "Bearer "+e.secret)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	return http.DefaultClient.Do(req)
}

func chatBody(model string, stream bool) string {
	s := "false"
	if stream {
		s = "true"
	}
	return `{"model":"` + model + `","stream":` + s + `,"max_tokens":200,"messages":[{"role":"user","content":"hi"}]}`
}

func msgBody(model string, stream bool) string {
	s := "false"
	if stream {
		s = "true"
	}
	return `{"model":"` + model + `","stream":` + s + `,"max_tokens":200,"messages":[{"role":"user","content":"hi"}]}`
}

// auditRows waits briefly for the audit row of a request (written just after the response ends).
func (e *env) auditRow(resp *http.Response) audit.Row {
	e.t.Helper()
	id := resp.Header.Get("X-Request-Id")
	var rows []audit.Row
	for i := 0; i < 200; i++ {
		var err error
		rows, err = e.audit.ByRequest(context.Background(), id)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(rows) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 {
		e.t.Fatalf("want 1 audit row for %s, got %d", id, len(rows))
	}
	return rows[0]
}

func (e *env) noActiveHolds() {
	e.t.Helper()
	var n int
	for i := 0; i < 200; i++ {
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM holds WHERE status='active'`).Scan(&n); err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n != 0 {
		e.t.Fatalf("%d active holds left", n)
	}
	if err := e.led.Audit(context.Background()); err != nil {
		e.t.Fatalf("ledger audit: %v", err)
	}
}

func (e *env) balance() ledger.Balance {
	b, err := e.led.Balance(context.Background(), e.acct)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}
