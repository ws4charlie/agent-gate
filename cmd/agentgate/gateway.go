package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/cedarpolicy"
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
	"github.com/ws4charlie/agent-gate/internal/wallet"
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

// envBool reads a boolean env var; unset or unparsable is false.
func envBool(k string) bool { v, _ := strconv.ParseBool(os.Getenv(k)); return v }

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
	spiffeListen := c.fs.String("spiffe-listen", "", "address of the mutual-TLS listener for SPIFFE identities, e.g. :18443 (empty: off)")
	spiffeSocket := c.fs.String("spiffe-socket", envOr("AG_SPIFFE_SOCKET", "unix:///run/spire/agent/sockets/agent.sock"), "SPIFFE Workload API socket (default $AG_SPIFFE_SOCKET)")
	spiffeDomain := c.fs.String("spiffe-trust-domain", os.Getenv("AG_SPIFFE_TRUST_DOMAIN"), "trust domains whose SVIDs are accepted, comma-separated; the first is ours, the others are federated (required with --spiffe-listen)")
	cedarDir := c.fs.String("cedar-policies", os.Getenv("AG_CEDAR_POLICIES"), "directory of Cedar policies to run beside the Go engine: shadow mode (recorded, Go decides) unless --cedar-enforce (default $AG_CEDAR_POLICIES; empty: off)")
	registryEnforce := c.fs.Bool("registry-enforce", envBool("AG_REGISTRY_ENFORCE"), "deny models and tools that are not approved in the AI Registry (rule registry_unapproved; default $AG_REGISTRY_ENFORCE, off)")
	cedarEnforce := c.fs.Bool("cedar-enforce", envBool("AG_CEDAR_ENFORCE"), "let Cedar decide, record the Go engine beside it, and deny when Cedar fails; needs --cedar-policies (default $AG_CEDAR_ENFORCE)")
	spiffeID := c.fs.String("spiffe-id", os.Getenv("AG_SPIFFE_ID"), "the SPIFFE ID of the gateway's own SVID, when the Workload API returns several (default $AG_SPIFFE_ID; empty: the first one)")
	reconcileEvery := c.fs.Duration("reconcile-every", 30*time.Second, "background reconcile interval")
	cfg, pool, err := c.open(ctx, args)
	if err != nil {
		return err
	}
	defer pool.Close()
	if *cedarEnforce && *cedarDir == "" {
		return errors.New("--cedar-enforce needs --cedar-policies (or AG_CEDAR_POLICIES)")
	}
	var spiffeTDs []spiffeid.TrustDomain
	var spiffeSelf spiffeid.ID
	if *spiffeListen != "" {
		if spiffeTDs, err = parseTrustDomains(*spiffeDomain); err != nil {
			return fmt.Errorf("--spiffe-trust-domain is required with --spiffe-listen: %w", err)
		}
		if *spiffeID != "" {
			if spiffeSelf, err = spiffeid.FromString(*spiffeID); err != nil {
				return fmt.Errorf("--spiffe-id: %w", err)
			}
		}
	}
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

	if *registryEnforce {
		srv.RegistryEnforce = true
		log.Info("registry check on")
	}
	if *cedarDir != "" {
		eng, err := cedarpolicy.Load(*cedarDir)
		if err != nil {
			return fmt.Errorf("--cedar-policies %s: %w", *cedarDir, err)
		}
		srv.Cedar, srv.CedarEnforce = eng, *cedarEnforce
		if *cedarEnforce {
			log.Info("cedar enforce mode on", "dir", *cedarDir)
		} else {
			log.Info("cedar shadow mode on", "dir", *cedarDir)
		}
	}

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
	spiffeDone := make(chan struct{})
	close(spiffeDone)
	if *spiffeListen != "" {
		l := &spiffeListener{open: workloadSource(*spiffeSocket, spiffeSelf), addr: *spiffeListen, tds: spiffeTDs,
			handler: srv.Handler(), log: log, stdout: stdout, minWait: time.Second, maxWait: 30 * time.Second}
		spiffeDone = make(chan struct{})
		go func() { defer close(spiffeDone); l.run(ctx) }()
	}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sc)
	}()
	fmt.Fprintf(stdout, "agentgate listening on %s\n", cfg.Listen)
	err = hs.ListenAndServe()
	stop() // a plain listener that failed to start must not leave the other one running
	<-spiffeDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// spiffeSource is what the mutual-TLS listener needs from the Workload API:
// the gateway's SVID and the trust bundles. workloadapi.X509Source is one.
type spiffeSource interface {
	x509svid.Source
	x509bundle.Source
	Close() error
}

// pickSVID returns the picker that chooses the SVID whose ID is id. The
// gateway and the runner share a unix user on the VM, so the Workload API
// returns both identities and each process must say which one is its own. A
// nil result means "none matches"; an empty id keeps the default (first SVID).
func pickSVID(id spiffeid.ID) func([]*x509svid.SVID) *x509svid.SVID {
	if id.IsZero() {
		return nil
	}
	return func(svids []*x509svid.SVID) *x509svid.SVID {
		for _, s := range svids {
			if s.ID == id {
				return s
			}
		}
		return nil
	}
}

