package mcp_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/mcp"
)

func TestParse(t *testing.T) {
	if _, e := mcp.Parse([]byte(`[{"jsonrpc":"2.0","id":1,"method":"x"}]`)); e == nil || e.Code != mcp.CodeInvalidRequest {
		t.Fatalf("batch: %v", e)
	}
	if _, e := mcp.Parse([]byte(`{`)); e == nil || e.Code != mcp.CodeParse {
		t.Fatalf("bad json: %v", e)
	}
	if _, e := mcp.Parse([]byte(`{"jsonrpc":"2.0","id":null,"method":"x"}`)); e == nil {
		t.Fatal("a null id is refused: the 2026-07-28 base protocol forbids it")
	}
	r, e := mcp.Parse([]byte(`{"jsonrpc":"2.0","method":"notifications/x"}`))
	if e != nil || !r.IsNotification() {
		t.Fatalf("notification: %v %v", r, e)
	}
}

func TestHeaderValueEncoding(t *testing.T) {
	for _, v := range []string{"echo", "héllo", " padded ", "=?base64?abc?=", "line1\nline2"} {
		got, ok := mcp.DecodeHeaderValue(mcp.EncodeHeaderValue(v))
		if !ok || got != v {
			t.Errorf("%q round-trips to %q", v, got)
		}
	}
	if mcp.EncodeHeaderValue("plain") != "plain" || !strings.HasPrefix(mcp.EncodeHeaderValue("=?base64?x?="), "=?base64?") {
		t.Fatal("plain stays plain; a lookalike of the sentinel is encoded")
	}
}

func TestReadEvents(t *testing.T) {
	in := ": keep-alive\n\nevent: message\ndata: {\"a\":1}\n\ndata: l1\ndata: l2\n\ndata: tail"
	var got []string
	var raw []string
	_ = mcp.ReadEvents(strings.NewReader(in), func(e mcp.Event) bool {
		got = append(got, string(e.Data))
		raw = append(raw, string(e.Raw))
		return true
	})
	if strings.Join(got, "|") != `|{"a":1}|l1`+"\n"+`l2|tail` {
		t.Fatalf("data %q", got)
	}
	if raw[1] != "event: message\ndata: {\"a\":1}\n\n" {
		t.Fatalf("raw %q", raw[1])
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.json")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	c, err := mcp.LoadConfig(write(`{"servers":{"a":{"url":"http://x","auth_env":"A_AUTH","default_price_nanos":5,"prices":{"t":0}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Servers["a"]
	if s.Price("t") != 0 || s.Price("other") != 5 {
		t.Fatalf("prices: %d %d", s.Price("t"), s.Price("other"))
	}
	if n, v := s.Credential(func(string) string { return "Bearer z" }); n != "Authorization" || v != "Bearer z" {
		t.Fatalf("%s %s", n, v)
	}
	for _, bad := range []string{`{"servers":{"a":{"url":"http://x","api_key":"sk-1"}}}`, `{"servers":{"a":{}}}`, `{"servers":{"a":{"url":"u","default_price_nanos":-1}}}`} {
		if _, err := mcp.LoadConfig(write(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestCheckLegacyInitializeNamesVersions(t *testing.T) {
	st, e := mcp.Check(http.Header{}, mcp.Request{JSONRPC: "2.0", ID: []byte("1"), Method: "initialize"})
	if st != 400 || e == nil || e.Code != mcp.CodeUnsupportedVer || !strings.Contains(e.Message, mcp.ProtocolVersion) {
		t.Fatalf("%d %v", st, e)
	}
}
