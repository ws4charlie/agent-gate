// Command bench measures the latency the gateway adds, offline.
//
// It runs a fake upstream with a fixed delay and an in-process gateway on a
// throwaway Postgres database, then sends the same streaming request directly
// to the fake and through the gateway, at concurrency 1 and 50, and prints
// p50/p99 time-to-first-byte and total time. Everything is loopback: the numbers
// are overhead against a fake, not real-provider latency.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/db"
	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/pricing"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/server"
)

func main() {
	n1 := flag.Int("n1", 300, "requests at concurrency 1")
	n50 := flag.Int("n50", 3000, "requests at concurrency 50")
	latency := flag.Duration("upstream-latency", 20*time.Millisecond, "fake upstream delay before the first byte")
	chunk := flag.Duration("chunk-delay", 2*time.Millisecond, "fake upstream delay between events")
	flag.Parse()
	if err := run(*n1, *n50, *latency, *chunk); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run(n1, n50 int, latency, chunk time.Duration) error {
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("set DATABASE_URL")
	}
	pool, cleanup, err := tempDB(ctx, url)
	if err != nil {
		return err
	}
	defer cleanup()

	fake := fakeupstream.New()
	defer fake.Close()
	fake.SetDefault(fakeupstream.Script{Latency: latency, ChunkDelay: chunk})

	led := ledger.New(pool)
	srv := &server.Server{Pool: pool, Ledger: led, Audit: audit.NewStore(pool),
		Providers: provider.Config{
			Providers: map[string]provider.ProviderCfg{"fake": {Kind: provider.KindOpenAICompatible, BaseURL: fake.URL + "/v1"}},
			Routes:    []provider.Route{{Prefix: "", Provider: "fake"}},
		},
		Prices: &pricing.Table{Models: map[string]pricing.Price{"m": {Input: 1_000_000_000, Output: 2_000_000_000}}},
		Keys:   func(string) string { return "bench-upstream-key" }, MaxHold: 5_000_000_000, DefaultMaxTokens: 100}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()

	acct, _ := led.CreateAccount(ctx, "bench")
	if err := led.Deposit(ctx, acct, 1_000_000_000_000, "dep", ""); err != nil {
		return err
	}
	secret, _, err := server.CreateKey(ctx, pool, acct, "bench")
	if err != nil {
		return err
	}

	body := `{"model":"m","stream":true,"max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 100}}
	fmt.Printf("upstream delay %v + %v/event, loopback, offline against the fake\n\n", latency, chunk)
	fmt.Println("| concurrency | path | requests | TTFB p50 | TTFB p99 | total p50 | total p99 |")
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, c := range []struct{ conc, n int }{{1, n1}, {50, n50}} {
		for _, t := range []struct{ name, url, key string }{{"direct to fake", fake.URL + "/v1/chat/completions", "bench-upstream-key"}, {"via gateway", gw.URL + "/v1/chat/completions", secret}} {
			ttfb, total, fails := load(client, t.url, t.key, body, c.conc, c.n)
			fmt.Printf("| %d | %s | %d | %s | %s | %s | %s |\n", c.conc, t.name, c.n-fails,
				pct(ttfb, 50), pct(ttfb, 99), pct(total, 50), pct(total, 99))
			if fails > 0 {
				fmt.Printf("|  |  failures: %d |  |  |  |  |  |\n", fails)
			}
		}
	}
	return led.Audit(ctx)
}

func load(client *http.Client, url, key, body string, conc, n int) (ttfb, total []time.Duration, fails int) {
	var mu sync.Mutex
	jobs := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				start := time.Now()
				req, _ := http.NewRequest("POST", url, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+key)
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					mu.Lock()
					fails++
					mu.Unlock()
					continue
				}
				one := make([]byte, 1)
				_, err = io.ReadFull(resp.Body, one)
				first := time.Since(start)
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				done := time.Since(start)
				mu.Lock()
				if err != nil || resp.StatusCode != 200 {
					fails++
				} else {
					ttfb, total = append(ttfb, first), append(total, done)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return
}

func pct(d []time.Duration, p int) string {
	if len(d) == 0 {
		return "n/a"
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(len(s)-1, len(s)*p/100)].Round(10 * time.Microsecond).String()
}

func tempDB(ctx context.Context, url string) (*pgxpool.Pool, func(), error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, nil, err
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	name := "agentgate_bench_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		return nil, nil, err
	}
	pcfg, _ := pgxpool.ParseConfig(url)
	pcfg.ConnConfig.Database = name
	pcfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, nil, err
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		return nil, nil, err
	}
	return pool, func() {
		pool.Close()
		admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
		admin.Close(ctx)
	}, nil
}
