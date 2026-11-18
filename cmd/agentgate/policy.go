package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

func csv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// usd parses an optional USD flag; empty means "no limit" (0).
func usd(flag, v string) (money.Nanos, error) {
	if v == "" {
		return 0, nil
	}
	n, err := money.FromUSD(v)
	if err != nil {
		return 0, fmt.Errorf("--%s: %w", flag, err)
	}
	return n, nil
}

// agentCmd: agent create | list | revoke | set-spiffe | api-key | rotate-key | key-expiry. Needs direct database access, like
// every administrative command: there is no HTTP route that changes an agent.
func agentCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("agent: need create | list | revoke | set-spiffe | api-key | rotate-key | key-expiry")
	}
	c := newCommon("agent " + args[0])
	acct := c.fs.String("account", "", "ledger account the agent spends from (create)")
	name := c.fs.String("name", "", "unique agent name (create)")
	models := c.fs.String("models", "", "allowed models, comma separated; a trailing * is a prefix, * is all (create)")
	tools := c.fs.String("tools", "", "allowed tools, comma separated (create)")
	payees := c.fs.String("payees", "", "allowed payee 0x addresses, comma separated (create)")
	daily := c.fs.String("daily", "", "daily budget in USD, per UTC day; empty: no limit (create)")
	task := c.fs.String("task", "", "per-task budget in USD (X-AgentGate-Task); empty: no limit (create)")
	velocity := c.fs.String("velocity", "", "most USD per rolling minute; empty: no limit (create)")
	approvalOver := c.fs.String("approval-over", "", "a single request above this many USD needs a human; empty: never (create)")
	keyTTL := c.fs.String("key-ttl", "", "key lifetime such as 90d or 12h; empty: never expires (create, rotate-key)")
	grace := c.fs.String("grace", "24h", "how long the old key keeps working, such as 24h; 0: stops now (rotate-key)")
	clearID := c.fs.Bool("clear", false, "remove the SPIFFE ID (set-spiffe)")
	sub, rest := args[0], args[1:]
	ref, value := "", ""
	switch sub {
	case "revoke", "set-spiffe", "api-key", "rotate-key", "key-expiry":
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return fmt.Errorf("agent %s: need an agent id or name first", sub)
		}
		ref, rest = rest[0], rest[1:]
	}
	// The value of set-spiffe (an ID, or "" to clear) and api-key (on|off) is
	// the next word; flags such as --clear may follow it or take its place.
	if sub == "set-spiffe" || sub == "api-key" || sub == "key-expiry" {
		if len(rest) > 0 && (!strings.HasPrefix(rest[0], "-") || rest[0] == "") {
			value, rest = rest[0], rest[1:]
		} else if sub != "set-spiffe" || !hasFlag(rest, "clear") {
			return fmt.Errorf("agent %s: need a value after the agent", sub)
		}
	}
	_, pool, err := c.open(ctx, rest)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := agents.NewStore(pool)
	switch sub {
	case "create":
		if *acct == "" || *name == "" {
			return errors.New("agent create: --account and --name are required")
		}
		sc := policy.Scope{Models: csv(*models), Tools: csv(*tools), Payees: csv(*payees)}
		var perr error
		for _, f := range []struct {
			dst  *money.Nanos
			n, v string
		}{{&sc.DailyBudget, "daily", *daily}, {&sc.TaskBudget, "task", *task}, {&sc.VelocityPerMinute, "velocity", *velocity}, {&sc.ApprovalThreshold, "approval-over", *approvalOver}} {
			if *f.dst, perr = usd(f.n, f.v); perr != nil {
				return perr
			}
		}
		ttl, perr := parseLifetime(*keyTTL)
		if perr != nil {
			return fmt.Errorf("--key-ttl: %w", perr)
		}
		secret, a, err := st.CreateWithTTL(ctx, *acct, *name, sc, ttl)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, secret) // the only time the secret exists in plaintext
		fmt.Fprintf(os.Stderr, "agent %s (%s): store the secret now, it cannot be shown again\n", a.Name, a.ID)
		if a.KeyExpiresAt != nil {
			fmt.Fprintf(os.Stderr, "the key expires %s\n", a.KeyExpiresAt.Local().Format(time.RFC3339))
		}
		if len(sc.Models) == 0 {
			fmt.Fprintln(os.Stderr, "note: --models is empty, so this agent may call no model")
		}
	case "list":
		list, err := st.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tACCOUNT\tSTATUS\tMODELS\tTOOLS\tPAYEES\tDAILY\tTASK\tPER-MIN\tAPPROVE-OVER\tKEY\tKEY EXPIRES\tSPIFFE-ID")
		for _, a := range list {
			s := a.Scope
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Name, a.AccountID, a.Status,
				strings.Join(s.Models, ","), strings.Join(s.Tools, ","), len(s.Payees),
				lim(s.DailyBudget), lim(s.TaskBudget), lim(s.VelocityPerMinute), lim(s.ApprovalThreshold), onOff(a.APIKeyEnabled), keyExpiry(a.KeyExpiresAt, time.Now()), dash(a.SpiffeID))
		}
		return tw.Flush()
	case "revoke":
		a, err := st.Revoke(ctx, ref)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "revoked %s (%s): its credential is refused from now on\n", a.Name, a.ID)
	case "set-spiffe":
		if *clearID {
			value = ""
		}
		a, err := st.SetSpiffeID(ctx, ref, value)
		if err != nil {
			return err
		}
		if a.SpiffeID == "" {
			fmt.Fprintf(stdout, "cleared the SPIFFE ID of %s (%s)\n", a.Name, a.ID)
		} else {
			fmt.Fprintf(stdout, "%s (%s) is now %s over mutual TLS\n", a.Name, a.ID, a.SpiffeID)
		}
	case "api-key":
		var on bool
		switch value {
		case "on":
			on = true
		case "off":
		default:
			return fmt.Errorf("agent api-key: want on or off, got %q", value)
		}
		a, err := st.SetAPIKeyEnabled(ctx, ref, on)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "api key of %s (%s) is %s\n", a.Name, a.ID, value)
		if !on && a.SpiffeID == "" {
			fmt.Fprintln(os.Stderr, "note: this agent has no SPIFFE ID, so it can no longer authenticate")
		}
	case "rotate-key":
		g, err := parseLifetime(*grace)
		if err != nil {
			return fmt.Errorf("--grace: %w", err)
		}
		ttl, err := parseLifetime(*keyTTL)
		if err != nil {
			return fmt.Errorf("--key-ttl: %w", err)
		}
		secret, a, err := st.RotateKey(ctx, ref, g, ttl)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, secret) // the only time the new secret exists in plaintext
		fmt.Fprintf(os.Stderr, "agent %s (%s): store the new secret now, it cannot be shown again\n", a.Name, a.ID)
		if g > 0 {
			fmt.Fprintf(os.Stderr, "the old key keeps working for %s\n", g)
		} else {
			fmt.Fprintln(os.Stderr, "the old key stopped working now")
		}
		if a.KeyExpiresAt != nil {
			fmt.Fprintf(os.Stderr, "the new key expires %s\n", a.KeyExpiresAt.Local().Format(time.RFC3339))
		}
	case "key-expiry":
		ttl, err := parseLifetime(value)
		if err != nil {
			return fmt.Errorf("agent key-expiry: %w", err)
		}
		a, err := st.SetKeyExpiry(ctx, ref, ttl)
		if err != nil {
			return err
		}
		if a.KeyExpiresAt == nil {
			fmt.Fprintf(stdout, "the key of %s (%s) never expires\n", a.Name, a.ID)
		} else {
			fmt.Fprintf(stdout, "the key of %s (%s) expires %s\n", a.Name, a.ID, a.KeyExpiresAt.Local().Format(time.RFC3339))
		}
	default:
		return fmt.Errorf("agent: unknown subcommand %q", sub)
	}
	return nil
}

