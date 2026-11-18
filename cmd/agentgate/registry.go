package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ws4charlie/agent-gate/internal/registry"
)

// registryCmd: registry add | list | status | remove | gaps. The registry is
// a record for the operator; nothing here changes what the gateway allows.
func registryCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("registry: need add | list | status | remove | gaps")
	}
	sub, rest := args[0], args[1:]
	// Positional words come first, flags after, as with the agent commands.
	var pos []string
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	want := map[string]int{"add": 2, "status": 3, "remove": 2, "list": 0, "gaps": 0}
	n, ok := want[sub]
	if !ok {
		return fmt.Errorf("registry: unknown subcommand %q (want add | list | status | remove | gaps)", sub)
	}
	if len(pos) != n {
		return fmt.Errorf("registry %s: %s", sub, map[string]string{"add": "need <kind> <name>", "status": "need <kind> <name> <status>",
			"remove": "need <kind> <name>", "list": "takes only flags", "gaps": "takes only flags"}[sub])
	}
	c := newCommon("registry " + sub)
	owner := c.fs.String("owner", "", "who is responsible: a person or team (add)")
	purpose := c.fs.String("purpose", "", "what it is for (add)")
	status := c.fs.String("status", "", "approved | proposed | deprecated | blocked (add; default proposed, or unchanged on update)")
	notes := c.fs.String("notes", "", "free-text notes (add)")
	kind := c.fs.String("kind", "", "only this kind: model | tool | agent (list)")
	since := c.fs.String("since", "30d", "look back this far, e.g. 24h or 30d (gaps)")
	_, pool, err := c.open(ctx, rest)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := registry.NewStore(pool)
	switch sub {
	case "add":
		e, err := st.Upsert(ctx, registry.Entry{Kind: pos[0], Name: pos[1], Owner: *owner, Purpose: *purpose, Status: *status, Notes: *notes})
		if errors.Is(err, registry.ErrBadEntry) {
			return errors.New("registry add: --owner and --purpose are required")
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s: %s, owner %s\n", e.Kind, e.Name, e.Status, e.Owner)
	case "list":
		list, err := st.List(ctx, *kind)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "KIND\tNAME\tSTATUS\tOWNER\tPURPOSE\tUPDATED")
		for _, e := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Kind, e.Name, e.Status, e.Owner, e.Purpose, e.UpdatedAt.Format("2006-01-02"))
		}
		return tw.Flush()
	case "status":
		e, err := st.SetStatus(ctx, pos[0], pos[1], pos[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s is now %s\n", e.Kind, e.Name, e.Status)
	case "remove":
		if err := st.Delete(ctx, pos[0], pos[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s %s\n", pos[0], pos[1])
	case "gaps":
		from, err := parseSince(*since, time.Now())
		if err != nil {
			return err
		}
		un, err := st.Unregistered(ctx, from)
		if err != nil {
			return err
		}
		bad, err := st.InUseButNotApproved(ctx, from)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "In use, not registered (last %s):\n", *since)
		printInUse(stdout, un, false)
		fmt.Fprintf(stdout, "\nIn use, registered as deprecated or blocked (last %s):\n", *since)
		printInUse(stdout, bad, true)
	}
	return nil
}

func printInUse(w io.Writer, list []registry.InUse, withStatus bool) {
	if len(list) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	head := "  KIND\tNAME\tCALLS\tLAST SEEN"
	if withStatus {
		head += "\tSTATUS"
	}
	fmt.Fprintln(tw, head)
	for _, u := range list {
		line := fmt.Sprintf("  %s\t%s\t%d\t%s", u.Kind, u.Name, u.Calls, u.LastSeen.Local().Format("2006-01-02 15:04"))
		if withStatus {
			line += "\t" + u.Status
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
}
