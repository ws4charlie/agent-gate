package audit

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/pricing"
)

// Line is one report row.
type Line struct {
	Key                                  string
	Requests                             int64
	Spend, Unrecovered                   money.Nanos
	Input, Output, CacheRead, CacheWrite int64
	Savings                              money.Nanos // what the cache-read tokens saved versus plain input
	Unmetered                            int64       // holds the reconciler released with no usage found (I6)
	UnmeteredNanos                       money.Nanos // what those releases returned
	BySource                             map[string]SourceSplit
}

type SourceSplit struct {
	Requests int64
	Spend    money.Nanos
}

// HitRate is cache-read tokens over all input-side tokens.
func (l Line) HitRate() float64 {
	total := l.Input + l.CacheRead + l.CacheWrite
	if total == 0 {
		return 0
	}
	return float64(l.CacheRead) / float64(total)
}

// Report aggregates audit rows since a time, by "model" or "account".
// priceOf maps the client-facing model name to its price row (nil: no savings).
func Report(ctx context.Context, pool *pgxpool.Pool, since time.Time, by string, priceOf func(model string) (pricing.Price, bool)) ([]Line, error) {
	if by != "model" && by != "account" {
		return nil, fmt.Errorf("report: --by must be model or account, not %q", by)
	}
	rows, err := pool.Query(ctx, `SELECT account_id, model, cost_source,
		count(*) FILTER (WHERE status NOT LIKE 'reconciled\_%'), sum(cost_nanos), sum(unrecovered_nanos),
		sum(input_tokens), sum(output_tokens), sum(cache_read_tokens), sum(cache_write_tokens),
		count(*) FILTER (WHERE unmetered), sum(unmetered_nanos)
		FROM audit_events WHERE at >= $1 AND kind = 'model' GROUP BY account_id, model, cost_source`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agg := map[string]*Line{}
	for rows.Next() {
		var acct, model, src string
		var n int64
		var cost, unrec, in, out, cr, cw, um, umN int64
		if err := rows.Scan(&acct, &model, &src, &n, &cost, &unrec, &in, &out, &cr, &cw, &um, &umN); err != nil {
			return nil, err
		}
		key := model
		if key == "" {
			key = "(reconciler)"
		}
		if by == "account" {
			key = acct
		}
		l := agg[key]
		if l == nil {
			l = &Line{Key: key, BySource: map[string]SourceSplit{}}
			agg[key] = l
		}
		l.Requests += n // reconciler rows are not requests: the request itself has its own row
		l.Unmetered += um
		l.UnmeteredNanos += money.Nanos(umN)
		l.Spend += money.Nanos(cost)
		l.Unrecovered += money.Nanos(unrec)
		l.Input, l.Output, l.CacheRead, l.CacheWrite = l.Input+in, l.Output+out, l.CacheRead+cr, l.CacheWrite+cw
		if src == "" {
			src = "none" // refusals and failures that never priced anything
		}
		s := l.BySource[src]
		s.Requests += n
		s.Spend += money.Nanos(cost)
		l.BySource[src] = s
		if priceOf != nil {
			if p, ok := priceOf(model); ok && p.Input > p.CacheRead {
				if sv, err := money.Cost(cr, p.Input-p.CacheRead); err == nil {
					l.Savings += sv
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Line, 0, len(agg))
	for _, l := range agg {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Spend > out[j].Spend || out[i].Spend == out[j].Spend && out[i].Key < out[j].Key
	})
	return out, nil
}

// Print renders lines as a table followed by the cost-source split.
func Print(w io.Writer, by string, lines []Line) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\treqs\tspend USD\tinput\toutput\tcache read\tcache write\thit rate\test. savings USD\n", by)
	var total Line
	total.BySource = map[string]SourceSplit{}
	for _, l := range lines {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%d\t%d\t%d\t%.1f%%\t%s\n", l.Key, l.Requests, l.Spend, l.Input, l.Output,
			l.CacheRead, l.CacheWrite, l.HitRate()*100, l.Savings)
		total.Requests += l.Requests
		total.Spend += l.Spend
		total.Input += l.Input
		total.Output += l.Output
		total.CacheRead += l.CacheRead
		total.CacheWrite += l.CacheWrite
		total.Savings += l.Savings
		total.Unmetered += l.Unmetered
		total.UnmeteredNanos += l.UnmeteredNanos
		for k, v := range l.BySource {
			s := total.BySource[k]
			s.Requests += v.Requests
			s.Spend += v.Spend
			total.BySource[k] = s
		}
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%s\t%d\t%d\t%d\t%d\t%.1f%%\t%s\n", total.Requests, total.Spend, total.Input, total.Output,
		total.CacheRead, total.CacheWrite, total.HitRate()*100, total.Savings)
	tw.Flush()
	names := make([]string, 0, len(total.BySource))
	for k := range total.BySource {
		names = append(names, k)
	}
	sort.Strings(names)
	fmt.Fprint(w, "\ncost source:")
	for _, k := range names {
		s := total.BySource[k]
		fmt.Fprintf(w, "  %s=%d reqs / %s USD", k, s.Requests, s.Spend)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "unmetered: %d holds released with no usage found, %s USD returned\n", total.Unmetered, total.UnmeteredNanos)
}
