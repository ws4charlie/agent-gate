package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

func TestGuard(t *testing.T) {
	h := guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, c := range []struct {
		method, host, origin string
		want                 int
	}{
		{"GET", "127.0.0.1:18095", "", 200},
		{"GET", "localhost:18095", "", 200},
		{"GET", "[::1]:18095", "", 200},
		{"GET", "evil.example:18095", "", 403},                  // DNS rebinding
		{"POST", "127.0.0.1:18095", "", 403},                    // no Origin on a write
		{"POST", "127.0.0.1:18095", "http://evil.example", 403}, // another site posting to localhost
		{"POST", "127.0.0.1:18095", "http://127.0.0.1:18095", 200},
		{"PUT", "127.0.0.1:18095", "http://localhost:18095", 403}, // same machine, different origin
	} {
		r := httptest.NewRequest(c.method, "/api/x", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s host=%s origin=%q: %d, want %d", c.method, c.host, c.origin, w.Code, c.want)
		}
	}
}

func TestBuiltinTemplatesAreValid(t *testing.T) {
	b, err := builtins()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 3 {
		t.Fatalf("%d built-in tasks", len(b))
	}
	for id, tm := range b {
		if err := tm.validate(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		if tm.Runner != "gpt-researcher" && tm.Runner != "claude-code" {
			t.Errorf("%s: runner %q", id, tm.Runner)
		}
	}
}

func TestTemplateValidate(t *testing.T) {
	ok := Template{ID: "x", Name: "X", SystemPrompt: "p", DefaultTopic: "q", Runner: "gpt-researcher"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Template){
		"bad id":       func(t *Template) { t.ID = "../x" },
		"empty prompt": func(t *Template) { t.SystemPrompt = " " },
		"no question":  func(t *Template) { t.DefaultTopic = "" },
		"no runner":    func(t *Template) { t.Runner = "" },
		"bad runner":   func(t *Template) { t.Runner = "a b" },
		"long prompt":  func(t *Template) { t.SystemPrompt = strings.Repeat("x", 9000) },
	} {
		tm := ok
		mut(&tm)
		if err := tm.validate(); !errors.Is(err, errInvalid) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

// fakeRunner writes an executable that plays an agent's runner.
func fakeRunner(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// goodRunner checks what the console hands it, reports a step and writes a report.
const goodRunner = `case "$AG_RUN_ID" in run_*) ;; *) echo "bad run id"; exit 3 ;; esac
[ -n "$AG_TOPIC" ] || exit 4
case "$AG_INSTRUCTIONS" in *"Today is"*) ;; *) exit 5 ;; esac
[ -z "$PROVIDER_SECRET" ] || { echo "a console secret leaked"; exit 6; }
echo "▸ looking into $AG_TOPIC"
echo "some log line"
printf '# Report\n\nX [a](https://example.com/a)\n' > "$AG_RUN_DIR/report.md"`

func newTestServer(t *testing.T, runners map[string]string) *Server {
	t.Helper()
	s, err := New(Config{Gateway: "http://127.0.0.1:1", DataDir: t.TempDir(), Runners: runners}, dbtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// apiReq is a request as the console's own page sends it.
func apiReq(s *Server, method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:18095"
	r.Header.Set("Origin", "http://127.0.0.1:18095")
	r.Header.Set("X-Console-Key", s.Key())
	return r
}

func TestAPINeedsKey(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.Handler()
	for _, key := range []string{"", "wrong", s.Key() + "x"} {
		r := apiReq(s, "PUT", "/api/agents/x/scope", `{"daily_budget_usd":"1000"}`)
		r.Header.Set("X-Console-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("key %q: %d", key, w.Code)
		}
	}
	// The page itself needs no key; it gets the key from the printed link.
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "127.0.0.1:18095"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AgentGate") {
		t.Fatalf("page: %d", w.Code)
	}
	// The key survives a restart, so a bookmarked link keeps working.
	s2, err := New(s.cfg, s.pool)
	if err != nil || s2.Key() != s.Key() || len(s.Key()) < 40 {
		t.Fatalf("key changed on restart: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(s.cfg.DataDir, "console.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v %v", fi, err)
	}
}

func start(t *testing.T, s *Server, h http.Handler, body string) runMeta {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, apiReq(s, "POST", "/api/runs", body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var m runMeta
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m
}

func wait(t *testing.T, s *Server, id string) runMeta {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _, err := s.loadRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != stRunning {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return runMeta{}
}

func startAndWait(t *testing.T, s *Server, h http.Handler, body string) runMeta {
	t.Helper()
	return wait(t, s, start(t, s, h, body).ID)
}

// A task runs as the agent it names: the console starts that agent's runner
// with the run id, the question and the instructions, and nothing else from
// its own environment.
func TestRunStartsTheAgentsRunner(t *testing.T) {
	t.Setenv("PROVIDER_SECRET", "sk-should-not-leak")
	s := newTestServer(t, map[string]string{"gpt-researcher": fakeRunner(t, goodRunner)})
	h := s.Handler()
	m := startAndWait(t, s, h, `{"template_id":"agent-security","topic":"test topic"}`)
	if m.Status != stDone || m.Agent != "gpt-researcher" {
		_, ev, _ := s.loadRun(m.ID)
		t.Fatalf("status %s (%s), agent %s, events %+v", m.Status, m.Error, m.Agent, ev)
	}
	_, events, _ := s.loadRun(m.ID)
	var steps []string
	for _, e := range events {
		if e.Type == "step" {
			steps = append(steps, e.Text)
		}
	}
	if len(steps) != 1 || steps[0] != "looking into test topic" {
		t.Fatalf("steps %q", steps)
	}
	dir := filepath.Join(s.cfg.DataDir, "runs", m.ID)
	if log, _ := os.ReadFile(filepath.Join(dir, "log.txt")); !strings.Contains(string(log), "some log line") {
		t.Fatalf("log %q", log)
	}
	// The report is served as plain text, never as HTML; other files are not served.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, apiReq(s, "GET", "/api/runs/"+m.ID+"/files/report.md", ""))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || !strings.Contains(w.Body.String(), "example.com") {
		t.Fatalf("file: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, apiReq(s, "GET", "/api/runs/"+m.ID+"/files/meta.json", ""))
	if w.Code != 404 {
		t.Fatalf("a file outside the result list was served: %d", w.Code)
	}
	// A task whose agent has no runner here cannot start.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, apiReq(s, "POST", "/api/runs", `{"template_id":"agent-payments"}`))
	if w.Code != http.StatusConflict {
		t.Fatalf("claude-code task without its runner: %d", w.Code)
	}
}

func TestRunOutcomes(t *testing.T) {
	s := newTestServer(t, map[string]string{
		"gpt-researcher": fakeRunner(t, `echo "▸ started"`), // exits without a report
		"claude-code":    fakeRunner(t, `exit 1`),
	})
	h := s.Handler()
	if m := startAndWait(t, s, h, `{"template_id":"agent-security"}`); m.Status != stFailed {
		t.Fatalf("no report: %s", m.Status)
	}
	// A runner that stops after the gateway refused its run is "refused", with the reason.
	m := start(t, s, h, `{"template_id":"agent-payments"}`)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO audit_events (request_id, agent_id, run_id, kind, model, status, err, at)
		VALUES ('r1', 'a1', $1, 'model', 'claude-haiku-5-5', 'policy_denied', 'budget_task: over the per-task budget', now())`, m.ID); err != nil {
		t.Fatal(err)
	}
	if m = wait(t, s, m.ID); m.Status != stRefused || !strings.Contains(m.Error, "budget_task") {
		t.Fatalf("refused: %s %q", m.Status, m.Error)
	}
}

func TestKillStopsTheRunner(t *testing.T) {
	s := newTestServer(t, map[string]string{"gpt-researcher": fakeRunner(t, `echo "▸ working"; sleep 30`)})
	h := s.Handler()
	m := start(t, s, h, `{"template_id":"agent-security"}`)
	time.Sleep(200 * time.Millisecond)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, apiReq(s, "POST", "/api/runs/"+m.ID+"/kill", ""))
	if w.Code != 200 {
		t.Fatalf("kill: %d %s", w.Code, w.Body)
	}
	t0 := time.Now()
	if got := wait(t, s, m.ID); got.Status != stKilled || time.Since(t0) > 5*time.Second {
		t.Fatalf("after kill: %s in %s", got.Status, time.Since(t0))
	}
}

func TestOverviewQueries(t *testing.T) {
	s := newTestServer(t, map[string]string{"gpt-researcher": fakeRunner(t, goodRunner)})
	newAgent(t, s, "gpt-researcher")
	ov, err := s.overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Agents) != 1 || len(ov.Agents[0].Hourly) != 24 || !ov.Run.Enabled || ov.Run.Runners[0] != "gpt-researcher" {
		t.Fatalf("%+v", ov)
	}
	if _, err := s.events(context.Background(), eventFilter{Limit: 10}); err != nil {
		t.Fatal(err)
	}
}

func TestRunNeedsRunners(t *testing.T) {
	s := &Server{cfg: Config{Gateway: "http://127.0.0.1:1"}}
	if rr := s.runReady(); rr.Enabled {
		t.Fatal("tasks enabled without a runner")
	}
}

// newAgent makes an agent with its own funded account.
func newAgent(t *testing.T, s *Server, name string) string {
	t.Helper()
	ctx := context.Background()
	led := ledger.New(s.pool)
	acct, err := led.CreateAccount(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Deposit(ctx, acct, money.Nanos(5e9), "dep-"+name, ""); err != nil {
		t.Fatal(err)
	}
	_, a, err := s.agents.Create(ctx, acct, name, policy.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// A count on the overview links to the event feed filtered the same way, so the
// two must agree on what an error, a refusal and a block are.
func TestEventFiltersMatchCounts(t *testing.T) {
	s := newTestServer(t, nil)
	newAgent(t, s, "me")
	ctx := context.Background()
	ov, err := s.overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	me := ov.Agents[0]
	now := time.Now()
	for i, row := range []struct {
		agent, kind, model, status string
		cost                       int64
		at                         time.Time
	}{
		{me.ID, "model", "m/a", "ok", 5, now},
		{me.ID, "model", "m/a", "policy_denied", 0, now},
		{me.ID, "model", "m/a", "upstream_error", 0, now},
		{me.ID, "mcp", "srv/tool", "screened", 0, now},
		{me.ID, "payment", "paid", "reconciled_unmetered", 0, now},
		{me.ID, "model", "m/a", "upstream_error", 0, now.Add(-48 * time.Hour)},
		{"someone-else", "model", "m/b", "upstream_error", 7, now},
	} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO audit_events (request_id, agent_id, kind, model, status, cost_nanos, at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, "r"+strconv.Itoa(i), row.agent, row.kind, row.model, row.status, row.cost, row.at); err != nil {
			t.Fatal(err)
		}
	}
	if ov, err = s.overview(ctx); err != nil {
		t.Fatal(err)
	}
	me = ov.Agents[0]
	count := func(f eventFilter) int {
		t.Helper()
		f.Limit = 100
		ev, err := s.events(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(ev)
	}
	day := 24 * time.Hour
	for _, c := range []struct {
		name string
		f    eventFilter
		want int64
	}{
		{"refused", eventFilter{Agent: me.Name, Class: "refused", Since: day}, me.Refused},
		{"error", eventFilter{Agent: me.Name, Class: "error", Since: day}, me.Errors},
		{"blocked", eventFilter{Agent: me.Name, Class: "blocked", Since: day}, me.Blocked},
	} {
		if got := count(c.f); int64(got) != c.want || c.want == 0 {
			t.Errorf("%s: feed has %d rows, overview counts %d", c.name, got, c.want)
		}
	}
	if got := count(eventFilter{Agent: me.Name}); got != 4 { // refused, 2 errors, blocked; no notice
		t.Errorf("all problems for the agent: %d rows, want 4", got)
	}
	if got := count(eventFilter{Agent: me.Name, Class: "notice"}); got != 1 {
		t.Errorf("notices: %d rows, want 1", got)
	}
	if len(me.Models) != 1 || me.Models[0].Name != "m/a" || me.Models[0].Spend.Nanos != 5 {
		t.Errorf("agent's own model spend: %+v", me.Models)
	}
}

// A built-in task can be edited in place and reset; the operator's order sticks.
func TestEditResetAndOrderTasks(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.Handler()
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, apiReq(s, method, path, body))
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w.Body.Bytes()
	}
	list := func() []Template {
		t.Helper()
		var out struct{ Templates []Template }
		if err := json.Unmarshal(call("GET", "/api/templates", "", 200), &out); err != nil {
			t.Fatal(err)
		}
		return out.Templates
	}
	find := func(id string) Template {
		t.Helper()
		for _, x := range list() {
			if x.ID == id {
				return x
			}
		}
		t.Fatalf("no task %s", id)
		return Template{}
	}
	orig := find("agent-security")
	edited := orig
	edited.Name = "Security, my way"
	raw, _ := json.Marshal(edited)
	call("POST", "/api/templates", string(raw), 200)
	if got := find("agent-security"); got.Name != "Security, my way" || !got.Builtin || !got.Edited || len(list()) != 3 {
		t.Fatalf("after edit: %+v (%d tasks)", got, len(list()))
	}
	call("DELETE", "/api/templates/agent-security", "", 204)
	if got := find("agent-security"); got.Name != orig.Name || got.Edited {
		t.Fatalf("after reset: %+v", got)
	}
	call("DELETE", "/api/templates/agent-security", "", 404)

	call("PUT", "/api/templates/order", `{"ids":["ai-primary-market","agent-security"]}`, 204)
	var ids []string
	for _, x := range list() {
		ids = append(ids, x.ID)
	}
	if strings.Join(ids, ",") != "ai-primary-market,agent-security,agent-payments" {
		t.Fatalf("order: %v", ids)
	}
	call("PUT", "/api/templates/order", `{"ids":["../x"]}`, 400)
}

// Old task agents keep their names, and a finished run can be deleted.
func TestRenameAndDeleteRun(t *testing.T) {
	s := newTestServer(t, map[string]string{"gpt-researcher": fakeRunner(t, goodRunner)})
	h := s.Handler()
	id := startAndWait(t, s, h, `{"template_id":"agent-security","topic":"t"}`).ID
	call := func(method, path, body string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, apiReq(s, method, path, body))
		return w.Code
	}
	newAgent(t, s, "claude-code-1")
	newAgent(t, s, "console-agent-security") // a task agent made by an older console
	for _, c := range []struct {
		path, name string
		want       int
	}{
		{"/api/agents/claude-code-1/name", "claude-code", 200},
		{"/api/agents/claude-code/name", "console-x", 400},
		{"/api/agents/console-agent-security/name", "mine", 400},
		{"/api/agents/claude-code/name", "console-agent-security", 400},
	} {
		if got := call("PUT", c.path, `{"name":"`+c.name+`"}`); got != c.want {
			t.Errorf("%s -> %s: %d, want %d", c.path, c.name, got, c.want)
		}
	}
	if got := call("DELETE", "/api/runs/"+id, ""); got != 204 {
		t.Fatalf("delete run: %d", got)
	}
	if got := call("GET", "/api/runs/"+id, ""); got != 404 {
		t.Fatalf("deleted run still there: %d", got)
	}
}
