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

// cedarShadow summarises policy_decisions rows recorded with Cedar on (shadow or enforce mode).
func cedarShadow(ctx context.Context, args []string, stdout io.Writer) error {
	c := newCommon("cedar shadow")
	since := c.fs.Duration("since", 24*time.Hour, "look back this far")
	_, pool, err := c.open(ctx, args)
	if err != nil {
		return err
	}
	defer pool.Close()
	cutoff := time.Now().Add(-*since)

	// The Go engine's answer is go_verdict/go_rule when Cedar enforced (then
	// decision/rule are Cedar's), else decision/rule.
	const goSide = `SELECT CASE WHEN go_verdict <> '' THEN go_verdict ELSE decision END AS gv,
		CASE WHEN go_verdict <> '' THEN go_rule ELSE rule END AS gr, cedar_verdict AS cv, cedar_rule AS cr
		FROM policy_decisions WHERE cedar_verdict <> '' AND at >= $1`
	var total, agree, errs int64
	err = pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE cv = gv AND cr = gr),
		count(*) FILTER (WHERE cv = 'error') FROM (`+goSide+`) t`, cutoff).Scan(&total, &agree, &errs)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "since %s: %d decisions with Cedar on, %d agree, %d differ, %d cedar errors\n",
		since.String(), total, agree, total-agree-errs, errs)
	rows, err := pool.Query(ctx, `SELECT gr, cr, gv, cv, count(*) FROM (`+goSide+`) t
		WHERE NOT (cv = gv AND cr = gr)
		GROUP BY gr, cr, gv, cv ORDER BY count(*) DESC, gr, cr LIMIT 10`, cutoff)
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
