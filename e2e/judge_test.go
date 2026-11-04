package e2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/screen"
	"github.com/ws4charlie/agent-gate/internal/server"
)

// The optional screen Judge runs for real in CI: ProviderJudge builds the
// provider request, sends it over HTTP to the fixture mock, and reads the
// one-word answer back. The rules alone let the paraphrase through (that limit
// is pinned in internal/screen); the judge's fixture answer blocks it.
func TestProviderJudgeAgainstFixtures(t *testing.T) {
	mock, fx := fixtureMock(t)
	srv := &server.Server{
		Providers: provider.Config{
			Providers: map[string]provider.ProviderCfg{"openrouter": {Kind: provider.KindOpenAICompatible, BaseURL: mock.URL + "/v1"}},
			Routes:    []provider.Route{{Prefix: "", Provider: "openrouter"}},
		},
		Keys:   func(string) string { return "mock" },
		Client: http.DefaultClient,
	}
	sc := &screen.Screener{Judge: &server.ProviderJudge{S: srv, Model: "mock/judge-tiny"}}
	ctx := context.Background()
	text := func(id string) screen.ToolResult {
		return screen.ToolResult{Server: "docs", Tool: "fetch", Texts: []string{fx.ByID(id).Match.Prompt}}
	}

	if v := (&screen.Screener{}).Screen(ctx, text("judge-flags-paraphrase")); v.Blocked {
		t.Fatalf("the rules already block this text (%s): pick a paraphrase they miss", v.Rule)
	}
	if v := sc.Screen(ctx, text("judge-flags-paraphrase")); !v.Blocked || v.Rule != screen.RuleJudgeFlagged {
		t.Fatalf("paraphrase: %+v, want blocked by the judge", v)
	}
	if v := sc.Screen(ctx, text("judge-passes-plain")); v.Blocked {
		t.Fatalf("plain data was blocked: %+v", v)
	}
	if n := mock.CallCount(); n != 2 {
		t.Fatalf("%d judge calls reached the mock, want 2", n)
	}

	// Text nobody recorded: the mock answers 500, the judge errors, and the
	// default is to block (fail closed), not to wave the text through.
	v := sc.Screen(ctx, screen.ToolResult{Texts: []string{"a tool result with no fixture"}})
	if !v.Blocked || v.Rule != screen.RuleJudgeUnavailable {
		t.Fatalf("unmatched text: %+v, want judge_unavailable", v)
	}
	if um := mock.Unmatched(); len(um) != 1 || um[0].Hash != fakeupstream.PromptHash("mock/judge-tiny", "a tool result with no fixture") {
		t.Fatalf("unmatched %+v", um)
	}
}
