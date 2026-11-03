package pricing

import (
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/provider"
)

func TestCostHandComputed(t *testing.T) {
	// $3/M in, $15/M out, $0.30/M cache read, $3.75/M cache write.
	p := Price{Input: 3_000_000_000, Output: 15_000_000_000, CacheRead: 300_000_000, CacheWrite: 3_750_000_000}
	u := provider.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 2000, CacheWriteTokens: 400}
	// 1000*3e9/1e6 = 3_000_000 ; 500*15e9/1e6 = 7_500_000 ; 2000*3e8/1e6 = 600_000 ; 400*3.75e9/1e6 = 1_500_000
	got, err := p.Cost(u)
	if err != nil || got != 12_600_000 {
		t.Fatalf("got %d, %v want 12600000", got, err)
	}
}

func TestLookupFallsBackToInput(t *testing.T) {
	tb := &Table{Models: map[string]Price{"m": {Input: 100, Output: 200}}}
	p, err := tb.Lookup("m")
	if err != nil || p.CacheRead != 100 || p.CacheWrite != 100 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := tb.Lookup("x"); err == nil {
		t.Fatal("want unknown model")
	}
}

func TestParseOpenRouterFixture(t *testing.T) {
	const fixture = `{"data":[
	 {"id":"anthropic/claude-x","pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003","input_cache_write":"0.00000375"}},
	 {"id":"openai/gpt-y","pricing":{"prompt":"0.0000025","completion":"0.00001"}},
	 {"id":"openrouter/auto","pricing":{"prompt":"-1","completion":"-1"}}]}`
	tb, err := ParseOpenRouterModels(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	want := Price{Input: 3_000_000_000, Output: 15_000_000_000, CacheRead: 300_000_000, CacheWrite: 3_750_000_000}
	if tb.Models["anthropic/claude-x"] != want {
		t.Fatalf("got %+v", tb.Models["anthropic/claude-x"])
	}
	if tb.Models["openai/gpt-y"].Input != 2_500_000_000 {
		t.Fatalf("got %+v", tb.Models["openai/gpt-y"])
	}
	if _, ok := tb.Models["openrouter/auto"]; ok {
		t.Fatal("negative price should be skipped")
	}
	_ = money.Nanos(0)
}
