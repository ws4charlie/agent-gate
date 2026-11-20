package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/user"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/pricing"
	"github.com/ws4charlie/agent-gate/internal/registry"
)

// registryCmd: registry add | list | status | remove | gaps | history | seed.
// With the gateway's --registry-enforce on, an entry's status decides whether a
// model or tool may be used: only "approved" passes.
func registryCmd(ctx context.Context, args []string, stdout io.Writer) error {
	const subs = "add | list | status | remove | gaps | history | seed"
	if len(args) == 0 {
		return errors.New("registry: need " + subs)
	}
	sub, rest := args[0], args[1:]
	// Positional words come first, flags after, as with the agent commands.
	var pos []string
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	want := map[string]int{"add": 2, "status": 3, "remove": 2, "list": 0, "gaps": 0, "history": -1, "seed": 0}
	n, ok := want[sub]
	if !ok {
		return fmt.Errorf("registry: unknown subcommand %q (want %s)", sub, subs)
	}
	if sub == "history" && len(pos) != 0 && len(pos) != 2 {
		return errors.New("registry history: give <kind> <name>, or nothing for everything")
	}
	if n >= 0 && len(pos) != n {
		return fmt.Errorf("registry %s: %s", sub, map[string]string{"add": "need <kind> <name>", "status": "need <kind> <name> <status>",
			"remove": "need <kind> <name>", "list": "takes only flags", "gaps": "takes only flags", "seed": "takes only flags"}[sub])
	}
	c := newCommon("registry " + sub)
	owner := c.fs.String("owner", "", "who is responsible: a person or team (add)")
	purpose := c.fs.String("purpose", "", "what it is for (add)")
	status := c.fs.String("status", "", "approved | proposed | deprecated | blocked (add; default proposed, or unchanged on update)")
	notes := c.fs.String("notes", "", "add: free-text notes on the entry; status, remove: the reason, kept in the history")
	kind := c.fs.String("kind", "", "only this kind: model | tool | agent (list)")
	tier := c.fs.String("risk", "", "low | medium | high (add; default medium, or unchanged on update)")
	version := c.fs.String("version", "", "model snapshot or tool release (add; default empty, or unchanged on update)")
	actor := c.fs.String("actor", "", "who is making the change, for the history (default: your OS user)")
	dry := c.fs.Bool("dry-run", false, "show what would be added, change nothing (seed)")
	since := c.fs.String("since", "30d", "look back this far, e.g. 24h or 30d (gaps)")
	_, pool, err := c.open(ctx, rest)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := registry.NewStore(pool)
	if *actor == "" {
		*actor = osActor()
	}
	switch sub {
	case "add":
		e, err := st.Upsert(ctx, registry.Entry{Kind: pos[0], Name: pos[1], Owner: *owner, Purpose: *purpose, Status: *status, Notes: *notes,
			RiskTier: *tier, Version: *version, Actor: *actor})
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
		fmt.Fprintln(tw, "KIND\tNAME\tSTATUS\tRISK\tVERSION\tOWNER\tPURPOSE\tUPDATED")
		for _, e := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Kind, e.Name, e.Status, e.RiskTier, e.Version, e.Owner, e.Purpose, e.UpdatedAt.Format("2006-01-02"))
		}
		return tw.Flush()
	case "status":
		e, err := st.SetStatus(ctx, pos[0], pos[1], pos[2], *actor, *notes)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s is now %s\n", e.Kind, e.Name, e.Status)
	case "remove":
		if err := st.Delete(ctx, pos[0], pos[1], *actor, *notes); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s %s\n", pos[0], pos[1])
	case "history":
		var k, name string
		if len(pos) == 2 {
			k, name = pos[0], pos[1]
		}
		h, err := st.History(ctx, k, name, 0)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "WHEN\tKIND\tNAME\tFROM\tTO\tACTOR\tNOTE")
		for _, ch := range h {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ch.At.Local().Format("2006-01-02 15:04"), ch.Kind, ch.Name,
				orDash(ch.From, "(new)"), orDash(ch.To, "(removed)"), ch.Actor, ch.Note)
		}
		return tw.Flush()
	case "seed":
		return registrySeed(ctx, st, pool, *c.prices, *actor, *dry, stdout)
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

func osActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return registry.DefaultActor
}

func orDash(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// registrySeed approves, as "seeded from live config", every model in the
// price table and every exact model or tool name on an active agent's
// allowlist that has no entry yet. Wildcards are listed, not seeded. Existing
// entries are never changed. Safe to run again.
func registrySeed(ctx context.Context, st *registry.Store, pool *pgxpool.Pool, pricesPath, actor string, dry bool, w io.Writer) error {
	seen := map[registry.SeedItem]bool{}
	var items []registry.SeedItem
	add := func(kind, name string) {
		it := registry.SeedItem{Kind: kind, Name: name}
		if !seen[it] {
			seen[it] = true
			items = append(items, it)
		}
	}
	pt, err := pricing.Load(pricesPath)
	if err != nil {
		return fmt.Errorf("registry seed: price table: %w", err)
	}
	for _, m := range pt.Names() {
		add("model", m)
	}
	list, err := agents.NewStore(pool).List(ctx)
	if err != nil {
		return err
	}
	wild := map[string]bool{}
	var wilds []string
	for _, a := range list {
		if a.Status != policy.StatusActive {
			continue
		}
		for _, p := range []struct {
			kind string
			xs   []string
		}{{"model", a.Scope.Models}, {"tool", a.Scope.Tools}} {
			for _, n := range p.xs {
				switch {
				case strings.Contains(n, "*"):
					if k := p.kind + " " + n; !wild[k] {
						wild[k] = true
						wilds = append(wilds, k)
					}
				case n != "":
					add(p.kind, n)
				}
			}
		}
	}
	added, err := st.Seed(ctx, items, actor, dry)
	if err != nil {
		return err
	}
	verb := "added"
	if dry {
		verb = "would add"
	}
	fmt.Fprintf(w, "%s %d approved entries (%d already registered)\n", verb, len(added), len(items)-len(added))
	for _, it := range added {
		fmt.Fprintf(w, "  %s %s\n", it.Kind, it.Name)
	}
	sort.Strings(wilds)
	for _, k := range wilds {
		fmt.Fprintf(w, "not seeded: wildcard %s\n", k)
	}
	return nil
}
