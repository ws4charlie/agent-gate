package console

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"time"

	runs "github.com/ws4charlie/agent-gate/internal/run"
)

var runIDRe = regexp.MustCompile(`^run_[0-9a-f-]{36}$`)

func (s *Server) runDir(id string) (string, error) {
	if !runIDRe.MatchString(id) {
		return "", fmt.Errorf("%w: bad run id", errInvalid)
	}
	return filepath.Join(s.cfg.DataDir, "runs", id), nil
}

// loadRun reads a run's record: from memory while it runs, else from disk. A
// run left "running" by a console that stopped is shown as failed.
func (s *Server) loadRun(id string) (runMeta, []runEvent, error) {
	dir, err := s.runDir(id)
	if err != nil {
		return runMeta{}, nil, err
	}
	s.mu.Lock()
	tk := s.live[id]
	s.mu.Unlock()
	var meta runMeta
	if tk != nil {
		meta = tk.snapshot()
	} else {
		raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if os.IsNotExist(err) {
			return runMeta{}, nil, fmt.Errorf("%w: %s", runs.ErrNotFound, id)
		}
		if err != nil {
			return runMeta{}, nil, err
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return runMeta{}, nil, err
		}
		if meta.Status == stRunning {
			meta.Status, meta.Error = stFailed, "the console stopped while this run was going"
		}
	}
	var events []runEvent
	if f, err := os.Open(filepath.Join(dir, "events.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var e runEvent
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				events = append(events, e)
			}
		}
		_ = f.Close()
	}
	return meta, events, nil
}

type stepView struct {
	Kind     string     `json:"kind"`
	Name     string     `json:"name"`
	Status   string     `json:"status"`
	Cost     usd        `json:"cost"`
	Started  time.Time  `json:"started_at"`
	Finished *time.Time `json:"finished_at"`
}

type fileView struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type runView struct {
	runMeta
	Events  []runEvent `json:"events"`
	Steps   []stepView `json:"steps"`
	Spend   usd        `json:"spend"`
	Files   []fileView `json:"files"`
	Gateway []event    `json:"gateway_events"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	id := r.PathValue("id")
	meta, events, err := s.loadRun(id)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	v := runView{runMeta: meta, Events: nonNilEvents(events), Steps: []stepView{}, Files: []fileView{}}
	steps, err := s.runs.Steps(ctx, id)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	for _, st := range steps {
		v.Steps = append(v.Steps, stepView{Kind: st.Kind, Name: st.Name, Status: st.Status, Cost: dollars(int64(st.Cost)),
			Started: st.StartedAt, Finished: st.FinishedAt})
	}
	spend, err := s.runs.Spend(ctx, id)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	v.Spend = dollars(int64(spend))
	if v.Gateway, err = s.events(ctx, eventFilter{RunID: id, Class: "any", Limit: 50}); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	dir, _ := s.runDir(id)
	for _, name := range resultFiles {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil {
			v.Files = append(v.Files, fileView{Name: name, Size: fi.Size()})
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func nonNilEvents(e []runEvent) []runEvent {
	if e == nil {
		return []runEvent{}
	}
	return e
}

type runSummary struct {
	runMeta
	Spend usd `json:"spend"`
}

// handleRuns lists the console's runs, newest first; ?template= keeps one task's.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	dirs, err := os.ReadDir(filepath.Join(s.cfg.DataDir, "runs"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	task := r.URL.Query().Get("template") // only this task's runs
	var metas []runMeta
	for _, d := range dirs {
		if !d.IsDir() || !runIDRe.MatchString(d.Name()) {
			continue
		}
		if m, _, err := s.loadRun(d.Name()); err == nil && (task == "" || m.TemplateID == task) {
			metas = append(metas, m)
		}
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].StartedAt.After(metas[j].StartedAt) })
	if len(metas) > 40 {
		metas = metas[:40]
	}
	out := []runSummary{}
	for _, m := range metas {
		spend, err := s.runs.Spend(ctx, m.ID)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		out = append(out, runSummary{runMeta: m, Spend: dollars(int64(spend))})
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// handleKillRun marks the run killed in the gateway's database, so its next
// request is refused and its in-flight call is cancelled, then stops the loop.
func (s *Server) handleKillRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.runDir(id); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if _, err := s.runs.Kill(r.Context(), id); err != nil && !errors.Is(err, runs.ErrNotFound) {
		writeErr(w, statusFor(err), err)
		return
	}
	s.mu.Lock()
	tk := s.live[id]
	s.mu.Unlock()
	if tk != nil {
		tk.emit("refused", "killed from the console")
		tk.markKilled()
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": stKilled})
}

// handleDeleteRun removes a finished run's files from this machine: its
// report, the raw tool results and the transcript. The gateway's own audit
// rows (costs and statuses, no content) stay.
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dir, err := s.runDir(id)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.mu.Lock()
	tk := s.live[id]
	s.mu.Unlock()
	if tk != nil && tk.snapshot().Status == stRunning {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: the run is still going; kill it first", errInvalid))
		return
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%w: %s", runs.ErrNotFound, id))
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	s.mu.Lock()
	delete(s.live, id)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunFile(w http.ResponseWriter, r *http.Request) {
	dir, err := s.runDir(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	name := r.PathValue("name")
	if !slices.Contains(resultFiles, name) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%w: no such file", runs.ErrNotFound))
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%w: %s", runs.ErrNotFound, name))
		return
	}
	// Served as plain text: the page renders it, and a report written by a
	// model from web pages must never run as HTML on this origin.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}