// parseLifetime reads a duration such as 90d, 24h or 30m. "", "0" and "never"
// mean none (0). Days are 24 hours.
func parseLifetime(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "", "0", "never":
		return 0, nil
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.ParseUint(n, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("want a duration like 90d or 24h, got %q", v)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return 0, fmt.Errorf("want a duration like 90d or 24h, got %q", v)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("must not be negative, got %q", v)
	}
	return d, nil
}

// keyExpiry is the KEY EXPIRES column: a date, "never" or "expired".
func keyExpiry(t *time.Time, now time.Time) string {
	switch {
	case t == nil:
		return "never"
	case !now.Before(*t):
		return "expired"
	}
	return t.Local().Format("2006-01-02")
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--"+name || a == "-"+name {
			return true
		}
	}
	return false
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func lim(n money.Nanos) string {
	if n == 0 {
		return "-"
	}
	return "$" + n.String()
}

// killCmd: the kill switch (I12). It writes the database; a running gateway
// sees it on the next request and, within a poll tick, cancels in-flight calls.
func killCmd(ctx context.Context, args []string, stdout io.Writer) error {
	c := newCommon("kill")
	agent := c.fs.String("agent", "", "agent id or name")
	resume := c.fs.Bool("resume", false, "undo a kill (a revoked agent stays revoked)")
	_, pool, err := c.open(ctx, args)
	if err != nil {
		return err
	}
	defer pool.Close()
	if *agent == "" {
		return errors.New("kill: --agent required")
	}
	st := agents.NewStore(pool)
	if *resume {
		a, err := st.Resume(ctx, *agent)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "resumed %s (%s)\n", a.Name, a.ID)
		return nil
	}
	a, err := st.Kill(ctx, *agent)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "killed %s (%s): next request refused; in-flight calls are cancelled within a second\n", a.Name, a.ID)
	return nil
}

