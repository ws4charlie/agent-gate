package console

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
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

func fakeSudo(path string, err error) func(string) (string, error) {
	return func(string) (string, error) { return path, err }
}

// With a runner user the command is sudo with the exact environment names.
func TestRunnerCmdThroughSudo(t *testing.T) {
	s := &Server{cfg: Config{RunnerUser: "agentgate-runner", RunnerUserRunners: []string{"gpt-researcher"}}, lookPath: fakeSudo("/usr/bin/sudo", nil)}
	env := []string{"AG_RUN_ID=run_1", "AG_TOPIC=a=b, c", "PATH=/bin"}
	cmd, err := s.runnerCmd(true, "/opt/agentgate/bin/runner-gpt-researcher", env)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/sudo", "-n", "-u", "agentgate-runner", "--preserve-env=AG_RUN_ID,AG_TOPIC,PATH", "/opt/agentgate/bin/runner-gpt-researcher"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args %q, want %q", cmd.Args, want)
	}
}

func TestRunnerCmdWithoutUserIsTheProgram(t *testing.T) {
	s := &Server{lookPath: fakeSudo("", errors.New("must not be called"))}
	cmd, err := s.runnerCmd(false, "/x/runner", []string{"A=1"})
	if err != nil || !reflect.DeepEqual(cmd.Args, []string{"/x/runner"}) {
		t.Errorf("%q %v", cmd.Args, err)
	}
}

func TestRunnerCmdNeedsSudo(t *testing.T) {
	s := &Server{cfg: Config{RunnerUser: "r"}, lookPath: fakeSudo("", errors.New("not found"))}
	if _, err := s.runnerCmd(true, "/x", nil); err == nil || !strings.Contains(err.Error(), "needs sudo") {
		t.Errorf("err %v", err)
	}
}

// The runner fetches its own certificate: the console passes where and what to
// expect, and no certificate material.
func TestSelfFetchEnv(t *testing.T) {
	s := &Server{cfg: Config{RunnerUser: "r", RunnerSPIFFEID: "spiffe://x/runner", RunnerGatewayURL: "https://127.0.0.1:1",
		SPIFFESocket: "unix:///s.sock", SpireAgentBin: "/b/spire-agent"}}
	if !s.selfFetch() {
		t.Fatal("self fetch should be on")
	}
	want := []string{"AG_SPIFFE_SELF_FETCH=1", "AG_SPIFFE_SOCKET=unix:///s.sock", "AG_SPIFFE_EXPECT_ID=spiffe://x/runner", "AG_SPIRE_AGENT_BIN=/b/spire-agent"}
	if got := s.selfFetchEnv(); !reflect.DeepEqual(got, want) {
		t.Errorf("env %q, want %q", got, want)
	}
	s.cfg.RunnerUser = ""
	if s.selfFetch() {
		t.Error("self fetch needs a runner user")
	}
}

