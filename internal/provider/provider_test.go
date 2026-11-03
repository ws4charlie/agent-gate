package provider

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAIUsageStreamSplitAcrossWrites(t *testing.T) {
	stream := "data: {\"id\":\"gen-1\",\"choices\":[{\"delta\":{\"content\":\"hey\"}}],\"usage\":null}\n\n" +
		"data: {\"id\":\"gen-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":50," +
		"\"prompt_tokens_details\":{\"cached_tokens\":600,\"cache_write_tokens\":100},\"cost\":1.5e-05}}\n\ndata: [DONE]\n\n"
	p := NewParser(FormatOpenAI)
	for i := 0; i < len(stream); i += 7 { // awkward chunking: lines straddle writes
		p.Write([]byte(stream[i:min(i+7, len(stream))]))
	}
	u, ok := p.Usage()
	if !ok || u.UpstreamID != "gen-1" || u.InputTokens != 300 || u.OutputTokens != 50 || u.CacheReadTokens != 600 || u.CacheWriteTokens != 100 {
		t.Fatalf("%+v %v", u, ok)
	}
	if u.CostNanos == nil || *u.CostNanos != 15_000 { // $0.000015 = 15000 Nanos
		t.Fatalf("cost %v", u.CostNanos)
	}
	if p.OutChars != 3 {
		t.Fatalf("outchars %d", p.OutChars)
	}
}

func TestAnthropicUsageMergesStartAndDelta(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":12,\"cache_creation_input_tokens\":5,\"cache_read_input_tokens\":30,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"abcd\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":77}}\n\n"
	p := NewParser(FormatAnthropic)
	p.Write([]byte(stream))
	u, ok := p.Usage()
	if !ok || u.UpstreamID != "msg_1" || u.InputTokens != 12 || u.OutputTokens != 77 || u.CacheReadTokens != 30 || u.CacheWriteTokens != 5 || u.CostNanos != nil {
		t.Fatalf("%+v %v", u, ok)
	}
}

func TestParseBodyNoUsage(t *testing.T) {
	_, n, ok := ParseBody(FormatOpenAI, []byte(`{"id":"x","choices":[{"message":{"content":"hello"}}]}`))
	if ok || n != 5 {
		t.Fatalf("ok=%v n=%d", ok, n)
	}
}

func TestResolveAndBuild(t *testing.T) {
	c := Config{
		Providers: map[string]ProviderCfg{
			"or": {Kind: KindOpenAICompatible, BaseURL: "https://openrouter.test/api/v1/", IncludeUsage: true},
			"an": {Kind: KindAnthropic, BaseURL: "https://anthropic.test/v1"},
		},
		Routes: []Route{{Prefix: "claude-", Provider: "an"}, {Prefix: "", Provider: "or"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	tg, _ := c.Resolve("claude-x")
	if tg.Name != "an" || tg.Model != "claude-x" {
		t.Fatalf("%+v", tg)
	}
	tg, _ = c.Resolve("openai/gpt-y")
	hdr := http.Header{"Authorization": {"Bearer client-secret"}, "X-Api-Key": {"client-secret"}, "Anthropic-Beta": {"b"}}
	up, err := Build(tg, "UPKEY", FormatOpenAI, hdr, []byte(`{"model":"openai/gpt-y","stream":true,"extra":{"k":1}}`), true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if up.URL != "https://openrouter.test/api/v1/chat/completions" || up.Header.Get("Authorization") != "Bearer UPKEY" || up.Header.Get("X-Api-Key") != "" {
		t.Fatalf("%s %v", up.URL, up.Header)
	}
	if !strings.Contains(string(up.Body), `"include_usage":true`) || !strings.Contains(string(up.Body), `"extra":{"k":1}`) {
		t.Fatalf("%s", up.Body)
	}
	// same model, nothing to add: the body goes through byte for byte
	raw := []byte(`{ "model" : "openai/gpt-y",  "x":1 }`)
	up, _ = Build(tg, "K", FormatAnthropic, hdr, raw, false, 100)
	if !bytes.Equal(up.Body, raw) || up.URL != "https://openrouter.test/api/v1/messages" {
		t.Fatalf("%s %s", up.URL, up.Body)
	}
}

func TestChatToMessagesToolRoundTrip(t *testing.T) {
	in := `{"model":"x","stop":"END","temperature":0.5,"messages":[
	 {"role":"system","content":"S1"},{"role":"developer","content":"S2"},
	 {"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
	 {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
	 {"role":"tool","tool_call_id":"c1","content":"result"},
	 {"role":"user","content":"thanks"}],
	 "tool_choice":{"type":"function","function":{"name":"f"}}}`
	out, err := ChatToMessages([]byte(in), "claude-y", 321)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Model     string           `json:"model"`
		MaxTokens int              `json:"max_tokens"`
		System    string           `json:"system"`
		Stop      []string         `json:"stop_sequences"`
		Messages  []map[string]any `json:"messages"`
		Choice    map[string]any   `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m.Model != "claude-y" || m.MaxTokens != 321 || m.System != "S1\n\nS2" || m.Stop[0] != "END" || m.Choice["name"] != "f" {
		t.Fatalf("%s", out)
	}
	// user, assistant(tool_use), user(tool_result + text merged): three messages
	if len(m.Messages) != 3 {
		t.Fatalf("messages %d: %s", len(m.Messages), out)
	}
	last := m.Messages[2]["content"].([]any)
	if last[0].(map[string]any)["type"] != "tool_result" || last[1].(map[string]any)["text"] != "thanks" {
		t.Fatalf("%v", last)
	}
	img := m.Messages[0]["content"].([]any)[1].(map[string]any)["source"].(map[string]any)
	if img["type"] != "base64" || img["media_type"] != "image/png" || img["data"] != "AAAA" {
		t.Fatalf("%v", img)
	}
}

func TestSSETranslatorTextStream(t *testing.T) {
	tr := NewSSETranslator("m")
	var out strings.Builder
	for _, ev := range []string{
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":4,"cache_read_input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":6}}`,
		`{"type":"message_stop"}`,
	} {
		for _, f := range tr.Event([]byte(ev)) {
			out.Write(f)
		}
	}
	s := out.String()
	for _, want := range []string{`"id":"msg_1"`, `"content":"Hi"`, `"finish_reason":"length"`, `"prompt_tokens":14`, `"completion_tokens":6`, `"cached_tokens":10`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}