// workloadSource opens the Workload API source for the gateway's own SVID. It
// fails when the API does not answer in time or has no SVID with that ID.
func workloadSource(socket string, id spiffeid.ID) func(context.Context) (spiffeSource, error) {
	return func(ctx context.Context) (spiffeSource, error) {
		opts := []workloadapi.X509SourceOption{workloadapi.WithClientOptions(workloadapi.WithAddr(socket))}
		if pk := pickSVID(id); pk != nil {
			opts = append(opts, workloadapi.WithDefaultX509SVIDPicker(pk))
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		src, err := workloadapi.NewX509Source(sctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("Workload API at %s not reachable: %w", socket, err)
		}
		if _, err := src.GetX509SVID(); err != nil {
			src.Close()
			return nil, fmt.Errorf("Workload API at %s has no SVID %s", socket, id)
		}
		return src, nil
	}
}

// spiffeListener keeps the mutual-TLS listener alive next to the plain one
// without ever being a reason for the gateway to stop: while SPIRE is not
// there, it logs, waits and tries again (at most maxWait apart), and opens
// the listener once an SVID is available. Everything on the plain listener
// goes on as usual meanwhile.
type spiffeListener struct {
	open    func(context.Context) (spiffeSource, error)
	addr    string
	tds     []spiffeid.TrustDomain // the first is ours, the rest are federated
	handler http.Handler
	log     *slog.Logger
	stdout  io.Writer
	minWait time.Duration
	maxWait time.Duration
}

// run blocks until ctx ends, then closes the listener and the source.
func (l *spiffeListener) run(ctx context.Context) {
	wait := l.minWait
	for ctx.Err() == nil {
		err := l.serveOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		l.log.Error("spiffe mutual-TLS listener not available; the plain listener keeps serving", "err", err, "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, l.maxWait)
	}
}

// serveOnce opens the source and the listener and serves until ctx ends (nil)
// or the listener fails (the error).
func (l *spiffeListener) serveOnce(ctx context.Context) error {
	src, err := l.open(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	ln, err := tls.Listen("tcp", l.addr, server.ServerTLSConfig(src, src, l.tds...))
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: server.SPIFFEHandler(l.handler), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(l.stdout, "agentgate listening on %s with SPIFFE mutual TLS (trust domains %s)\n", l.addr, joinDomains(l.tds))
	var have, missing []string
	for _, td := range l.tds {
		if b, err := src.GetX509BundleForTrustDomain(td); err == nil && b != nil {
			have = append(have, td.Name())
		} else {
			missing = append(missing, td.Name())
		}
	}
	l.log.Info("spiffe trust domains", "own", l.tds[0].Name(), "accepted", joinDomains(l.tds), "with_bundle", strings.Join(have, ","))
	if len(missing) > 0 {
		l.log.Warn("spiffe trust domains listed but without a bundle from the Workload API; their clients are refused until it arrives (is the entry registered with -federatesWith?)", "domains", strings.Join(missing, ","))
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sc)
		return nil
	case err := <-errc:
		return err
	}
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
	by := c.fs.String("by", "model", "model, account or agent (agent: model spend and tool payments side by side, reconciled with the chain)")
	paymentsPath := c.fs.String("payments", os.Getenv("AG_PAYMENTS"), "payments config JSON: with --by agent, also compares payment holds with the chain")
	_, pool, err := c.open(ctx, args)
	if err != nil {
		return err
	}
	defer pool.Close()
	from, err := parseSince(*since, time.Now())
	if err != nil {
		return err
	}
	if *by == "agent" {
		lines, err := audit.ReportByAgent(ctx, pool, from)
		if err != nil {
			return err
		}
		audit.PrintAgents(stdout, lines)
		if *paymentsPath == "" {
			fmt.Fprintln(stdout, "\nchain reconciliation: skipped (no --payments config)")
			return nil
		}
		pf, err := readPaymentsFile(*paymentsPath)
		if err != nil {
			return err
		}
		reader, err := chainReader(ctx, pf)
		if err != nil {
			return err
		}
		rep, err := wallet.ReconcileChain(ctx, pool, reader, from)
		if err != nil {
			return err
		}
		rep.Print(stdout)
		if len(rep.Findings) > 0 {
			return fmt.Errorf("report: %d ledger/chain mismatches", len(rep.Findings))
		}
		return nil
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

// parseTrustDomains reads the comma-separated --spiffe-trust-domain value:
// at least one domain, the first is ours, duplicates are dropped.
func parseTrustDomains(v string) ([]spiffeid.TrustDomain, error) {
	var out []spiffeid.TrustDomain
	seen := map[spiffeid.TrustDomain]bool{}
	for _, f := range strings.Split(v, ",") {
		td, err := spiffeid.TrustDomainFromString(strings.TrimSpace(f))
		if err != nil {
			return nil, err
		}
		if !seen[td] {
			seen[td] = true
			out = append(out, td)
		}
	}
	return out, nil
}

func joinDomains(tds []spiffeid.TrustDomain) string {
	names := make([]string, len(tds))
	for i, td := range tds {
		names[i] = td.Name()
	}
	return strings.Join(names, ",")
}
