// Package audit defines the append-only audit event. The writer arrives in T2.
package audit

import (
	"time"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/provider"
)

type Event struct {
	RequestID, AccountID, AgentID, RunID, Kind, Model, Provider, Status, CostSource string
	Usage                                                                           provider.Usage
	Cost                                                                            money.Nanos
	LatencyMs, OverheadMs                                                           int64
	Err                                                                             string
	At                                                                              time.Time
}
