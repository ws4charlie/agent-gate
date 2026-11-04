package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	runs "github.com/ws4charlie/agent-gate/internal/run"
)

// runCmd is `agentgate run list | show ID | kill ID`.
func runCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("run: need list | show ID | kill ID")
	}
	c := newCommon("run " + args[0])
	limit := c.fs.Int("limit", 20, "list: how many runs")
	_, pool, err := c.open(ctx, args[1:])
	if err != nil {
		return err
	}
	defer pool.Close()
	st := runs.New(pool)
	id := ""
	if rest := c.fs.Args(); len(rest) > 0 {
		id = rest[0]
	}
	switch args[0] {
	case "list":
		runs, err := st.List(ctx, *limit)
		if err != nil {
			return err
		}
		for _, r := range runs {
			spend, err := st.Spend(ctx, r.ID)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s  %-6s  spent %s USD  agent %s  %s\n", r.ID, r.Status, spend, r.AgentID, r.CreatedAt.Local().Format(time.RFC3339))
		}
	case "show":
		if id == "" {
			return errors.New("run show: need a run id")
		}
		r, err := st.Get(ctx, id)
		if err != nil {
			return err
		}
		steps, err := st.Steps(ctx, id)
		if err != nil {
			return err
		}
		spend, err := st.Spend(ctx, id)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "run %s  %s  agent %s\nspent %s USD (from the ledger)\n", r.ID, r.Status, r.AgentID, spend)
		for i, s := range steps {
			fmt.Fprintf(stdout, "%3d  %-7s %-24s %-20s cost %s USD  %s\n", i+1, s.Kind, s.Name, s.Status, s.Cost, s.StartedAt.Local().Format("15:04:05.000"))
		}
	case "kill":
		if id == "" {
			return errors.New("run kill: need a run id")
		}
		r, err := st.Kill(ctx, id)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "killed run %s: its next request is refused; in-flight calls are cancelled within a second\n", r.ID)
	default:
		return fmt.Errorf("run: unknown subcommand %q", args[0])
	}
	return nil
}
