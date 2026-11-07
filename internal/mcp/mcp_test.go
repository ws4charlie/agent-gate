package mcp_test

import (
	"encoding/json"
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

func TestLegacyMessagesAreDetectedByMissingMeta(t *testing.T) {
	legacy := mcp.Request{JSONRPC: "2.0", ID: []byte("1"), Method: "tools/list", Params: []byte(`{}`)}
	modern := mcp.Request{JSONRPC: "2.0", ID: []byte("1"), Method: "tools/list",
		Params: []byte(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`)}
	h := http.Header{}
	if !mcp.Legacy(h, legacy) || mcp.Legacy(h, modern) {
		t.Fatal("missing _meta must mean legacy, and only then")
	}
	h.Set("MCP-Protocol-Version", "2025-06-18")
	if !mcp.Legacy(h, legacy) {
		t.Fatal("a legacy version header is legacy")
	}
	h.Set("MCP-Protocol-Version", "1999-01-01")
	if mcp.Legacy(h, legacy) {
		t.Fatal("an unknown version must not be adapted")
	}
}

func TestInitializeOffersAKnownVersionAndNoSession(t *testing.T) {
	for in, want := range map[string]string{"2025-06-18": "2025-06-18", "1999-01-01": "2025-11-25"} {
		b := mcp.InitializeResponse(mcp.Request{ID: []byte("7"), Params: []byte(`{"protocolVersion":"` + in + `"}`)})
		var out struct {
			ID     int `json:"id"`
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if err := json.Unmarshal(b, &out); err != nil || out.ID != 7 || out.Result.ProtocolVersion != want {
			t.Fatalf("%s: %s", in, b)
		}
	}
}

func TestUpgradePassesTheModernChecks(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"go"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file:///a"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	} {
		r, perr := mcp.Parse([]byte(body))
		if perr != nil {
			t.Fatal(perr)
		}
		h := http.Header{}
		h.Set("Mcp-Name", "forged")
		out, err := mcp.Upgrade(h, r, []byte(body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		up, perr := mcp.Parse(out)
		if perr != nil {
			t.Fatalf("%s: %v", out, perr)
		}
		if st, e := mcp.Check(h, up); e != nil {
			t.Fatalf("%s: %d %v", out, st, e)
		}
		if mcp.Legacy(h, up) {
			t.Fatalf("still legacy: %s", out)
		}
	}
}
