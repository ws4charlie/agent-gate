package agents

import (
	"context"
	"sync"
)

// Registry holds the cancel funcs of in-flight calls, by agent. It is the
// in-process half of the kill switch (I12): the kill itself is a database
// write (so the CLI, a separate process, can do it), and the gateway cancels
// what it finds running for an agent that is no longer active.
//
// A single gateway process is a stated limit of the project; with several the
// registry would need a shared channel.
type Registry struct {
	mu   sync.Mutex
	next uint64
	m    map[string]map[uint64]context.CancelFunc
}

// Add registers a cancel func for an agent and returns the function that
// removes it (call it when the request is done).
func (r *Registry) Add(agentID string, cancel context.CancelFunc) (remove func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]map[uint64]context.CancelFunc{}
	}
	if r.m[agentID] == nil {
		r.m[agentID] = map[uint64]context.CancelFunc{}
	}
	r.next++
	id := r.next
	r.m[agentID][id] = cancel
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.m[agentID], id)
		if len(r.m[agentID]) == 0 {
			delete(r.m, agentID)
		}
	}
}

// Cancel cancels every registered call of an agent and returns how many.
func (r *Registry) Cancel(agentID string) int {
	r.mu.Lock()
	fns := make([]context.CancelFunc, 0, len(r.m[agentID]))
	for _, f := range r.m[agentID] {
		fns = append(fns, f)
	}
	r.mu.Unlock()
	for _, f := range fns {
		f()
	}
	return len(fns)
}

// AgentIDs lists the agents with at least one call in flight.
func (r *Registry) AgentIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.m))
	for id := range r.m {
		out = append(out, id)
	}
	return out
}
