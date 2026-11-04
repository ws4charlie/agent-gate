package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/config"
	"github.com/ws4charlie/agent-gate/internal/db"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/mcp"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/pricing"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/screen"
	"github.com/ws4charlie/agent-gate/internal/server"
	"github.com/ws4charlie/agent-gate/internal/telemetry"
)

// common holds the flags every database-backed command shares.
type common struct {
	fs        *flag.FlagSet
	cfgPath   *string
	providers *string
	prices    *string
}

func newCommon(name string) *common {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return &common{fs: fs,
		cfgPath:   fs.String("config", "", "path to JSON config (default $AG_CONFIG)"),
		providers: fs.String("providers", envOr("AG_PROVIDERS", "configs/providers.json"), "providers and routes file"),
		prices:    fs.String("prices", envOr("AG_PRICES", "configs/prices.json"), "price table file"),
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (c *common) open(ctx context.Context, args []string) (config.Config, *pgxpool.Pool, error) {
	if err := c.fs.Parse(args); err != nil {
		return config.Config{}, nil, err
	}
	cfg, err := config.Load(*c.cfgPath)
	if err != nil {
		return cfg, nil, err
	}
	if cfg.DatabaseURL == "" {
		return cfg, nil, errors.New("no database url: set DATABASE_URL or database_url in config")
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	return cfg, pool, err
}

func serve(ctx context.Context, args []string, stdout io.Writer) error {
	c := newCommon("serve")
	storeBodies := c.fs.Bool("store-bodies", false, "store redacted request/response bodies in the audit log (default off)")
	paymentsPath := c.fs.String("payments", os.Getenv("AG_PAYMENTS"), "payments config JSON (default $AG_PAYMENTS; empty: POST /v1/payments is off)")
	mcpPath := c.fs.String("mcp", os.Getenv("AG_MCP"), "MCP upstream servers JSON (default $AG_MCP; empty: /mcp/{server} is off)")
	noScreen := c.fs.Bool("no-screen", false, "do not screen tool output (default: screen it with the built-in rules)")
	judgeModel := c.fs.String("screen-judge-model", "", "also ask this model (a routable name) about each tool result; off by default, costs provider calls")
	reconcileEvery := c.fs.Duration("reconcile-every", 30*time.Second, "background reconcile interval")
	cfg, pool, err := c.open(ctx, args)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	pc, err := provider.LoadConfig(*c.providers)
	if err != nil {
		return err
	}
	prices, err := pricing.Load(*c.prices)
	if err != nil {
		return err
	}
	tracer, shutdown, err := telemetry.Init(ctx, "agentgate")
	if err != nil {
		return err
	}
	defer shutdown(context.Background())

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	led := ledger.New(pool)
	srv := &server.Server{Pool: pool, Ledger: led, Audit: audit.NewStore(pool), Providers: pc, Prices: prices,
		Keys: config.ProviderKey, Tracer: tracer, Log: log, MaxHold: cfg.MaxHoldNanos,
		DefaultMaxTokens: cfg.DefaultMaxTokens, StoreBodies: *storeBodies,
		Agents: agents.NewStore(pool), Approvals: &approval.Queue{Store: approval.NewStore(pool), Ledger: led}}

	if *mcpPath != "" {
		mc, err := mcp.LoadConfig(*mcpPath)
		if err != nil {
			return err
		}
		srv.MCP = &mc
		if !*noScreen {
			srv.Screen = &screen.Screener{}
			if *judgeModel != "" {
				srv.Screen.Judge = &server.ProviderJudge{S: srv, Model: *judgeModel}
			}
		}
		log.Info("mcp gateway enabled", "servers", len(mc.Servers), "screening", srv.Screen != nil, "judge", *judgeModel != "")
	}

	// Always routed: a payment hold must never fall through to the model lookup,
	// whose "not found" would release money that may have moved.
	lookup := &server.RouteLookup{Default: &provider.GenerationLookup{Client: http.DefaultClient, Providers: pc.Providers, Key: config.ProviderKey}}
	if *paymentsPath != "" {
		pf, err := readPaymentsFile(*paymentsPath)
		if err != nil {
			return err
		}
		pay, err := loadPayments(pf)
		if err != nil {
			return err
		}
		pl, err := paymentsLookup(ctx, pf)
		if err != nil {
			return err
		}
		lookup.Payments = pl
		srv.Payments = pay
		log.Info("payments enabled", "wallet", pay.Wallet.Address().Hex(), "network", pay.Wallet.Network())
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.WatchKills(ctx) // I12: cancel in-flight calls of killed or revoked agents
	go func() {            // I6: close holds whose process died
		t := time.NewTicker(*reconcileEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := srv.Approvals.Sweep(ctx); err != nil {
					log.Error("approval sweep", "err", err)
				} else if n > 0 {
					log.Info("approvals expired", "count", n)
				}
				if rep, err := server.Reconcile(ctx, pool, led, lookup, srv.Audit, srv.Agents, time.Now()); err != nil {
					log.Error("reconcile", "err", err)
				} else if rep.Settled+rep.Released+rep.Errors > 0 {
					log.Info("reconcile", "settled", rep.Settled, "released", rep.Released, "errors", rep.Errors)
				}
			}
		}
	}()

	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(sc)
	}()
	fmt.Fprintf(stdout, "agentgate listening on %s\n", cfg.Listen)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func account(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("account: need create | deposit | balance")
	}
	c := newCommon("account " + args[0])
	name := c.fs.String("name", "", "account name (create)")
	id := c.fs.String("account", "", "account id (deposit, balance)")
	amount := c.fs.String("amount", "", "USD amount, e.g. 5 or 0.25 (deposit)")
	idem := c.fs.String("idem", "", "idempotency key (deposit; default random)")
	_, pool, err := c.open(ctx, args[1:])
	if err != nil {
		return err
	}
	defer pool.Close()
	led := ledger.New(pool)
	switch args[0] {
	case "create":
		if *name == "" {
			return errors.New("account create: --name required")
		}
		aid, err := led.CreateAccount(ctx, *name)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, aid)
	case "deposit":
		n, err := money.FromUSD(*amount)
		if err != nil {
			return err
		}
		if *idem == "" {
			*idem = "cli-" + uuid.NewString()
		}
		if err := led.Deposit(ctx, *id, n, *idem, "cli deposit"); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "deposited %s USD\n", n)
	case "balance":
		b, err := led.Balance(ctx, *id)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "balance %s USD, held %s USD\n", b.Balance, b.Held)
	default:
		return fmt.Errorf("account: unknown subcommand %q", args[0])
	}
	return nil
}

func keyCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("key: need create --account ID")
	}
	c := newCommon("key create")
	acct := c.fs.String("account", "", "account id")
	label := c.fs.String("label", "", "label for your own bookkeeping")
	_, pool, err := c.open(ctx, args[1:])
	if err != nil {
		return err
	}
	defer pool.Close()
	if *acct == "" {
		return errors.New("key create: --account required")
	}
	secret, id, err := server.CreateKey(ctx, pool, *acct, *label)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, secret) // the only time the secret exists in plaintext
	fmt.Fprintf(os.Stderr, "key id %s: store the secret now, it cannot be shown again\n", id)
	return nil
}

func report(ctx context.Context, args []string, stdout io.Writer) error {
	c := newCommon("report")
	since := c.fs.String("since", "24h", "duration (24h, 7d) or RFC 3339 time")
	by := c.fs.String("by", "model", "model or account")
	_, pool, err := c.open(ctx, args)
	if err != nil {
		return err
	}
	defer pool.Close()
	from, err := parseSince(*since, time.Now())
	if err != nil {
		return err
	}
	var priceOf func(string) (pricing.Price, bool)
	if pc, err := provider.LoadConfig(*c.providers); err == nil {
		if pt, err := pricing.Load(*c.prices); err == nil {
			priceOf = func(model string) (pricing.Price, bool) {
				t, ok := pc.Resolve(model)
				if !ok {
					return pricing.Price{}, false
				}
				p, err := pt.Lookup(t.Model)
				return p, err == nil
			}
		}
	}
	lines, err := audit.Report(ctx, pool, from, *by, priceOf)
	if err != nil {
		return err
	}
	audit.Print(stdout, *by, lines)
	return nil
}

func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil {
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since: want a duration like 24h or 7d, or an RFC 3339 time: %q", s)
	}
	return now.Add(-d), nil
}

func reconcileCmd(ctx context.Context, args []string, stdout io.Writer) error {
	c := newCommon("reconcile")
	paymentsPath := c.fs.String("payments", os.Getenv("AG_PAYMENTS"), "payments config JSON: needed to close payment holds from the chain")
	_, pool, err := c.open(ctx, args)
	if err != nil {
		return err
	}
	defer pool.Close()
	pc, err := provider.LoadConfig(*c.providers)
	if err != nil {
		return err
	}
	var lookup ledger.UsageLookup = &provider.GenerationLookup{Client: http.DefaultClient, Providers: pc.Providers, Key: config.ProviderKey}
	rl := &server.RouteLookup{Default: lookup} // without chain access payment holds stay active (counted as errors)
	if *paymentsPath != "" {
		pf, err := readPaymentsFile(*paymentsPath)
		if err != nil {
			return err
		}
		if rl.Payments, err = paymentsLookup(ctx, pf); err != nil {
			return err
		}
	}
	rep, err := server.Reconcile(ctx, pool, ledger.New(pool), rl, audit.NewStore(pool), agents.NewStore(pool), time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "reconciled: settled=%d released=%d errors=%d\n", rep.Settled, rep.Released, rep.Errors)
	return nil
}

func prices(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "sync" {
		return errors.New("prices: need sync")
	}
	fs := flag.NewFlagSet("prices sync", flag.ContinueOnError)
	url := fs.String("url", "https://openrouter.ai/api/v1/models", "models endpoint (free, needs no key)")
	out := fs.String("out", "configs/prices.json", "output file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	t, err := pricing.Sync(cctx, http.DefaultClient, *url)
	if err != nil {
		return err
	}
	if err := t.Save(*out); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %d model prices to %s\n", len(t.Models), *out)
	return nil
}
