// Command agentgate is the single binary; subcommands are added by later tasks.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ws4charlie/agent-gate/internal/config"
	"github.com/ws4charlie/agent-gate/internal/db"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "agentgate", version)
	case "migrate":
		err = migrate(ctx, args[1:], stdout)
	case "serve":
		err = serve(ctx, args[1:], stdout)
	case "account":
		err = account(ctx, args[1:], stdout)
	case "key":
		err = keyCmd(ctx, args[1:], stdout)
	case "report":
		err = report(ctx, args[1:], stdout)
	case "reconcile":
		err = reconcileCmd(ctx, args[1:], stdout)
	case "prices":
		err = prices(ctx, args[1:], stdout)
	case "wallet":
		err = walletCmd(args[1:], stdout)
	case "paidtool":
		err = paidtoolCmd(ctx, args[1:], stdout)
	case "help", "-h", "--help":
		usage(stdout)
	default:
		fmt.Fprintf(stderr, "agentgate: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "agentgate:", err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage: agentgate <command>

commands:
  version     print the version
  migrate     apply database migrations
  serve       run the gateway (and the background reconciler)
  account     create | deposit | balance
  key         create --account ID: print a client key once
  report      --since 24h --by model|account
  reconcile   close expired holds once
  prices      sync: refresh configs/prices.json from OpenRouter
  wallet      new | address: the agent wallet key (testnet only)
  paidtool    run the paid x402 endpoint (--network sim | base-sepolia)`)
}

func migrate(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to JSON config (default $AG_CONFIG)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.DatabaseURL == "" {
		return fmt.Errorf("no database url: set DATABASE_URL or database_url in config")
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "applied %d migration(s)\n", len(applied))
	return nil
}
