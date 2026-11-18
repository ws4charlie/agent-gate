package console

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
)

// currentUser is a user and group the test may chown to without privileges.
func currentUser(t *testing.T) (name, group string) {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Skip(err)
	}
	return u.Username, g.Name
}

// fakeUnits stands in for systemd: Start runs fn, Stop ends a Start that waits.
type fakeUnits struct {
	mu      sync.Mutex
	started []string
	stopped []string
	fn      func(ctx context.Context, unit string) error
	stop    chan struct{}
}

func newFakeUnits(fn func(ctx context.Context, unit string) error) *fakeUnits {
	return &fakeUnits{fn: fn, stop: make(chan struct{})}
}

func (f *fakeUnits) Start(ctx context.Context, unit string) error {
	f.mu.Lock()
	f.started = append(f.started, unit)
	f.mu.Unlock()
	return f.fn(ctx, unit)
}

func (f *fakeUnits) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, unit)
	select {
	case <-f.stop:
	default:
		close(f.stop)
	}
	return nil
}

func (f *fakeUnits) calls() (started, stopped []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...), append([]string(nil), f.stopped...)
}

func unitServer(t *testing.T, listed []string, runners map[string]string, units unitRunner) *Server {
	t.Helper()
	name, group := currentUser(t)
	s, err := New(Config{Gateway: "http://127.0.0.1:1", DataDir: t.TempDir(), Runners: runners,
		RunnerGatewayURL: "https://127.0.0.1:18443", FetchCert: okCert, RunnerSPIFFEID: "spiffe://example.org/runner",
		SPIFFESocket: "unix:///run/spire/agent/sockets/agent.sock", SpireAgentBin: "/opt/spire/bin/spire-agent",
		RunnerUser: name, RunnerGroup: group, RunnerUserRunners: listed}, dbtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	s.units = units
	return s
}

// readEnvFile parses the unit's EnvironmentFile.
func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok || !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `"`) {
			t.Fatalf("bad env line %q", l)
		}
		out[k] = v[1 : len(v)-1]
	}
	return out
}

// A listed runner is started as a unit: the console writes the environment
// file and the topic and instruction files, follows runner.out for the same
// steps and log as before, and never fetches the certificate itself.
func TestUnitRunEndToEnd(t *testing.T) {
	var s *Server
	fetched := false
	units := newFakeUnits(func(_ context.Context, unit string) error {
		id := strings.TrimSuffix(strings.TrimPrefix(unit, "agentgate-run@"), ".service")
		dir := filepath.Join(s.cfg.DataDir, "runs", id)
		env := readEnvFile(t, filepath.Join(dir, "runner.env"))
		want := map[string]string{"AG_RUN_ID": id, "AG_RUN_DIR": dir, "AG_TOPIC_FILE": filepath.Join(dir, "topic.txt"),
			"AG_INSTRUCTIONS_FILE": filepath.Join(dir, "instructions.txt"), "AG_GATEWAY_URL": "https://127.0.0.1:18443",
			"AG_SPIFFE_SELF_FETCH": "1", "AG_SPIFFE_SOCKET": "unix:///run/spire/agent/sockets/agent.sock",
			"AG_SPIFFE_EXPECT_ID": "spiffe://example.org/runner", "AG_SPIRE_AGENT_BIN": "/opt/spire/bin/spire-agent"}
		if !reflect.DeepEqual(env, want) {
			t.Errorf("env file %v, want %v", env, want)
		}
		topic, _ := os.ReadFile(env["AG_TOPIC_FILE"])
		ins, _ := os.ReadFile(env["AG_INSTRUCTIONS_FILE"])
		if len(topic) == 0 || !strings.Contains(string(ins), "Today is") {
			t.Errorf("topic %q, instructions %q", topic, ins)
		}
		for _, f := range []string{"topic.txt", "instructions.txt", "runner.env", "runner.out"} {
			if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != 0o660 {
				t.Errorf("%s: %v %v, want 0660", f, fi, err)
			}
		}
		out, _ := os.OpenFile(filepath.Join(dir, "runner.out"), os.O_APPEND|os.O_WRONLY, 0)
		defer out.Close()
		out.WriteString("▸ looking\nplain log line\n")
		time.Sleep(250 * time.Millisecond) // the console follows the file while the unit runs
		out.WriteString("▸ writing\n")
		return os.WriteFile(filepath.Join(dir, "report.md"), []byte("# R\n"), 0o660)
	})
	s = unitServer(t, []string{"gpt-researcher"}, map[string]string{"gpt-researcher": "/unused"}, units)
	s.cfg.FetchCert = func(context.Context) (*runCert, error) { fetched = true; return okCert(nil) }
	m := startAndWait(t, s, s.Handler(), `{"template_id":"agent-security"}`)
	if m.Status != stDone {
		t.Fatalf("status %s (%s): %s", m.Status, m.Error, eventTexts(t, s, m.ID))
	}
	if fetched {
		t.Error("the console fetched the runner's certificate")
	}
	ev := eventTexts(t, s, m.ID)
	for _, w := range []string{"step: looking", "step: writing", "the runner fetches its own SPIFFE certificate"} {
		if !strings.Contains(ev, w) {
			t.Errorf("events lack %q: %s", w, ev)
		}
	}
	log, _ := os.ReadFile(filepath.Join(s.cfg.DataDir, "runs", m.ID, "log.txt"))
	if !strings.Contains(string(log), "plain log line") {
		t.Errorf("log: %s", log)
	}
	if started, stopped := units.calls(); len(started) != 1 || started[0] != "agentgate-run@"+m.ID+".service" || len(stopped) != 0 {
		t.Errorf("started %v stopped %v", started, stopped)
	}
}