// A run in runner-user mode goes through sudo, preserves exactly the names it
// sets, never has the console fetch a certificate, and reads files the
// runner's group wrote.
func TestRunThroughSudoEndToEnd(t *testing.T) {
	name, group := currentUser(t)
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "sudo-args")
	// A sudo stand-in: records its arguments, then runs the program like sudo
	// does after its own options.
	sudo := filepath.Join(dir, "sudo")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nshift 4\nexec \"$@\"\n"
	if err := os.WriteFile(sudo, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := fakeRunner(t, `[ "$AG_SPIFFE_SELF_FETCH" = 1 ] || exit 7
[ "$AG_SPIFFE_EXPECT_ID" = spiffe://example.org/runner ] || exit 8
[ -z "$AG_SPIFFE_CERT" ] || exit 9
[ "$AG_GATEWAY_URL" = https://127.0.0.1:18443 ] || exit 10
echo "▸ hello"; echo ok > "$AG_RUN_DIR/report.md"`)
	fetched := false
	s, err := New(Config{Gateway: "http://127.0.0.1:1", DataDir: t.TempDir(), Runners: map[string]string{"gpt-researcher": runner},
		RunnerGatewayURL: "https://127.0.0.1:18443", RunnerUser: name, RunnerGroup: group, RunnerUserRunners: []string{"gpt-researcher"},
		RunnerSPIFFEID: "spiffe://example.org/runner", SPIFFESocket: "unix:///x.sock",
		FetchCert: func(context.Context) (*runCert, error) { fetched = true; return okCert(nil) }}, dbtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	s.lookPath = fakeSudo(sudo, nil)
	m := startAndWait(t, s, s.Handler(), `{"template_id":"agent-security"}`)
	if m.Status != stDone {
		t.Fatalf("status %s (%s): %s", m.Status, m.Error, eventTexts(t, s, m.ID))
	}
	if fetched {
		t.Error("the console fetched the runner's certificate")
	}
	raw, _ := os.ReadFile(argsFile)
	args := strings.Fields(string(raw))
	if len(args) < 5 || args[0] != "-n" || args[1] != "-u" || args[2] != name || args[4] != runner {
		t.Fatalf("sudo args %q", args)
	}
	keys, ok := strings.CutPrefix(args[3], "--preserve-env=")
	if !ok || !strings.Contains(","+keys+",", ",AG_SPIFFE_SELF_FETCH,") || strings.Contains(keys, "AG_SPIFFE_CERT") {
		t.Errorf("preserve-env %q", args[3])
	}
}

// Killing a run under sudo: TERM to sudo, KILL only if it is still there after
// the grace period.
func TestStopThroughSudo(t *testing.T) {
	var mu sync.Mutex
	var got []syscall.Signal
	send := func(sig syscall.Signal) { mu.Lock(); got = append(got, sig); mu.Unlock() }

	exited := make(chan struct{})
	close(exited) // leaves right after TERM
	stopThroughSudo(send, exited, time.Hour)
	if !reflect.DeepEqual(got, []syscall.Signal{syscall.SIGTERM}) {
		t.Errorf("runner left: signals %v, want TERM only", got)
	}

	got = nil
	stopThroughSudo(send, make(chan struct{}), 20*time.Millisecond) // never leaves
	if !reflect.DeepEqual(got, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Errorf("runner stayed: signals %v, want TERM then KILL", got)
	}
}

// The run directory is shared with the runner's group, setgid, and the data
// directory can be passed through but not listed by the group.
func TestRunDirShared(t *testing.T) {
	name, group := currentUser(t)
	s, err := New(Config{Gateway: "http://127.0.0.1:1", DataDir: t.TempDir(), RunnerUser: name, RunnerGroup: group,
		RunnerUserRunners: []string{"gpt-researcher"}, Runners: map[string]string{"gpt-researcher": "/x"}}, dbtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
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

// sudo refuses HOME, PATH and USER from the caller: the console never passes
// them, and passes nothing the sudoers rule does not list.
func TestSudoEnvIsTheAllowlist(t *testing.T) {
	t.Setenv("HOME", "/home/agentgate")
	t.Setenv("USER", "agentgate")
	t.Setenv("LANG", "C.UTF-8")
	env := runnerEnv("/d", "run_1", "t", "i", "http://g", "", "")
	env = append(env, (&Server{cfg: Config{RunnerUser: "r", RunnerSPIFFEID: "spiffe://x/r", SPIFFESocket: "unix:///s", SpireAgentBin: "/b"}}).selfFetchEnv()...)
	env = sudoEnv(env)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HOME" || k == "PATH" || k == "USER" || !slices.Contains(runnerSudoEnv, k) {
			t.Errorf("%s must not be passed through sudo", k)
		}
	}
	s := &Server{cfg: Config{RunnerUser: "r", RunnerUserRunners: []string{"gpt-researcher"}}, lookPath: fakeSudo("/usr/bin/sudo", nil)}
	cmd, _ := s.runnerCmd(true, "/p", env)
	list := strings.Split(strings.TrimPrefix(cmd.Args[4], "--preserve-env="), ",")
	for _, k := range list {
		if !slices.Contains(runnerSudoEnv, k) {
			t.Errorf("--preserve-env names %s, not in the allowlist", k)
		}
	}
	if !slices.Contains(list, "AG_SPIFFE_SELF_FETCH") || !slices.Contains(list, "LANG") {
		t.Errorf("list %v misses variables that were set", list)
	}
}

// Only the listed runners start through sudo; the others run as before, with
// the console's certificate and none of the self-fetch variables.
func TestOnlyListedRunnersUseSudo(t *testing.T) {
	name, group := currentUser(t)
	dir := t.TempDir()
	sudo := filepath.Join(dir, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\necho sudo > "+filepath.Join(dir, "used")+"\nshift 4\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	direct := fakeRunner(t, `[ -z "$AG_SPIFFE_SELF_FETCH" ] || exit 7
[ -n "$AG_SPIFFE_CERT" ] || exit 8
[ -n "$HOME" ] || exit 9
echo ok > "$AG_RUN_DIR/report.md"`)
	s, err := New(Config{Gateway: "http://127.0.0.1:1", DataDir: t.TempDir(),
		Runners:          map[string]string{"gpt-researcher": direct, "claude-code": "/unused"},
		RunnerGatewayURL: "https://127.0.0.1:18443", FetchCert: okCert, RunnerSPIFFEID: "spiffe://example.org/runner",
		RunnerUser: name, RunnerGroup: group, RunnerUserRunners: []string{"claude-code"}}, dbtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	s.lookPath = fakeSudo(sudo, nil)
	if !s.viaSudo("claude-code") || s.viaSudo("gpt-researcher") {
		t.Fatal("viaSudo wrong")
	}
	m := startAndWait(t, s, s.Handler(), `{"template_id":"agent-security"}`)
	if m.Status != stDone {
		t.Fatalf("status %s (%s): %s", m.Status, m.Error, eventTexts(t, s, m.ID))
	}
	if _, err := os.Stat(filepath.Join(dir, "used")); err == nil {
		t.Error("an unlisted runner went through sudo")
	}
	fi, _ := os.Stat(filepath.Join(s.cfg.DataDir, "runs", m.ID))
	if fi.Mode() != os.ModeDir|0o700 {
		t.Errorf("unlisted runner's run dir mode %v, want 0700", fi.Mode())
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
