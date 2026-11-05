package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Upstream is a ready-to-send request.
type Upstream struct {
	URL       string
	Header    http.Header
	Body      []byte
	Wire      Format // format the upstream speaks (picks the usage parser)
	Translate bool   // response must be converted back to chat-completions
}

// Headers copied from the client. Authorization and x-api-key are NOT here: the
// client's gateway credential must never reach a provider (I9). anthropic-*
// pass unchanged so prompt caching and thinking betas keep working.
var forwardRequest = []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta", "Http-Referer", "X-Title"}

// Headers copied back to the client. An allowlist, so a provider's auth or
// cookie headers can never leak through (I9).
var forwardResponse = []string{"Content-Type", "Cache-Control", "Retry-After"}

func CopyResponseHeaders(dst, src http.Header) {
	for _, h := range forwardResponse {
		if v := src.Values(h); len(v) > 0 {
			dst[h] = append([]string(nil), v...)
		}
	}
}

// Build makes the upstream request for a client request in format client.
//
//	client openai    + openai-compatible: passthrough to /chat/completions
//	client openai    + anthropic:         translate to /messages
//	client anthropic + openai-compatible: passthrough to /messages (OpenRouter's Anthropic surface)
//	client anthropic + anthropic:         passthrough to /messages
func Build(t Target, key string, client Format, in http.Header, body []byte, stream bool, defaultMax int64) (Upstream, error) {
	base := strings.TrimRight(t.Cfg.BaseURL, "/")
	up := Upstream{Header: http.Header{}}
	for _, h := range forwardRequest {
		if v := in.Values(h); len(v) > 0 {
			up.Header[h] = append([]string(nil), v...)
		}
	}
	up.Header.Set("Content-Type", "application/json")

	var err error
	switch {
	case client == FormatOpenAI && t.Cfg.Kind == KindOpenAICompatible:
		up.URL, up.Wire = base+"/chat/completions", FormatOpenAI
		up.Body, err = rewrite(body, t.Model, func(m map[string]json.RawMessage) bool {
			changed := limitOutput(m, defaultMax)
			if stream && t.Cfg.IncludeUsage {
				if _, ok := m["stream_options"]; !ok {
					m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
					changed = true
				}
			}
			return changed
		})
		up.Header.Set("Authorization", "Bearer "+key)
	case client == FormatOpenAI && t.Cfg.Kind == KindAnthropic:
		up.URL, up.Wire, up.Translate = base+"/messages", FormatAnthropic, true
		up.Body, err = ChatToMessages(body, t.Model, defaultMax)
		anthropicAuth(up.Header, key)
	case client == FormatAnthropic && t.Cfg.Kind == KindOpenAICompatible:
		up.URL, up.Wire = base+"/messages", FormatAnthropic
		up.Body, err = rewrite(body, t.Model, func(m map[string]json.RawMessage) bool { return limitOutput(m, defaultMax) })
		up.Header.Set("Authorization", "Bearer "+key)
	case client == FormatAnthropic && t.Cfg.Kind == KindAnthropic:
		up.URL, up.Wire = base+"/messages", FormatAnthropic
		up.Body, err = rewrite(body, t.Model, func(m map[string]json.RawMessage) bool { return limitOutput(m, defaultMax) })
		anthropicAuth(up.Header, key)
	default:
		err = fmt.Errorf("provider: no adapter for %s client on %s", client, t.Cfg.Kind)
	}
	return up, err
}

func anthropicAuth(h http.Header, key string) {
	h.Set("X-Api-Key", key)
	if h.Get("Anthropic-Version") == "" {
		h.Set("Anthropic-Version", "2023-06-01")
	}
}

// limitOutput adds the output limit the hold assumed when the client set none.
// Without it the upstream could produce more than the gateway estimated, and
// one request could pass the agent's budget.
func limitOutput(m map[string]json.RawMessage, limit int64) bool {
	positive := func(key string) bool {
		var n int64
		return json.Unmarshal(m[key], &n) == nil && n > 0
	}
	if positive("max_tokens") || positive("max_completion_tokens") {
		return false
	}
	m["max_tokens"], _ = json.Marshal(limit)
	return true
}

// rewrite sets "model" (and applies edit, which reports whether it changed
// anything) while keeping every other field as the client sent it. If nothing
// changes the original bytes go through.
func rewrite(body []byte, model string, edit func(map[string]json.RawMessage) bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("provider: request body: %w", err)
	}
	var cur string
	_ = json.Unmarshal(m["model"], &cur)
	if !edit(m) && cur == model {
		return body, nil
	}
	mb, _ := json.Marshal(model)
	m["model"] = mb
	return json.Marshal(m)
}
