package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/bank"
	"github.com/ws4charlie/agent-gate/internal/ledger"
)

// mercuryFlags are the gateway flags and env vars of the bank rail. The token
// is read from the environment only (AG_MERCURY_TOKEN or MERCURY_API_TOKEN):
// a flag would show up in the process list.
type mercuryFlags struct {
	on                      *bool
	baseURL, account, proxy *string
	poll                    *time.Duration
}

func addMercuryFlags(c *common) mercuryFlags {
	return mercuryFlags{
		on:      c.fs.Bool("mercury", envBool("AG_MERCURY"), "turn on the Mercury ACH bank rail: POST /v1/bank/payments and the poller (default $AG_MERCURY, off; sandbox only)"),
		baseURL: c.fs.String("mercury-base-url", envOr("AG_MERCURY_BASE_URL", bank.SandboxBaseURL), "Mercury API base URL (default $AG_MERCURY_BASE_URL, the sandbox)"),
		account: c.fs.String("mercury-account", os.Getenv("AG_MERCURY_ACCOUNT"), "Mercury checking account id payments are paid from (default $AG_MERCURY_ACCOUNT)"),
		proxy:   c.fs.String("mercury-proxy", os.Getenv("AG_MERCURY_PROXY"), "egress proxy URL for Mercury calls (default $AG_MERCURY_PROXY; empty: direct)"),
		poll:    c.fs.Duration("mercury-poll", envDuration("AG_MERCURY_POLL", 30*time.Second), "how often to ask Mercury about payments that are not final (default $AG_MERCURY_POLL, 30s)"),
	}
}

func envDuration(k string, d time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return d
}

func mercuryToken() string {
	if v := os.Getenv("AG_MERCURY_TOKEN"); v != "" {
		return v
	}
	return os.Getenv("MERCURY_API_TOKEN")
}

// newBankService wires the bank rail for the gateway.
func newBankService(f mercuryFlags, pool *pgxpool.Pool, led ledger.Ledger, ag *agents.Store, q *approval.Queue, log *slog.Logger) (*bank.Service, error) {
	rail, err := bank.NewMercury(bank.MercuryConfig{BaseURL: *f.baseURL, AccountID: *f.account, Token: mercuryToken(), Proxy: *f.proxy})
	if err != nil {
		return nil, fmt.Errorf("--mercury: %w (set AG_MERCURY_TOKEN or MERCURY_API_TOKEN, and --mercury-account)", err)
	}
	return &bank.Service{Store: bank.NewStore(pool), Rail: rail, Ledger: led, Charges: ag, Approvals: q.Store, Log: log}, nil
}

// bankCmd: bank list | show <payment_id> | simulate-return <payment_id> [--code R01].
func bankCmd(ctx context.Context, args []string, stdout io.Writer) error {
	const subs = "list | show <payment_id> | simulate-return <payment_id> [--code R01]"
	if len(args) == 0 {
		return errors.New("bank: need " + subs)
	}
	sub, rest := args[0], args[1:]
	var pos []string
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	want := map[string]int{"list": 0, "show": 1, "simulate-return": 1}
	n, ok := want[sub]
	if !ok {
		return fmt.Errorf("bank: unknown subcommand %q (want %s)", sub, subs)
	}
	if len(pos) != n {
		return fmt.Errorf("bank %s: need %d id argument(s)", sub, n)
	}
	c := newCommon("bank " + sub)
	code := c.fs.String("code", "R01", "ACH return code to record (simulate-return)")
	base := c.fs.String("mercury-base-url", envOr("AG_MERCURY_BASE_URL", bank.SandboxBaseURL), "Mercury API base URL; simulate-return is refused unless it is the sandbox")
	limit := c.fs.Int("limit", 50, "how many payments to list (list)")
	if err := c.fs.Parse(rest); err != nil {
		return err
	}
	if sub == "simulate-return" && !bank.IsSandbox(*base) {
		return errors.New("bank simulate-return: refused, the Mercury base URL is not the sandbox; a real return comes from the bank")
	}
	_, pool, err := c.open(ctx, rest)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := bank.NewStore(pool)
	switch sub {
	case "list":
		ps, err := st.List(ctx, *limit)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PAYMENT\tSTATE\tAMOUNT\tRECIPIENT\tMERCURY REQUEST\tRULE\tCREATED")
		for _, p := range ps {
			fmt.Fprintf(tw, "%s\t%s\t$%s\t%s\t%s\t%s\t%s\n", p.ID, p.State, p.Amount, p.RecipientID, p.RequestID, p.Rule, p.CreatedAt.UTC().Format(time.RFC3339))
		}
		return tw.Flush()
	case "show":
		p, err := st.Get(ctx, pos[0])
		if err != nil {
			return fmt.Errorf("bank show: no payment %q", pos[0])
		}
		evs, err := st.Events(ctx, p.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "payment   %s\nstate     %s\nagent     %s\namount    $%s\nrecipient %s\nmercury   request %s txn %s\nhold      %s\n\n",
			p.ID, p.State, p.AgentID, p.Amount, p.RecipientID, dash(p.RequestID), dash(p.TxnID), dash(p.HoldID))
		for _, e := range evs {
			from := string(e.From)
			if from == "" {
				from = "-"
			}
			fmt.Fprintf(stdout, "%2d  %s  %s -> %s  by %s  %s\n", e.Seq, e.At.UTC().Format("15:04:05"), from, e.To, e.Actor, e.Detail)
		}
		return nil
	default: // simulate-return
		led := ledger.New(pool)
		svc := &bank.Service{Store: st, Ledger: led, Charges: agents.NewStore(pool)}
		p, err := svc.Return(ctx, pos[0], *code, "simulated-return")
		if err != nil {
			return fmt.Errorf("bank simulate-return: %w", err)
		}
		fmt.Fprintf(stdout, "payment %s is now %s (simulated ACH return %s; the sandbox cannot produce a real one)\n", p.ID, p.State, *code)
		return nil
	}
}