// The runner's exit status decides: non-zero is a failure, and a zero exit
// with no report is one too.
func TestUnitExitStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{"failed": {errors.New("exit status 1"), "the runner failed: exit status 1"}, "no report": {nil, "the runner stopped without a report"}} {
		t.Run(name, func(t *testing.T) {
			s := unitServer(t, []string{"gpt-researcher"}, map[string]string{"gpt-researcher": "/unused"},
				newFakeUnits(func(context.Context, string) error { return tc.err }))
			m := startAndWait(t, s, s.Handler(), `{"template_id":"agent-security"}`)
			if m.Status != stFailed || m.Error != tc.want {
				t.Errorf("status %s error %q, want failed %q", m.Status, m.Error, tc.want)
			}
		})
	}
}

// Killing a run stops the unit: systemd ends everything in it.
func TestUnitKillStopsTheUnit(t *testing.T) {
	var units *fakeUnits
	units = newFakeUnits(func(ctx context.Context, unit string) error {
		<-units.stop
		return errors.New("exit status 143")
	})
	s := unitServer(t, []string{"gpt-researcher"}, map[string]string{"gpt-researcher": "/unused"}, units)
	h := s.Handler()
	m := start(t, s, h, `{"template_id":"agent-security"}`)
	time.Sleep(200 * time.Millisecond)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, apiReq(s, "POST", "/api/runs/"+m.ID+"/kill", ""))
	if w.Code != 200 {
		t.Fatalf("kill: %d %s", w.Code, w.Body)
	}
	if got := wait(t, s, m.ID); got.Status != stKilled {
		t.Fatalf("after kill: %s", got.Status)
	}
	if _, stopped := units.calls(); !reflect.DeepEqual(stopped, []string{"agentgate-run@" + m.ID + ".service"}) {
		t.Errorf("stopped %v", stopped)
	}
}

// An unlisted runner starts as before: the program itself, the console's
// certificate, the whole environment, a private run directory, no unit.
func TestUnlistedRunnerIsUnchanged(t *testing.T) {
	direct := fakeRunner(t, `[ -z "$AG_SPIFFE_SELF_FETCH" ] || exit 7
[ -n "$AG_SPIFFE_CERT" ] || exit 8
[ -n "$AG_TOPIC" ] || exit 9
[ -z "$AG_TOPIC_FILE" ] || exit 10
echo ok > "$AG_RUN_DIR/report.md"`)
	units := newFakeUnits(func(context.Context, string) error { return errors.New("must not run") })
	s := unitServer(t, []string{"claude-code"}, map[string]string{"gpt-researcher": direct, "claude-code": "/unused"}, units)
	if s.viaUnit("gpt-researcher") || !s.viaUnit("claude-code") {
		t.Fatal("viaUnit wrong")
	}
	m := startAndWait(t, s, s.Handler(), `{"template_id":"agent-security"}`)
	if m.Status != stDone {
		t.Fatalf("status %s (%s): %s", m.Status, m.Error, eventTexts(t, s, m.ID))
	}
	if started, _ := units.calls(); len(started) != 0 {
		t.Errorf("unit started: %v", started)
	}
	if fi, _ := os.Stat(filepath.Join(s.cfg.DataDir, "runs", m.ID)); fi.Mode() != os.ModeDir|0o700 {
		t.Errorf("run dir mode %v, want 0700", fi.Mode())
	}
}

// An id the console did not make never becomes a unit name or a path.
func TestUnitRefusesBadRunID(t *testing.T) {
	units := newFakeUnits(func(context.Context, string) error { return nil })
	s := unitServer(t, []string{"gpt-researcher"}, map[string]string{"gpt-researcher": "/unused"}, units)
	for _, id := range []string{"run_../../x", "..", "run_x", "run_" + strings.Repeat("a", 36) + "/../x", ""} {
		tk := &task{s: s, dir: t.TempDir(), cancel: func() {}, meta: runMeta{ID: id, Status: stRunning}}
		tk.runUnit(context.Background(), "i")
		if m := tk.snapshot(); m.Status != stFailed {
			t.Errorf("id %q: status %s", id, m.Status)
		}
	}
	if started, _ := units.calls(); len(started) != 0 {
		t.Errorf("started %v", started)
	}
}

