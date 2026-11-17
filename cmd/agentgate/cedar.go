package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ws4charlie/agent-gate/internal/cedarpolicy"
)

// cedarCmd: check DIR | shadow [--since 24h].
func cedarCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: agentgate cedar check DIR | shadow [--since 24h]")
	}
	switch args[0] {
	case "check":
		return cedarCheck(args[1:], stdout)
	case "shadow":
		return cedarShadow(ctx, args[1:], stdout)
	}
	return fmt.Errorf("cedar: unknown subcommand %q (check | shadow)", args[0])
}

// cedarCheck parses the policies and runs the built-in equivalence cases
// (the policy test table and 5,000 generated cases) against policy.Evaluate.
func cedarCheck(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: agentgate cedar check DIR")
	}
	e, err := cedarpolicy.Load(args[0])
	if err != nil {
		return err
	}
	cases := cedarpolicy.BuiltinCases(5000)
	diffs := cedarpolicy.Check(e, cases)
	for i, d := range diffs {
		if i == 20 {
			fmt.Fprintf(stdout, "... and %d more\n", len(diffs)-20)
			break
		}
		fmt.Fprintln(stdout, "DIFF", d)
	}
	if len(diffs) > 0 {
		return fmt.Errorf("cedar policies differ from the Go engine in %d of %d cases", len(diffs), len(cases))
	}
	fmt.Fprintf(stdout, "ok: %d cases, Cedar and the Go engine agree\n", len(cases))
	return nil
}

// cedarShadow summarises policy_decisions rows recorded with shadow on.
func cedarShadow(ctx context.Context, args []string, stdout io.Writer) error {
	c := newCommon("cedar shadow")
	since := c.fs.Duration("since", 24*time.Hour, "look back this far")
	_, pool, err := c.open(ctx, args)
	if err != nil {
		return err
	}
	defer pool.Close()
	cutoff := time.Now().Add(-*since)

	var total, agree, errs int64
	err = pool.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE cedar_verdict = decision AND cedar_rule = rule),
		count(*) FILTER (WHERE cedar_verdict = 'error')
		FROM policy_decisions WHERE cedar_verdict <> '' AND at >= $1`, cutoff).Scan(&total, &agree, &errs)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "since %s: %d decisions with shadow on, %d agree, %d differ, %d cedar errors\n",
		since.String(), total, agree, total-agree-errs, errs)
	rows, err := pool.Query(ctx, `SELECT rule, cedar_rule, decision, cedar_verdict, count(*) FROM policy_decisions
		WHERE cedar_verdict <> '' AND at >= $1 AND NOT (cedar_verdict = decision AND cedar_rule = rule)
		GROUP BY rule, cedar_rule, decision, cedar_verdict ORDER BY count(*) DESC, rule, cedar_rule LIMIT 10`, cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		var gr, cr, gv, cv string
		var n int64
		if err := rows.Scan(&gr, &cr, &gv, &cv, &n); err != nil {
			return err
		}
		if first {
			fmt.Fprintln(stdout, "top mismatches (go rule -> cedar rule):")
			first = false
		}
		fmt.Fprintf(stdout, "  %6d  %s/%s -> %s/%s\n", n, gv, gr, cv, cr)
	}
	return rows.Err()
}
