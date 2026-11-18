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
	if c.span2.secretConfirmed && d.Verdict == policy.Allow {
		return // the confirmed secret stays the span's reason; a later refusal still wins
	}
	c.decision, c.rule = string(d.Verdict), d.Rule
}

// spanFacts holds what annotate writes beyond the policy verdict.
type spanFacts struct {
	secretConfirmed bool
	payNetwork      string
	payAmount       int64
	payKnown        bool
}

// noteOutcome records a decision that did not come from the policy engine
// (the secret gate, the result screen). verdict is a policy verdict name.
func (c *call) noteOutcome(verdict, rule string) {
	c.decision, c.rule = verdict, rule
}

// notePayment records the network and amount of a paid-tool payment. Never
// the payee, wallet or signature.
func (c *call) notePayment(network string, amount int64) {
	c.span2.payNetwork, c.span2.payAmount, c.span2.payKnown = network, amount, true
}

// identify sets the attributes that exist as soon as the caller is known.
func (c *call) identify() {
	// How the caller proved who it is: a verified certificate or an API key.
	// Never the certificate or the key itself.
	auth := "api_key"
	if _, ok := SPIFFEIDFromContext(c.r.Context()); ok {
		auth = "spiffe"
	}
	c.span.SetAttributes(
		attribute.String("agentgate.auth", auth),
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
	if c.span2.payKnown {
		if c.span2.payNetwork != "" {
			attrs = append(attrs, attribute.String("agentgate.payment.network", c.span2.payNetwork))
		}
		attrs = append(attrs, attribute.Int64("agentgate.payment.amount_nanos", c.span2.payAmount))
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
		if m := c.ev.Usage.Model; m != "" {
			attrs = append(attrs, attribute.String("gen_ai.response.model", m))
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