// approver names who is deciding: --by, else $AG_APPROVER, else the OS user.
func approver(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("AG_APPROVER"); v != "" {
		return v
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// approveCmd: approve list | allow ID | deny ID. This is the only way to
// approve: it needs the database, which an agent credential never has.
func approveCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("approve: need list | allow ID | deny ID")
	}
	c := newCommon("approve " + args[0])
	by := c.fs.String("by", "", "who is deciding (default $AG_APPROVER, else the OS user)")
	status := c.fs.String("status", "pending", "list: pending | approved | denied | expired | consumed | all")
	sub, rest, id := args[0], args[1:], ""
	if sub == "allow" || sub == "deny" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return fmt.Errorf("approve %s: need an approval id first", sub)
		}
		id, rest = rest[0], rest[1:]
	}
	_, pool, err := c.open(ctx, rest)
	if err != nil {
		return err
	}
	defer pool.Close()
	q := &approval.Queue{Store: approval.NewStore(pool), Ledger: ledger.New(pool)}
	switch sub {
	case "list":
		st := approval.Status(*status)
		if *status == "all" {
			st = ""
		}
		list, err := q.List(ctx, st)
		if err != nil {
			return err
		}
		names := map[string]string{}
		if all, err := agents.NewStore(pool).List(ctx); err == nil {
			for _, a := range all {
				names[a.ID] = a.Name
			}
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tAGENT\tKIND\tWHAT\tPAYEE\tAMOUNT\tSTATUS\tBY\tEXPIRES")
		for _, a := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t$%s\t%s\t%s\t%s\n", a.ID, names[a.AgentID], a.Kind, a.Subject, a.Payee,
				a.Amount, a.Status, a.DecidedBy, a.ExpiresAt.Local().Format(time.RFC3339))
		}
		return tw.Flush()
	case "allow", "deny":
		who := approver(*by)
		if who == "" {
			return errors.New("approve: cannot tell who you are; pass --by")
		}
		var a approval.Approval
		if sub == "allow" {
			a, err = q.Allow(ctx, id, who)
		} else {
			a, err = q.Deny(ctx, id, who)
		}
		if err != nil {
			return err
		}
		if sub == "allow" {
			fmt.Fprintf(stdout, "approved %s ($%s, %s %s) by %s: the agent may replay it once with the same Idempotency-Key\n", a.ID, a.Amount, a.Kind, a.Subject, who)
		} else {
			fmt.Fprintf(stdout, "denied %s by %s: its hold is released\n", a.ID, who)
		}
		return nil
	}
	return fmt.Errorf("approve: unknown subcommand %q", sub)
}
