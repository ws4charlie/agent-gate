// Package provider holds what is specific to an upstream wire format: usage
// parsing, routing, request building, and the chat-completions <-> Messages
// translation. It never touches the ledger.
package provider

import "github.com/ws4charlie/agent-gate/internal/money"

// Usage is what an upstream reported (or what we counted) for one call.
// CostNanos is nil when the provider reports no cost, so settlement can fall
// back to the price table (PLAN "Request flow", step 6).
type Usage struct {
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64
	CostNanos                                                    *money.Nanos
	UpstreamID                                                   string
}
