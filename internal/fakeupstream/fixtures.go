package fakeupstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// A Fixture is one recorded-style exchange: for this model and this last
// message, the provider answers like this and bills this much. They are written
// by hand (testdata/llm); nothing comes from real traffic.
//
// Replay is at the HTTP boundary: the gateway's routing, SSE passthrough and
// usage parsing all run. Fixtures say nothing about how a real provider
// behaves; they prove the gateway's accounting given a provider that answers
// this way.
type Fixture struct {
	ID    string `json:"id"`
	Match struct {
		Model  string `json:"model"`  // the model name as the upstream sees it (after the route strips its prefix)
		Prompt string `json:"prompt"` // text of the last message: the last user turn, or the tool result that follows a tool call
	} `json:"match"`
	Response struct {
		Text      string `json:"text"`
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"` // a JSON object
		} `json:"tool_calls"`
	} `json:"response"`
	Usage struct {
		Input      int64 `json:"input"` // non-cached input tokens
		Output     int64 `json:"output"`
		CacheRead  int64 `json:"cache_read"`
		CacheWrite int64 `json:"cache_write"`
		// CostInline false: the provider reports no cost in the response (like
		// direct Anthropic), so the gateway prices from tokens. /generation
		// still returns Cost. Default true (OpenRouter sends usage.cost).
		CostInline *bool `json:"cost_inline"`
	} `json:"usage"`
	Cost       json.Number `json:"cost"` // USD, as the provider bills it
	UpstreamID string      `json:"upstream_id"`
}

// Fixtures is a loaded directory of fixtures.
type Fixtures struct {
	byKey map[string]*Fixture
	all   []*Fixture
}

// PromptHash is the 12-hex-char name an unmatched request is reported under.
func PromptHash(model, prompt string) string {
	h := sha256.Sum256([]byte(model + "\x00" + strings.TrimSpace(prompt)))
	return hex.EncodeToString(h[:])[:12]
}

// LoadFixtures reads every *.json in dir. A malformed fixture, a duplicate
// match or id, or a missing field is an error, so a typo fails at startup.
func LoadFixtures(dir string) (*Fixtures, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	fx := &Fixtures{byKey: map[string]*Fixture{}}
	ids := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var x Fixture
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&x); err != nil {
			return nil, fmt.Errorf("fixture %s: %w", f, err)
		}
		switch {
		case x.ID == "" || x.Match.Model == "" || strings.TrimSpace(x.Match.Prompt) == "":
			return nil, fmt.Errorf("fixture %s: id, match.model and match.prompt are required", f)
		case x.Response.Text == "" && len(x.Response.ToolCalls) == 0:
			return nil, fmt.Errorf("fixture %s: response needs text or tool_calls", f)
		case len(x.Response.ToolCalls) > 1:
			return nil, fmt.Errorf("fixture %s: at most one tool call per fixture", f)
		case x.Usage.Input <= 0 || x.Usage.Output <= 0:
			return nil, fmt.Errorf("fixture %s: usage.input and usage.output must be positive", f)
		case !validCost(x.Cost):
			return nil, fmt.Errorf("fixture %s: cost must be a non-negative USD number", f)
		case !strings.HasPrefix(x.UpstreamID, "gen-"):
			return nil, fmt.Errorf("fixture %s: upstream_id must start with gen- (the /generation endpoint's id shape)", f)
		case ids[x.ID]:
			return nil, fmt.Errorf("fixture %s: duplicate id %s", f, x.ID)
		}
		key := x.Match.Model + "\x00" + strings.TrimSpace(x.Match.Prompt)
		if prev, dup := fx.byKey[key]; dup {
			return nil, fmt.Errorf("fixture %s: same model and prompt as %s", x.ID, prev.ID)
		}
		ids[x.ID] = true
		fx.byKey[key] = &x
		fx.all = append(fx.all, &x)
	}
	if len(fx.all) == 0 {
		return nil, fmt.Errorf("no fixtures in %s", dir)
	}
	return fx, nil
}

// All returns the fixtures in file-name order.
func (f *Fixtures) All() []*Fixture { return f.all }

// ByID finds a fixture, or nil.
func (f *Fixtures) ByID(id string) *Fixture {
	for _, x := range f.all {
		if x.ID == id {
			return x
		}
	}
	return nil
}

// script turns the matching fixture into the Script the wire builders speak.
func (f *Fixtures) script(model string, body []byte) (Script, *Fixture, string, bool) {
	prompt := lastMessageText(body)
	x := f.byKey[model+"\x00"+prompt]
	if x == nil {
		return Script{}, nil, prompt, false
	}
	sc := Script{
		InputTokens: x.Usage.Input, OutputTokens: x.Usage.Output,
		CachedTokens: x.Usage.CacheRead, CacheWriteTokens: x.Usage.CacheWrite,
		GenCostUSD: string(x.Cost), ID: x.UpstreamID,
		Text: words(x.Response.Text),
	}
	if x.Usage.CostInline == nil || *x.Usage.CostInline {
		sc.CostUSD = string(x.Cost)
	}
	if len(sc.Text) == 0 {
		sc.Text = []string{""}
	}
	if len(x.Response.ToolCalls) == 1 {
		sc.ToolName, sc.ToolArgs = x.Response.ToolCalls[0].Name, string(x.Response.ToolCalls[0].Arguments)
	}
	return sc, x, prompt, true
}

func validCost(c json.Number) bool {
	r, ok := new(big.Rat).SetString(string(c))
	return ok && c != "" && r.Sign() >= 0
}

// words splits text into stream chunks that rejoin to the original.
func words(s string) []string {
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, ' ')
		if i < 0 {
			out, s = append(out, s), ""
			break
		}
		out, s = append(out, s[:i+1]), s[i+1:]
	}
	return out
}

// lastMessageText is what a fixture matches on. It reads either wire format:
// OpenAI chat (content is a string or parts; a tool result is a role "tool"
// message) and Anthropic Messages (content is a string or blocks; a tool result
// is a tool_result block in a user message). The text is trimmed.
func lastMessageText(body []byte) string {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return ""
	}
	return strings.TrimSpace(contentText(req.Messages[len(req.Messages)-1].Content))
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"` // tool_result
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "tool_result":
			b.WriteString(contentText(p.Content))
		}
	}
	return b.String()
}

// Unmatched is what the server saw that no fixture answered; tests assert it is empty.
type Unmatched struct{ Model, Prompt, Hash string }

type unmatchedLog struct {
	mu sync.Mutex
	l  []Unmatched
}

func (u *unmatchedLog) add(m Unmatched) { u.mu.Lock(); u.l = append(u.l, m); u.mu.Unlock() }
func (u *unmatchedLog) all() []Unmatched {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Unmatched(nil), u.l...)
}

// UseFixtures switches the server to fixture replay. Scripts pushed with Push
// still win, so failure scripts keep working beside fixtures.
func (s *Server) UseFixtures(f *Fixtures) { s.mu.Lock(); s.fx = f; s.mu.Unlock() }

// Unmatched lists requests no fixture answered (each got a loud 500).
func (s *Server) Unmatched() []Unmatched { return s.miss.all() }

func unmatchedBody(m Unmatched) string {
	p := m.Prompt
	if len(p) > 80 {
		p = p[:80] + "..."
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": "fixture_not_found", "type": "fixture_not_found",
		"message": fmt.Sprintf("no fixture for model %q and prompt hash %s (prompt starts: %q); add one to testdata/llm", m.Model, m.Hash, p)}})
	return string(b)
}
