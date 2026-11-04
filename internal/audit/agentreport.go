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
)

// AgentLine is one agent's spend, model calls and tool payments side by side.
// Priced MCP tool calls ("tool") and x402 payments ("payment") are both tool
// spend, kept apart because only payments move money on chain.
type AgentLine struct {
	Agent                string // agent name, else id; "(none)" for rows with no agent
	ModelReqs, ToolReqs  int64
	PaymentReqs          int64
	Model, Tool, Payment money.Nanos
	PaymentsPending      int64 // payments whose outcome is not yet known (202)
}

func (l AgentLine) Total() money.Nanos { return l.Model + l.Tool + l.Payment }

// ReportByAgent sums audit rows since a time per agent. Reconciler rows carry
// the cost of a request that died or was pending, so their cost counts but they
// are not counted as requests (the request has its own row).
func ReportByAgent(ctx context.Context, pool *pgxpool.Pool, since time.Time) ([]AgentLine, error) {
	rows, err := pool.Query(ctx, `SELECT COALESCE(NULLIF(a.name,''), NULLIF(e.agent_id,''), '(none)'), e.kind,
		count(*) FILTER (WHERE e.status NOT LIKE 'reconciled\_%'),
		COALESCE(sum(e.cost_nanos),0),
		count(*) FILTER (WHERE e.status = 'settlement_pending')
		FROM audit_events e LEFT JOIN agents a ON a.id = e.agent_id
		WHERE e.at >= $1 AND e.kind IN ('model','mcp','payment')
		GROUP BY 1, 2`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agg := map[string]*AgentLine{}
	for rows.Next() {
		var name, kind string
		var n, cost, pend int64
		if err := rows.Scan(&name, &kind, &n, &cost, &pend); err != nil {
			return nil, err
		}
		l := agg[name]
		if l == nil {
			l = &AgentLine{Agent: name}
			agg[name] = l
		}
		switch kind {
		case "model":
			l.ModelReqs, l.Model = l.ModelReqs+n, l.Model+money.Nanos(cost)
		case "mcp":
			l.ToolReqs, l.Tool = l.ToolReqs+n, l.Tool+money.Nanos(cost)
		case "payment":
			l.PaymentReqs, l.Payment = l.PaymentReqs+n, l.Payment+money.Nanos(cost)
			l.PaymentsPending += pend
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AgentLine, 0, len(agg))
	for _, l := range agg {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Total() > out[j].Total() || out[i].Total() == out[j].Total() && out[i].Agent < out[j].Agent
	})
	return out, nil
}

// PrintAgents renders the side-by-side table.
func PrintAgents(w io.Writer, lines []AgentLine) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "agent\tmodel reqs\tmodel USD\ttool reqs\ttool USD\tpayment reqs\tpayments USD\tpending\ttotal USD")
	var t AgentLine
	for _, l := range lines {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%d\t%s\t%d\t%s\n", l.Agent, l.ModelReqs, l.Model, l.ToolReqs, l.Tool,
			l.PaymentReqs, l.Payment, l.PaymentsPending, l.Total())
		t.ModelReqs += l.ModelReqs
		t.ToolReqs += l.ToolReqs
		t.PaymentReqs += l.PaymentReqs
		t.PaymentsPending += l.PaymentsPending
		t.Model += l.Model
		t.Tool += l.Tool
		t.Payment += l.Payment
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%s\t%d\t%s\t%d\t%s\t%d\t%s\n", t.ModelReqs, t.Model, t.ToolReqs, t.Tool, t.PaymentReqs, t.Payment, t.PaymentsPending, t.Total())
	tw.Flush()
}
