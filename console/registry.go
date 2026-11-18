package console

import (
	"context"
	"net/http"
	"time"

	"github.com/ws4charlie/agent-gate/internal/registry"
)

// registryWindow is how far back "in use" looks.
const registryWindow = 30 * 24 * time.Hour

type registryEntryView struct {
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	AgentName string    `json:"agent_name,omitempty"` // for kind agent: the agent's name
	Owner     string    `json:"owner"`
	Purpose   string    `json:"purpose"`
	Status    string    `json:"status"`
	Notes     string    `json:"notes"`
	UpdatedAt time.Time `json:"updated_at"`
	// InUse is set when a deprecated or blocked entry was seen in use in the window.
	InUse *inUseView `json:"in_use,omitempty"`
}

type inUseView struct {
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	AgentName string    `json:"agent_name,omitempty"`
	Status    string    `json:"status,omitempty"`
	Calls     int64     `json:"calls"`
	LastSeen  time.Time `json:"last_seen"`
}

// handleRegistry returns the catalogue and what is in use but not (or no
// longer) approved. It is read-only: the registry is edited with the CLI.
func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st := registry.NewStore(s.pool)
	since := time.Now().Add(-registryWindow)
	entries, err := st.List(ctx, "")
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	un, err := st.Unregistered(ctx, since)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	bad, err := st.InUseButNotApproved(ctx, since)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	names := map[string]string{}
	if list, err := s.agents.List(ctx); err == nil {
		for _, a := range list {
			names[a.ID] = a.Name
		}
	}
	agentName := func(kind, name string) string {
		if kind == "agent" {
			return names[name]
		}
		return ""
	}
	used := map[string]registry.InUse{}
	for _, u := range bad {
		used[u.Kind+"\x00"+u.Name] = u
	}
	out := struct {
		Since        time.Time           `json:"since"`
		Entries      []registryEntryView `json:"entries"`
		Unregistered []inUseView         `json:"unregistered"`
	}{Since: since, Entries: []registryEntryView{}, Unregistered: []inUseView{}}
	for _, e := range entries {
		v := registryEntryView{Kind: e.Kind, Name: e.Name, AgentName: agentName(e.Kind, e.Name), Owner: e.Owner,
			Purpose: e.Purpose, Status: e.Status, Notes: e.Notes, UpdatedAt: e.UpdatedAt}
		if u, ok := used[e.Kind+"\x00"+e.Name]; ok {
			v.InUse = &inUseView{Kind: u.Kind, Name: u.Name, Status: u.Status, Calls: u.Calls, LastSeen: u.LastSeen}
		}
		out.Entries = append(out.Entries, v)
	}
	for _, u := range un {
		out.Unregistered = append(out.Unregistered, inUseView{Kind: u.Kind, Name: u.Name, AgentName: agentName(u.Kind, u.Name),
			Calls: u.Calls, LastSeen: u.LastSeen})
	}
	writeJSON(w, http.StatusOK, out)
}
