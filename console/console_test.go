package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
		t.Fatalf("%d built-in templates", len(b))
	}
	for id, tm := range b {
		if err := tm.validate(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		sc := tm.scope()
		if sc.DailyBudget <= 0 || sc.TaskBudget <= 0 || len(sc.Models) != 1 {
			t.Errorf("%s: scope %+v must cap every run and every day", id, sc)
		}
	}
}

func TestTemplateValidate(t *testing.T) {
	ok := Template{ID: "x", Name: "X", SystemPrompt: "p", Model: "m", RunBudget: "0.05", DailyBudget: "1"}
	if err := ok.validate(); err != nil || ok.MaxSteps != defaultMaxSteps {
		t.Fatalf("%v %d", err, ok.MaxSteps)
	}
	for name, mut := range map[string]func(*Template){
		"bad id":          func(t *Template) { t.ID = "../x" },
		"wildcard model":  func(t *Template) { t.Model = "claude-*" },
		"no run budget":   func(t *Template) { t.RunBudget = "" },
		"run over daily":  func(t *Template) { t.RunBudget = "2" },
		"wildcard tool":   func(t *Template) { t.Tools = []string{"*"} },
		"bad payee":       func(t *Template) { t.Payees = []string{"0x12"} },
		"too many steps":  func(t *Template) { t.MaxSteps = 99 },
		"empty prompt":    func(t *Template) { t.SystemPrompt = " " },
		"negative budget": func(t *Template) { t.DailyBudget = "-1" },
	} {
		tm := ok
		mut(&tm)
		if err := tm.validate(); !errors.Is(err, errInvalid) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

func TestParseGatewayError(t *testing.T) {
	e := parseGatewayError(403, []byte(`{"error":{"type":"policy_denied","code":"policy_denied","rule":"budget_task","message":"over"}}`)).(*gatewayError)
	if e.Code != "policy_denied" || e.Rule != "budget_task" || !e.refusal() {
		t.Fatalf("gateway shape: %+v", e)
	}
	e = parseGatewayError(400, []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad","data":{"code":"bad_request"}}}`)).(*gatewayError)
	if e.Code != "bad_request" || e.refusal() {
		t.Fatalf("JSON-RPC shape: %+v", e)
	}
	e = parseGatewayError(502, []byte(`<html>`)).(*gatewayError)
	if e.Code != "http_502" {
		t.Fatalf("other: %+v", e)
	}
}

func TestLastSSEData(t *testing.T) {
	got := lastSSEData([]byte("event: message\ndata: {\"a\":1}\n\ndata: {\"b\":\ndata: 2}\n\n"))
	if string(got) != `{"b":2}` {
		t.Fatalf("%q", got)
	}
}

// fakeGateway answers like the gateway: one model call that asks for the
// search tool, a paid MCP search, then the final answer. With refuse set it
// refuses the tool call the way policy does.
func fakeGateway(t *testing.T, refuse bool) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || r.Header.Get("X-AgentGate-Run") == "" ||
			r.Header.Get("X-AgentGate-Task") != r.Header.Get("X-AgentGate-Run") || r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("%s: missing gateway headers %v", r.URL.Path, r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mcp/litescraper":
			var req struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &req)
			if req.Method == "tools/list" {
				io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search the web","inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}]}}`)
				return
			}
			if refuse {
				io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"payee not allowed","data":{"code":"scope_payee","rule":"scope_payee"}}}`)
				return
			}
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Result: https://example.com/a says X"}]}}`)
		case "/v1/chat/completions":
			n := calls.Add(1)
			var req struct {
				Tools []json.RawMessage `json:"tools"`
			}
			_ = json.Unmarshal(body, &req)
			if n == 1 {
				if len(req.Tools) != 1 || !strings.Contains(string(req.Tools[0]), `"litescraper__search"`) {
					t.Errorf("tools offered: %s", req.Tools)
				}
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"litescraper__search","arguments":"{\"query\":\"x\"}"}}]}}]}`)
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"# Report\n\nX [a](https://example.com/a)"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newTestServer(t *testing.T, gateway string) *Server {
	t.Helper()
	pool := dbtest.New(t)
	led := ledger.New(pool)
	acct, err := led.CreateAccount(context.Background(), "console")
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Deposit(context.Background(), acct, money.Nanos(5e9), "dep", ""); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Gateway: gateway, Account: acct, DataDir: t.TempDir()}, pool)
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
	s := newTestServer(t, "http://127.0.0.1:1")
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

func startAndWait(t *testing.T, s *Server, h http.Handler, body string) runMeta {
	t.Helper()
	r := apiReq(s, "POST", "/api/runs", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var m runMeta
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _, err := s.loadRun(m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != stRunning {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return m
}

func TestRunWritesReportThroughGateway(t *testing.T) {
	gw, calls := fakeGateway(t, false)
	s := newTestServer(t, gw.URL)
	h := s.Handler()
	m := startAndWait(t, s, h, `{"template_id":"agent-security","topic":"test topic"}`)
	if m.Status != stDone || calls.Load() != 2 {
		t.Fatalf("status %s (%s), %d model calls", m.Status, m.Error, calls.Load())
	}
	dir := filepath.Join(s.cfg.DataDir, "runs", m.ID)
	report, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil || !strings.Contains(string(report), "https://example.com/a") {
		t.Fatalf("report %q %v", report, err)
	}
	if tools, _ := os.ReadFile(filepath.Join(dir, "tools.md")); !strings.Contains(string(tools), "litescraper/search") {
		t.Fatalf("tools.md %q", tools)
	}
	// The template's agent exists with the template's limits, and its key is
	// kept only in the data directory, readable by the owner alone.
	a, err := s.agents.Get(context.Background(), "console-agent-security")
	if err != nil || a.Scope.TaskBudget <= 0 || a.Scope.DailyBudget <= 0 || a.Scope.Models[0] != "openai/gpt-4o-mini" {
		t.Fatalf("agent %+v %v", a, err)
	}
	if fi, err := os.Stat(filepath.Join(s.cfg.DataDir, "keys", "agent-security.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v %v", fi, err)
	}
	// A second run reuses the agent and its key.
	if m2 := startAndWait(t, s, h, `{"template_id":"agent-security"}`); m2.Status != stDone || m2.Topic == "" {
		t.Fatalf("second run %+v", m2)
	}
	// The report is served as plain text, never as HTML.
	r := apiReq(s, "GET", "/api/runs/"+m.ID+"/files/report.md", "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("file: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	r = apiReq(s, "GET", "/api/runs/"+m.ID+"/files/meta.json", "")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("a file outside the result list was served: %d", w.Code)
	}
}

func TestRefusedToolIsToldToTheModel(t *testing.T) {
	gw, calls := fakeGateway(t, true)
	s := newTestServer(t, gw.URL)
	m := startAndWait(t, s, s.Handler(), `{"template_id":"agent-security","topic":"t"}`)
	// The refusal goes back to the model as the tool result; the model then
	// answers, so the run completes and the event log names the rule.
	if m.Status != stDone || calls.Load() != 2 {
		t.Fatalf("status %s, %d calls", m.Status, calls.Load())
	}
	_, events, _ := s.loadRun(m.ID)
	found := false
	for _, e := range events {
		if e.Type == "refused" && strings.Contains(e.Text, "scope_payee") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no refusal event: %+v", events)
	}
}

func TestOverviewQueries(t *testing.T) {
	gw, _ := fakeGateway(t, false)
	s := newTestServer(t, gw.URL)
	startAndWait(t, s, s.Handler(), `{"template_id":"agent-security","topic":"t"}`)
	ov, err := s.overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Agents) != 1 || !ov.Agents[0].Console || len(ov.Agents[0].Hourly) != 24 || !ov.Run.Enabled {
		t.Fatalf("%+v", ov)
	}
	if _, err := s.events(context.Background(), eventFilter{Limit: 10}); err != nil {
		t.Fatal(err)
	}
}

func TestRunNeedsAccount(t *testing.T) {
	s := &Server{cfg: Config{Gateway: "http://127.0.0.1:1"}}
	if rr := s.runReady(); rr.Enabled {
		t.Fatal("tasks enabled without an account")
	}
}

// A count on the overview links to the event feed filtered the same way, so the
// two must agree on what an error, a refusal and a block are.
func TestEventFiltersMatchCounts(t *testing.T) {
	gw, _ := fakeGateway(t, false)
	s := newTestServer(t, gw.URL)
	startAndWait(t, s, s.Handler(), `{"template_id":"agent-security","topic":"t"}`)
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
	s := newTestServer(t, "http://127.0.0.1:1")
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

// Renaming keeps task agents' names, and a finished run can be deleted.
func TestRenameAndDeleteRun(t *testing.T) {
	gw, _ := fakeGateway(t, false)
	s := newTestServer(t, gw.URL)
	h := s.Handler()
	id := startAndWait(t, s, h, `{"template_id":"agent-security","topic":"t"}`).ID
	call := func(method, path, body string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, apiReq(s, method, path, body))
		return w.Code
	}
	acct, err := ledger.New(s.pool).CreateAccount(context.Background(), "cc")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.agents.Create(context.Background(), acct, "claude-code-1", policy.Scope{}); err != nil {
		t.Fatal(err)
	}
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
