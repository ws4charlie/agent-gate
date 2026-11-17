package server

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

// Span attributes follow the OpenTelemetry GenAI semantic conventions
// (gen_ai.*); facts with no GenAI key live under agentgate.*. Values are ids,
// counts, names and status only: never prompts, completions or tool
// arguments and results (I9).

// noteDecision remembers the policy verdict for the span.
func (c *call) noteDecision(d policy.Decision) {
	c.decision, c.rule = string(d.Verdict), d.Rule
}

// identify sets the attributes that exist as soon as the caller is known.
func (c *call) identify() {
	c.span.SetAttributes(
		attribute.String("agentgate.account.id", c.agent.AccountID),
		attribute.String("agentgate.agent.id", c.agent.ID),
		attribute.String("gen_ai.agent.id", c.agent.ID),
		attribute.String("gen_ai.agent.name", c.agent.Name),
	)
}

// nameSpan renames the request span once the model or tool is known.
func (c *call) nameSpan() {
	switch {
	case c.tool != "":
		c.span.SetName("execute_tool " + c.tool)
	case c.ev.Kind == "model" && c.ev.Model != "":
		c.span.SetName("chat " + c.ev.Model)
	case c.ev.Kind == "mcp" && c.ev.Model != "":
		c.span.SetName("mcp " + c.ev.Model)
	}
}

// annotate writes the closing attributes of the span.
func (c *call) annotate(status string) {
	c.nameSpan()
	attrs := []attribute.KeyValue{
		attribute.String("agentgate.request.status", status),
		attribute.String("agentgate.cost.source", c.ev.CostSource),
		attribute.Int64("agentgate.cost.nanos", int64(c.ev.Cost)),
	}
	if c.ev.RunID != "" {
		attrs = append(attrs, attribute.String("agentgate.run.id", c.ev.RunID))
	}
	if c.decision != "" {
		attrs = append(attrs, attribute.String("agentgate.decision", c.decision), attribute.String("agentgate.rule", c.rule))
	}
	switch {
	case c.tool != "":
		attrs = append(attrs,
			attribute.String("gen_ai.operation.name", "execute_tool"),
			attribute.String("gen_ai.tool.name", c.tool),
			attribute.String("gen_ai.provider.name", c.ev.Provider))
	case c.ev.Kind == "model":
		attrs = append(attrs, attribute.String("gen_ai.operation.name", "chat"))
		if c.ev.Provider != "" {
			attrs = append(attrs, attribute.String("gen_ai.provider.name", c.ev.Provider))
		}
		if c.ev.Model != "" {
			attrs = append(attrs, attribute.String("gen_ai.request.model", c.ev.Model))
		}
		if id := c.ev.Usage.UpstreamID; id != "" {
			attrs = append(attrs, attribute.String("gen_ai.response.id", id))
		}
		attrs = append(attrs,
			attribute.Int64("gen_ai.usage.input_tokens", c.ev.Usage.InputTokens),
			attribute.Int64("gen_ai.usage.output_tokens", c.ev.Usage.OutputTokens))
	}
	c.span.SetAttributes(attrs...)
	if status != audit.StatusOK {
		c.span.SetAttributes(attribute.String("error.type", status))
		c.span.SetStatus(codes.Error, status)
	}
}
