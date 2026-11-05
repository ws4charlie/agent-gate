package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
)

const repoRoot = ".."

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

// binary builds ./cmd/agentgate once per test run and returns its path.
func binary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "agentgate-e2e-bin-")
		if buildErr != nil {
			return
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, "agentgate"), "./cmd/agentgate")
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(binDir, "agentgate")
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// dbURL rebuilds a connection string for the pool's database, so a gateway
// process and the `agentgate` CLI can open the same per-test database.
func dbURL(p *pgxpool.Pool) string {
	c := p.Config().ConnConfig
	if strings.HasPrefix(c.Host, "/") { // unix socket
		return fmt.Sprintf("postgres:///%s?host=%s&port=%d&user=%s", c.Database, url.QueryEscape(c.Host), c.Port, url.QueryEscape(c.User))
	}
	return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable", url.QueryEscape(c.User), c.Host, c.Port, c.Database)
}

func freeAddr(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// mockProviders writes configs/providers.mock.json with its fixed mock address
// swapped for the in-process mock's, and returns the new file's path. Every
// route still goes to the mock: the gateway has no mock branch.
func mockProviders(t testing.TB, mockURL string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "configs", "providers.mock.json"))
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(mockURL, "http://")
	out := strings.ReplaceAll(string(b), "127.0.0.1:18999", host)
	if out == string(b) {
		t.Fatal("providers.mock.json no longer names 127.0.0.1:18999")
	}
	p := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fixtureMock(t testing.TB) (*fakeupstream.Server, *fakeupstream.Fixtures) {
	t.Helper()
	fx, err := fakeupstream.LoadFixtures(filepath.Join(repoRoot, "testdata", "llm"))
	if err != nil {
		t.Fatal(err)
	}
	m := fakeupstream.New()
	m.UseFixtures(fx)
	t.Cleanup(m.Close)
	return m, fx
}

// proc is a running gateway process.
type proc struct {
	URL string
	env []string
	cmd *exec.Cmd
	log *bytes.Buffer
}

// gatewayEnv is the environment of every process the tests start: a key that
// cannot authenticate anywhere, and a dead proxy for anything that is not
// loopback, so a stray outbound call fails loudly instead of leaving the box.
func gatewayEnv(dburl, listen, providers, prices string) []string {
	return append(os.Environ(),
		"DATABASE_URL="+dburl, "AG_LISTEN="+listen,
		"AG_PROVIDERS="+providers, "AG_PRICES="+prices,
		"OPENROUTER_API_KEY=mock", "ANTHROPIC_API_KEY=mock",
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "NO_PROXY=127.0.0.1,localhost",
	)
}

func startGateway(t testing.TB, env []string, listen string, args ...string) *proc {
	t.Helper()
	p := &proc{URL: "http://" + listen, env: env, log: &bytes.Buffer{}}
	p.cmd = exec.Command(binary(t), append([]string{"serve"}, args...)...)
	p.cmd.Env = env
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(p.URL + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return p
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gateway did not start:\n%s", p.log.String())
	return nil
}

// cli runs `agentgate <args>` with the gateway's environment.
func (p *proc) cli(t testing.TB, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), binary(t), args...)
	cmd.Env = p.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("agentgate %v: %v\n%s%s", args, err, out.String(), errb.String())
	}
	return out.String()
}
