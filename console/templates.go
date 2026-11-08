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
)

// A Template is a task: what to look into, the instructions, and which of
// the operator's agents does it (its runner). The agent's own limits apply:
// its per-task budget caps each run, its daily budget caps the day. The API
// still calls tasks templates.
type Template struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt"` // the task's instructions
	DefaultTopic string `json:"default_topic"` // what a run looks into unless told otherwise
	Runner       string `json:"runner"`        // the agent that runs it, e.g. gpt-researcher
	Builtin      bool   `json:"builtin"`
	Edited       bool   `json:"edited"` // a built-in the operator changed; deleting it restores the built-in
}

// agentPrefix marked the agents older consoles made for each task. Such
// names stay reserved so they cannot be confused with an operator's agent.
const agentPrefix = "console-"

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
	for _, d := range []string{dir, filepath.Join(dir, "templates"), filepath.Join(dir, "runs")} {
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

// validate checks every field a run uses.
func (t *Template) validate() error {
	t.Name = strings.TrimSpace(t.Name)
	t.Runner = strings.TrimSpace(t.Runner)
	switch {
	case !templateIDRe.MatchString(t.ID):
		return fmt.Errorf("%w: id must be lower-case letters, digits and '-', at most 40", errInvalid)
	case t.Name == "":
		return fmt.Errorf("%w: name is empty", errInvalid)
	case strings.TrimSpace(t.SystemPrompt) == "":
		return fmt.Errorf("%w: instructions are empty", errInvalid)
	case strings.TrimSpace(t.DefaultTopic) == "":
		return fmt.Errorf("%w: say what a run looks into", errInvalid)
	case !runnerNameRe.MatchString(t.Runner):
		return fmt.Errorf("%w: pick the agent that runs it", errInvalid)
	case len(t.SystemPrompt) > 8000 || len(t.DefaultTopic) > 2000 || len(t.Description) > 500:
		return fmt.Errorf("%w: text too long", errInvalid)
	}
	return nil
}

func (s *Server) handleTemplates(w http.ResponseWriter, _ *http.Request) {
	all, err := s.templates()
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": all, "runners": s.runnerNames()})
}

// handleSaveTemplate creates or replaces a task. Saving a built-in keeps the
// operator's version beside it (deleting that restores the built-in).
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
	writeJSON(w, http.StatusOK, t)
}

// handleDeleteTemplate deletes one of the operator's tasks, or resets an
// edited built-in to the version that ships. Its past runs stay.
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
	w.WriteHeader(http.StatusNoContent)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