func TestSystemctlArgs(t *testing.T) {
	u := unitName("run_1")
	if got, want := systemctlArgs("start", true, u), []string{"--no-ask-password", "--quiet", "start", "--wait", "agentgate-run@run_1.service"}; !reflect.DeepEqual(got, want) {
		t.Errorf("start: %q", got)
	}
	if got, want := systemctlArgs("stop", false, u), []string{"--no-ask-password", "--quiet", "stop", "agentgate-run@run_1.service"}; !reflect.DeepEqual(got, want) {
		t.Errorf("stop: %q", got)
	}
}

func TestEnvFile(t *testing.T) {
	b, err := envFile([]string{"A=1", "B=https://x/y"})
	if err != nil || string(b) != "A=\"1\"\nB=\"https://x/y\"\n" {
		t.Errorf("%q %v", b, err)
	}
	for _, bad := range []string{"A=a\"b", "A=a\\b", "A=a\nb"} {
		if _, err := envFile([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The tail reader hands over what is written while it waits, and what arrived
// just before the end.
func TestTailReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	f, _ := os.Create(path)
	r, _ := os.Open(path)
	done := make(chan struct{})
	got := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 64)
		for {
			n, err := (&tailReader{f: r, done: done}).Read(buf)
			b.Write(buf[:n])
			if err != nil {
				got <- b.String()
				return
			}
		}
	}()
	f.WriteString("one\n")
	time.Sleep(250 * time.Millisecond)
	f.WriteString("two\n")
	close(done)
	select {
	case s := <-got:
		if s != "one\ntwo\n" {
			t.Errorf("read %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the reader did not end")
	}
}

// The runner's group id is found and the run directory is shared with it,
// setgid; the data directory can be passed through but not listed.
func TestRunDirShared(t *testing.T) {
	s := unitServer(t, []string{"gpt-researcher"}, map[string]string{"gpt-researcher": "/x"}, nil)
	for _, d := range []string{s.cfg.DataDir, filepath.Join(s.cfg.DataDir, "runs")} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o710 {
			t.Errorf("%s: %v %v, want 0710", d, fi, err)
		}
	}
	dir := filepath.Join(s.cfg.DataDir, "runs", "run_x")
	if err := s.makeRunDir(dir, "gpt-researcher"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o770 || fi.Mode()&os.ModeSetgid == 0 {
		t.Errorf("run dir mode %v, want setgid 0770", fi.Mode())
	}
	if st := fi.Sys().(*syscall.Stat_t); int(st.Gid) != s.runnerGID {
		t.Errorf("run dir group %d, want %d", st.Gid, s.runnerGID)
	}
}

// Without a runner user the run directory stays private to the console.
func TestRunDirPrivateWithoutRunnerUser(t *testing.T) {
	s := newTestServer(t, nil)
	dir := filepath.Join(s.cfg.DataDir, "runs", "run_x")
	if err := s.makeRunDir(dir, "gpt-researcher"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode() != os.ModeDir|0o700 {
		t.Errorf("mode %v, want 0700", fi.Mode())
	}
}

func TestUnknownRunnerUserFails(t *testing.T) {
	_, err := New(Config{DataDir: t.TempDir(), RunnerUser: "no-such-user-xyz", RunnerUserRunners: []string{"a"}, Runners: map[string]string{"a": "/x"}}, dbtest.New(t))
	if err == nil || !strings.Contains(err.Error(), "--runner-user") {
		t.Errorf("err %v", err)
	}
}

func TestRunnerUserNeedsTheRunnerList(t *testing.T) {
	_, err := New(Config{DataDir: t.TempDir(), RunnerUser: "x", Runners: map[string]string{"a": "/x"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "--runner-user-runners") {
		t.Errorf("err %v", err)
	}
	_, err = New(Config{DataDir: t.TempDir(), RunnerUser: "x", RunnerUserRunners: []string{"b"}, Runners: map[string]string{"a": "/x"}}, nil)
	if err == nil || !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("err %v", err)
	}
}

// Certificate directories a crashed console left behind are removed at start.
func TestStartRemovesLeftoverCertificates(t *testing.T) {
	data := t.TempDir()
	keep := filepath.Join(data, "runs", "run_a", "report.md")
	left := []string{filepath.Join(data, "runs", "run_a", ".svid-123"), filepath.Join(data, "runs", "run_b", ".svid-456")}
	for _, d := range append(left, filepath.Dir(keep)) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(left[0], "svid.key"), []byte("K"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("r"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := removeStaleCerts(data); n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	for _, d := range left {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s still exists", d)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the report was removed: %v", err)
	}
}
