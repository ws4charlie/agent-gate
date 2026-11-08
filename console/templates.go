package console

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ws4charlie/agent-gate/internal/policy"
)

// A Template is a reusable research task: its own system prompt, model, tools
// and budgets. Each template runs as its own agent (agentPrefix + id), so its
// limits are the gateway's ordinary policy and its spend shows on its own row.
type Template struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	SystemPrompt string   `json:"system_prompt"`
	DefaultTopic string   `json:"default_topic"`
	Model        string   `json:"model"`
	Tools        []string `json:"tools"`  // "server/tool" for MCP tools, a bare name for a paid x402 tool
	Payees       []string `json:"payees"` // addresses the paid tools may be paid to
	RunBudget    string   `json:"run_budget_usd"`
	DailyBudget  string   `json:"daily_budget_usd"`
	MaxSteps     int      `json:"max_steps"`
	Builtin      bool     `json:"builtin"`
	Edited       bool     `json:"edited"` // a built-in the operator changed; deleting it restores the built-in
}

const (
	agentPrefix     = "console-"
	defaultMaxSteps = 6
	maxMaxSteps     = 20
)

var (
	errInvalid    = errors.New("invalid")
	errNoTemplate = errors.New("no such template")

	templateIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	payeeRe      = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
)

//go:embed templates/*.json
var builtinFS embed.FS

func builtins() (map[string]Template, error) {
	out := map[string]Template{}
	files, err := builtinFS.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		b, err := builtinFS.ReadFile("templates/" + f.Name())
		if err != nil {
			return nil, err
		}
		var t Template
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("built-in template %s: %w", f.Name(), err)
		}
		t.Builtin = true
		out[t.ID] = t
	}
	return out, nil
}

