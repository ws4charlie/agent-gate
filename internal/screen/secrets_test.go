package screen

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFindSecret(t *testing.T) {
	for _, c := range []struct {
		args, what, where string
	}{
		{`{"query":"why does sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123 fail"}`, "anthropic_api_key", "arguments.query"},
		{`{"q":"x","opts":{"headers":["Bearer ghp_abcdefghijklmnopqrstuvwxyz0123456789"]}}`, "github_token", "arguments.opts.headers[0]"},
		{`{"url":"https://admin:hunter2pass@db.example.com/x"}`, "password_in_url", "arguments.url"},
		{`{"key":"AKIAIOSFODNN7EXAMPLE"}`, "aws_access_key_id", "arguments.key"},
		{`{"pem":"-----BEGIN OPENSSH PRIVATE KEY-----\nb3Bl"}`, "private_key", "arguments.pem"},
	} {
		s, ok := FindSecret(json.RawMessage(c.args))
		if !ok || s.What != c.what || s.Where != c.where {
			t.Errorf("%s: got %+v %v, want %s at %s", c.args, s, ok, c.what, c.where)
		}
		if ok && s.Seen != "…" && strings.Count(s.Seen, "…") != 1 {
			t.Errorf("%s: seen %q", c.args, s.Seen)
		}
	}
	// Ordinary text, a transaction hash and a short sk- word are not secrets.
	for _, args := range []string{
		`{"query":"largest AI funding rounds this week"}`,
		`{"tx":"0x7a889db6ebf3a876e6e2035017a2624814f475f77a889db6ebf3a876e6e20350"}`,
		`{"query":"sk-learn tutorial"}`,
		`{"url":"https://example.com/a?b=c"}`,
	} {
		if s, ok := FindSecret(json.RawMessage(args)); ok {
			t.Errorf("%s: false positive %+v", args, s)
		}
	}
}

func TestSecretNeverShownWhole(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("Z", 40) + "a1f2"
	s, ok := FindSecret(json.RawMessage(`{"q":"` + key + `"}`))
	if !ok || strings.Contains(s.Seen, strings.Repeat("Z", 8)) || s.Seen != "sk-ant-…a1f2" {
		t.Fatalf("seen %q", s.Seen)
	}
}

func TestStripKey(t *testing.T) {
	out, v, found := StripKey(json.RawMessage(`{"b":1,"_c":"c_1","a":"x"}`), "_c")
	if !found || v != "c_1" || string(out) != `{"a":"x","b":1}` {
		t.Fatalf("%s %q %v", out, v, found)
	}
	in := json.RawMessage(`{"b":1}`)
	if out, _, found := StripKey(in, "_c"); found || string(out) != string(in) {
		t.Fatalf("absent key changed args: %s", out)
	}
}
