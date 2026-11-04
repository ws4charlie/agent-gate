package fakeupstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastMessageTextBothFormats(t *testing.T) {
	cases := map[string]string{
		"chat string":     `{"messages":[{"role":"user","content":"first"},{"role":"user","content":"  second \n"}]}`,
		"chat parts":      `{"messages":[{"role":"user","content":[{"type":"text","text":"sec"},{"type":"text","text":"ond"}]}]}`,
		"chat tool":       `{"messages":[{"role":"user","content":"q"},{"role":"tool","tool_call_id":"c","content":"second"}]}`,
		"messages blocks": `{"messages":[{"role":"user","content":[{"type":"text","text":"second"}]}]}`,
		"messages result": `{"messages":[{"role":"user","content":"q"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"second"}]}]}`,
		"result blocks":   `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"second"}]}]}]}`,
	}
	for name, body := range cases {
		if got := lastMessageText([]byte(body)); got != "second" {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func fixtureFile(t *testing.T, dir, name, json string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
}

const okFixture = `{"id":"%s","match":{"model":"m","prompt":"%s"},"response":{"text":"hi"},
 "usage":{"input":1,"output":1,"cache_read":0,"cache_write":0},"cost":0.000001,"upstream_id":"gen-x"}`

func TestLoadFixturesRejectsMistakes(t *testing.T) {
	for name, files := range map[string][]string{
		"duplicate match": {strings.NewReplacer("%s", "a").Replace(okFixture), strings.Replace(strings.Replace(okFixture, "%s", "b", 1), "%s", "a", 1)},
		"unknown field":   {`{"id":"a","bogus":1}`},
		"no usage":        {`{"id":"a","match":{"model":"m","prompt":"p"},"response":{"text":"t"},"usage":{},"cost":1,"upstream_id":"gen-1"}`},
		"bad id":          {strings.Replace(strings.NewReplacer("%s", "a").Replace(okFixture), "gen-x", "x", 1)},
	} {
		dir := t.TempDir()
		for i, f := range files {
			fixtureFile(t, dir, string(rune('a'+i))+".json", f)
		}
		if _, err := LoadFixtures(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadFixtures(t.TempDir()); err == nil {
		t.Error("an empty directory was accepted")
	}
}

// The committed fixtures load and cover what the e2e tests rely on.
func TestRepoFixtures(t *testing.T) {
	fx, err := LoadFixtures(filepath.Join("..", "..", "testdata", "llm"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fx.All()); n < 12 {
		t.Fatalf("%d fixtures, want at least 12", n)
	}
	var tool, write, read, injected, noCost bool
	for _, f := range fx.All() {
		tool = tool || len(f.Response.ToolCalls) > 0
		write = write || f.Usage.CacheWrite > 0
		read = read || f.Usage.CacheRead > 0
		injected = injected || strings.Contains(strings.ToLower(f.Match.Prompt), "ignore all previous instructions")
		noCost = noCost || f.Usage.CostInline != nil && !*f.Usage.CostInline
	}
	if !tool || !write || !read || !injected || !noCost {
		t.Errorf("coverage: tool=%v cache_write=%v cache_read=%v injected=%v no_inline_cost=%v", tool, write, read, injected, noCost)
	}
}

func TestWordsRejoin(t *testing.T) {
	for _, s := range []string{"", "one", "one two  three ", " lead"} {
		if got := strings.Join(words(s), ""); got != s {
			t.Errorf("%q -> %q", s, got)
		}
	}
}