func ensureDirs(dir string) error {
	if dir == "" {
		return errors.New("console: no data directory")
	}
	for _, d := range []string{dir, filepath.Join(dir, "templates"), filepath.Join(dir, "keys"), filepath.Join(dir, "runs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) templatePath(id string) string {
	return filepath.Join(s.cfg.DataDir, "templates", id+".json")
}

// templates lists every task: the built-ins (or the operator's edited
// version of one) and the operator's own, in the order the operator chose;
// tasks not placed yet follow, built-ins first, then by name.
func (s *Server) templates() ([]Template, error) {
	b, err := builtins()
	if err != nil {
		return nil, err
	}
	byID := map[string]Template{}
	for id, t := range b {
		if len(t.Payees) == 0 {
			t.Payees = nonNil(s.cfg.DefaultPayees)
		}
		byID[id] = t
	}
	files, err := filepath.Glob(filepath.Join(s.cfg.DataDir, "templates", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var t Template
		if json.Unmarshal(raw, &t) != nil || t.ID+".json" != filepath.Base(f) {
			continue
		}
		_, t.Builtin = b[t.ID]
		t.Edited = t.Builtin
		byID[t.ID] = t
	}
	out := make([]Template, 0, len(byID))
	for _, t := range byID {
		out = append(out, t)
	}
	rank := map[string]int{}
	for i, id := range s.order() {
		rank[id] = i + 1
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[out[i].ID], rank[out[j].ID]
		switch {
		case ri != 0 && rj != 0:
			return ri < rj
		case ri != 0 || rj != 0:
			return ri != 0 // placed tasks come first
		case out[i].Builtin != out[j].Builtin:
			return out[i].Builtin
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Server) orderPath() string { return filepath.Join(s.cfg.DataDir, "order.json") }

// order is the task order the operator dragged into place, or nothing.
func (s *Server) order() []string {
	raw, err := os.ReadFile(s.orderPath())
	if err != nil {
		return nil
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil {
		return nil
	}
	return ids
}

func (s *Server) handleSetOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(body.IDs) > 500 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: too many ids", errInvalid))
		return
	}
	for _, id := range body.IDs {
		if !templateIDRe.MatchString(id) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: bad id %q", errInvalid, id))
			return
		}
	}
	raw, _ := json.Marshal(clean(body.IDs))
	if err := writeFileAtomic(s.orderPath(), raw); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) template(id string) (Template, error) {
	all, err := s.templates()
	if err != nil {
		return Template{}, err
	}
	for _, t := range all {
		if t.ID == id {
			return t, nil
		}
	}
	return Template{}, fmt.Errorf("%w: %s", errNoTemplate, id)
}

// validate fills defaults and checks every field the runner and policy use.
func (t *Template) validate() error {
	t.Name = strings.TrimSpace(t.Name)
	t.Model = strings.TrimSpace(t.Model)
	t.Tools, t.Payees = clean(t.Tools), clean(t.Payees)
	if t.MaxSteps == 0 {
		t.MaxSteps = defaultMaxSteps
	}
	switch {
	case !templateIDRe.MatchString(t.ID):
		return fmt.Errorf("%w: id must be lower-case letters, digits and '-', at most 40", errInvalid)
	case t.Name == "":
		return fmt.Errorf("%w: name is empty", errInvalid)
	case strings.TrimSpace(t.SystemPrompt) == "":
		return fmt.Errorf("%w: system prompt is empty", errInvalid)
	case t.Model == "" || strings.ContainsAny(t.Model, "*, "):
		return fmt.Errorf("%w: model must be one exact model name", errInvalid)
	case t.MaxSteps < 1 || t.MaxSteps > maxMaxSteps:
		return fmt.Errorf("%w: max steps must be 1-%d", errInvalid, maxMaxSteps)
	case len(t.SystemPrompt) > 8000 || len(t.DefaultTopic) > 2000 || len(t.Description) > 500:
		return fmt.Errorf("%w: text too long", errInvalid)
	}
	for _, tool := range t.Tools {
		if strings.Contains(tool, "*") || strings.Count(tool, "/") > 1 {
			return fmt.Errorf("%w: tool %q: name one tool, no wildcards", errInvalid, tool)
		}
	}
	for _, p := range t.Payees {
		if !payeeRe.MatchString(p) {
			return fmt.Errorf("%w: payee %q is not a 0x address", errInvalid, p)
		}
	}
	run, err := parseUSD(t.RunBudget)
	if err != nil || run <= 0 {
		return fmt.Errorf("%w: run budget must be a USD amount above 0", errInvalid)
	}
	daily, err := parseUSD(t.DailyBudget)
	if err != nil || daily <= 0 {
		return fmt.Errorf("%w: daily budget must be a USD amount above 0", errInvalid)
	}
	if run > daily {
		return fmt.Errorf("%w: run budget is above the daily budget", errInvalid)
	}
	return nil
}

// scope is the template's policy: one model, its tools and payees, a daily
// budget and a per-run budget (each run is its own X-AgentGate-Task).
func (t Template) scope() policy.Scope {
	run, _ := parseUSD(t.RunBudget)
	daily, _ := parseUSD(t.DailyBudget)
	return policy.Scope{Models: []string{t.Model}, Tools: nonNil(t.Tools), Payees: nonNil(t.Payees),
		DailyBudget: daily, TaskBudget: run}
}

func (s *Server) handleTemplates(w http.ResponseWriter, _ *http.Request) {
	all, err := s.templates()
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": all, "known_tools": nonNil(s.cfg.KnownTools),
		"default_payees": nonNil(s.cfg.DefaultPayees)})
}

// handleSaveTemplate creates or replaces a task. Saving a built-in keeps the
// operator's version beside it (deleting that restores the built-in). If the
// task's agent exists, its scope is updated at once.
func (s *Server) handleSaveTemplate(w http.ResponseWriter, r *http.Request) {
	var t Template
	if err := readJSON(r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := t.validate(); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	b, err := builtins()
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	_, t.Builtin = b[t.ID]
	t.Edited = t.Builtin
	raw, _ := json.MarshalIndent(t, "", "  ")
	if err := writeFileAtomic(s.templatePath(t.ID), raw); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if _, err := s.agents.SetScope(r.Context(), agentPrefix+t.ID, t.scope()); err != nil && !isNotFound(err) {
		writeErr(w, statusFor(err), fmt.Errorf("task saved, but its agent's scope was not updated: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleDeleteTemplate deletes one of the operator's tasks, or resets an
// edited built-in to the version that ships. The agent and its runs stay.
func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !templateIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: bad id", errInvalid))
		return
	}
	if err := os.Remove(s.templatePath(id)); err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("%w: %s (a built-in task can be reset after it is edited, not deleted)", errNoTemplate, id))
			return
		}
		writeErr(w, statusFor(err), err)
		return
	}
	if b, err := builtins(); err == nil {
		if t, ok := b[id]; ok {
			if len(t.Payees) == 0 {
				t.Payees = nonNil(s.cfg.DefaultPayees)
			}
			if _, err := s.agents.SetScope(r.Context(), agentPrefix+id, t.scope()); err != nil && !isNotFound(err) {
				writeErr(w, statusFor(err), fmt.Errorf("task reset, but its agent's scope was not updated: %w", err))
				return
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
