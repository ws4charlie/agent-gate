package console

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
)

func certServer(t *testing.T, runner string, fetch CertFetcher) *Server {
	t.Helper()
	s, err := New(Config{Gateway: "http://127.0.0.1:1", DataDir: t.TempDir(),
		Runners:          map[string]string{"gpt-researcher": runner},
		RunnerGatewayURL: "https://127.0.0.1:18443", FetchCert: fetch}, dbtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func okCert(context.Context) (*runCert, error) {
	return &runCert{ID: "spiffe://example.org/runner", CertPEM: []byte("CERT"), KeyPEM: []byte("KEY"), BundlePEM: []byte("BUNDLE")}, nil
}

func eventTexts(t *testing.T, s *Server, id string) string {
	t.Helper()
	_, evs, _ := s.loadRun(id)
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Type + ": " + e.Text + "\n")
	}
	return b.String()
}

// The runner gets its certificate in a private directory, is pointed at the
// gateway's mutual-TLS address, and the directory is gone when the run ends.
func TestRunGetsACertificateAndItIsDeleted(t *testing.T) {
	// The runner reports where its files are and waits for the test to look at
	// them, so their modes are checked while they exist.
	s := certServer(t, fakeRunner(t, `
d=$(dirname "$AG_SPIFFE_CERT")
{
echo "url=$AG_GATEWAY_URL"
echo "dir=$d"
echo "cert=$(cat "$AG_SPIFFE_CERT") key=$(cat "$AG_SPIFFE_KEY") bundle=$(cat "$AG_SPIFFE_BUNDLE")"
} > "$AG_RUN_DIR/report.md"
touch "$AG_RUN_DIR/reported"
while [ ! -e "$AG_RUN_DIR/go" ]; do sleep 0.05; done`), okCert)
	h := s.Handler()
	m := start(t, s, h, `{"template_id":"agent-security"}`)
	runDir := filepath.Join(s.cfg.DataDir, "runs", m.ID)
	for i := 0; ; i++ {
		if _, err := os.Stat(filepath.Join(runDir, "reported")); err == nil {
			break
		}
		if i > 500 {
			t.Fatal("runner never reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rep, _ := os.ReadFile(filepath.Join(runDir, "report.md"))
	got := string(rep)
	var dir string
	for _, l := range strings.Split(got, "\n") {
		if v, ok := strings.CutPrefix(l, "dir="); ok {
			dir = v
		}
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("certificate directory %q: %v %v, want mode 0700", dir, fi, err)
	}
	for _, f := range []string{"svid.pem", "svid.key", "bundle.pem"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want mode 0600", f, fi, err)
		}
	}
	if err := os.WriteFile(filepath.Join(runDir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if m = wait(t, s, m.ID); m.Status != stDone {
		t.Fatalf("status %s (%s): %s", m.Status, m.Error, eventTexts(t, s, m.ID))
	}
	for _, want := range []string{"url=https://127.0.0.1:18443\n", "cert=CERT key=KEY bundle=BUNDLE"} {
		if !strings.Contains(got, want) {
			t.Errorf("runner saw %q, want it to contain %q", got, want)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("certificate directory %q still exists after the run (%v)", dir, err)
	}
	if ev := eventTexts(t, s, m.ID); !strings.Contains(ev, "running with a SPIFFE certificate (spiffe://example.org/runner)") ||
		strings.Contains(ev+got, "BEGIN") {
		t.Errorf("events: %s", ev)
	}
}

// No certificate: the run goes on with the API key and the log says so.
func TestRunFallsBackToAPIKeyWhenCertificateUnavailable(t *testing.T) {
	s := certServer(t, fakeRunner(t, `[ -z "$AG_SPIFFE_CERT" ] || exit 7
[ "$AG_GATEWAY_URL" = "http://127.0.0.1:1" ] || exit 8
echo ok > "$AG_RUN_DIR/report.md"`), func(context.Context) (*runCert, error) { return nil, errors.New("spire is down") })
	m := startAndWait(t, s, s.Handler(), `{"template_id":"agent-security"}`)
	if m.Status != stDone {
		t.Fatalf("status %s (%s)", m.Status, m.Error)
	}
	ev := eventTexts(t, s, m.ID)
	if !strings.Contains(ev, "certificate unavailable, using API key") || strings.Contains(ev, "error:") {
		t.Errorf("events: %s", ev)
	}
}

// Not configured: nothing changes and nothing is said.
func TestRunWithoutCertificateConfigIsUnchanged(t *testing.T) {
	s := certServer(t, fakeRunner(t, `[ -z "$AG_SPIFFE_CERT" ] || exit 7
echo ok > "$AG_RUN_DIR/report.md"`), nil)
	m := startAndWait(t, s, s.Handler(), `{"template_id":"agent-security"}`)
	if m.Status != stDone {
		t.Fatalf("status %s (%s)", m.Status, m.Error)
	}
	if ev := eventTexts(t, s, m.ID); strings.Contains(ev, "certificate") {
		t.Errorf("events mention a certificate: %s", ev)
	}
}

// A killed run leaves no certificate behind either.
func TestKilledRunDeletesItsCertificate(t *testing.T) {
	s := certServer(t, fakeRunner(t, `echo "▸ working"; sleep 30`), okCert)
	h := s.Handler()
	m := start(t, s, h, `{"template_id":"agent-security"}`)
	runDir := filepath.Join(s.cfg.DataDir, "runs", m.ID)
	time.Sleep(300 * time.Millisecond)
	if left, _ := filepath.Glob(filepath.Join(runDir, ".svid-*")); len(left) != 1 {
		t.Fatalf("expected one certificate directory while running, got %v", left)
	}
	s.mu.Lock()
	tk := s.live[m.ID]
	s.mu.Unlock()
	tk.markKilled()
	if m = wait(t, s, m.ID); m.Status != stKilled {
		t.Fatalf("status %s", m.Status)
	}
	time.Sleep(100 * time.Millisecond)
	if left, _ := filepath.Glob(filepath.Join(runDir, ".svid-*")); len(left) != 0 {
		t.Fatalf("certificate directory left behind: %v", left)
	}
}

func TestWriteRunCertModes(t *testing.T) {
	dir, err := writeRunCert(t.TempDir(), &runCert{CertPEM: []byte("c"), KeyPEM: []byte("k"), BundlePEM: []byte("b")})
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
	for _, f := range []string{"svid.pem", "svid.key", "bundle.pem"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
}
