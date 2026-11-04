package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
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

// agentCmd: agent create | list | revoke. Needs direct database access, like
// every administrative command: there is no HTTP route that changes an agent.
func agentCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("agent: need create | list | revoke")
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
	sub, rest := args[0], args[1:]
	ref := ""
	if sub == "revoke" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New("agent revoke: need an agent id or name first")
		}
		ref, rest = rest[0], rest[1:]
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
		secret, a, err := st.Create(ctx, *acct, *name, sc)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, secret) // the only time the secret exists in plaintext
		fmt.Fprintf(os.Stderr, "agent %s (%s): store the secret now, it cannot be shown again\n", a.Name, a.ID)
		if len(sc.Models) == 0 {
			fmt.Fprintln(os.Stderr, "note: --models is empty, so this agent may call no model")
		}
	case "list":
		list, err := st.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tACCOUNT\tSTATUS\tMODELS\tTOOLS\tPAYEES\tDAILY\tTASK\tPER-MIN\tAPPROVE-OVER")
		for _, a := range list {
			s := a.Scope
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", a.ID, a.Name, a.AccountID, a.Status,
				strings.Join(s.Models, ","), strings.Join(s.Tools, ","), len(s.Payees),
				lim(s.DailyBudget), lim(s.TaskBudget), lim(s.VelocityPerMinute), lim(s.ApprovalThreshold))
		}
		return tw.Flush()
	case "revoke":
		a, err := st.Revoke(ctx, ref)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "revoked %s (%s): its credential is refused from now on\n", a.Name, a.ID)
	default:
		return fmt.Errorf("agent: unknown subcommand %q", sub)
	}
	return nil
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
