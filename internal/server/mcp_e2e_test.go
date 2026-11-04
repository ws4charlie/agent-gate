package server_test

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/mcp"
	"github.com/ws4charlie/agent-gate/internal/screen"
	"github.com/ws4charlie/agent-gate/internal/server"
)

// Against the reference server, over Streamable HTTP. Skipped unless
// AG_MCP_E2E=1, because it needs npm access to fetch a free local dev server
// (@modelcontextprotocol/server-everything); nothing paid or remote is called.
//
//	AG_MCP_E2E=1 AG_REQUIRE_DB=1 DATABASE_URL=... go test -run TestMCPEndToEndServerEverything -v ./internal/server
//
// The gateway speaks 2026-07-28. If the reference server only speaks an
// earlier revision, this test shows what it answers: that is the finding, not
// a reason to hide the failure.
func TestMCPEndToEndServerEverything(t *testing.T) {
	if os.Getenv("AG_MCP_E2E") != "1" {
		t.Skip("set AG_MCP_E2E=1 (needs npm access for a local dev server)")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cmd := exec.Command("npx", "-y", "@modelcontextprotocol/server-everything", "streamableHttp")
	cmd.Env = append(os.Environ(), "PORT="+itoaInt(port))
	cmd.Dir = t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(90 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoaInt(port), time.Second)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server-everything did not start")
		}
		time.Sleep(500 * time.Millisecond)
	}

	m := &mcpEnv{}
	m.env = newEnv(t, 10*usd, func(s *server.Server) {
		s.MCP = &mcp.Config{Servers: map[string]mcp.Server{"fake": {URL: "http://127.0.0.1:" + itoaInt(port) + "/mcp"}}}
		s.Screen = &screen.Screener{}
	})
	resp, b := m.rpc("tools/list", nil, nil)
	t.Logf("tools/list: %d %.300s", resp.StatusCode, b)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"tools"`) {
		t.Fatalf("tools/list through the gateway failed: %d %s", resp.StatusCode, b)
	}
	resp, b = m.call("echo", map[string]any{"message": "hello"}, nil)
	t.Logf("tools/call echo: %d %.300s", resp.StatusCode, b)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "hello") {
		t.Fatalf("tools/call through the gateway failed: %d %s", resp.StatusCode, b)
	}
}

func itoaInt(n int) string { return strconv.Itoa(n) }
