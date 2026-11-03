package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Chat-completions <-> Anthropic Messages. Covers text, images (data/http URLs),
// system prompts, tool definitions, tool calls and tool results, streaming
// included. It does not cover audio, logprobs, n>1 or response_format.

type obj = map[string]any

func ChatToMessages(body []byte, model string, defaultMax int64) ([]byte, error) {
	var in struct {
		Messages            []chatMsg       `json:"messages"`
		MaxTokens           int64           `json:"max_tokens"`
		MaxCompletionTokens int64           `json:"max_completion_tokens"`
		Stream              bool            `json:"stream"`
		Temperature         *float64        `json:"temperature"`
		TopP                *float64        `json:"top_p"`
		Stop                json.RawMessage `json:"stop"`
		User                string          `json:"user"`
		Tools               []struct {
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("provider: chat request: %w", err)
	}
	out := obj{"model": model, "stream": in.Stream}
	maxTok := in.MaxTokens
	if in.MaxCompletionTokens > 0 {
		maxTok = in.MaxCompletionTokens
	}
	if maxTok <= 0 {
		maxTok = defaultMax // Messages requires max_tokens
	}
	out["max_tokens"] = maxTok
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if in.User != "" {
		out["metadata"] = obj{"user_id": in.User}
	}
	if len(in.Stop) > 0 && string(in.Stop) != "null" {
		var one string
		var many []string
		if json.Unmarshal(in.Stop, &one) == nil {
			many = []string{one}
		} else if json.Unmarshal(in.Stop, &many) != nil {
			return nil, fmt.Errorf("provider: bad stop")
		}
		out["stop_sequences"] = many
	}

	var system []string
	var msgs []obj
	push := func(role string, blocks ...obj) {
		if n := len(msgs); n > 0 && msgs[n-1]["role"] == role {
			msgs[n-1]["content"] = append(msgs[n-1]["content"].([]obj), blocks...)
			return
		}
		msgs = append(msgs, obj{"role": role, "content": blocks})
	}
	for _, m := range in.Messages {
		parts, err := m.parts()
		if err != nil {
			return nil, err
		}
		switch m.Role {
		case "system", "developer":
			for _, p := range parts {
				if p["type"] == "text" {
					system = append(system, p["text"].(string))
				}
			}
		case "user":
			push("user", parts...)
		case "assistant":
			blocks := parts
			for _, tc := range m.ToolCalls {
				var input any = obj{}
				if tc.Function.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
						return nil, fmt.Errorf("provider: tool call %s arguments: %w", tc.ID, err)
					}
				}
				blocks = append(blocks, obj{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
			}
			if len(blocks) == 0 {
				blocks = []obj{{"type": "text", "text": ""}}
			}
			push("assistant", blocks...)
		case "tool":
			var text []string
			for _, p := range parts {
				if p["type"] == "text" {
					text = append(text, p["text"].(string))
				}
			}
			push("user", obj{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": strings.Join(text, "")})
		default:
			return nil, fmt.Errorf("provider: unsupported role %q", m.Role)
		}
	}
	out["messages"] = msgs
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	if len(in.Tools) > 0 {
		var tools []obj
		for _, t := range in.Tools {
			var schema any = obj{"type": "object"}
			if len(t.Function.Parameters) > 0 {
				schema = t.Function.Parameters
			}
			tool := obj{"name": t.Function.Name, "input_schema": schema}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			tools = append(tools, tool)
		}
		out["tools"] = tools
	}
	if len(in.ToolChoice) > 0 && string(in.ToolChoice) != "null" {
		var s string
		var o struct {
			Function struct{ Name string } `json:"function"`
		}
		switch {
		case json.Unmarshal(in.ToolChoice, &s) == nil:
			out["tool_choice"] = obj{"type": map[string]string{"auto": "auto", "required": "any", "none": "none"}[s]}
		case json.Unmarshal(in.ToolChoice, &o) == nil && o.Function.Name != "":
			out["tool_choice"] = obj{"type": "tool", "name": o.Function.Name}
		}
	}
	return json.Marshal(out)
}

type chatMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// parts converts content (string or part array) into Messages blocks.
func (m chatMsg) parts() ([]obj, error) {
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []obj{{"type": "text", "text": s}}, nil
	}
	var arr []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(m.Content, &arr); err != nil {
		return nil, fmt.Errorf("provider: message content: %w", err)
	}
	var out []obj
	for _, p := range arr {
		switch p.Type {
		case "text":
			out = append(out, obj{"type": "text", "text": p.Text})
		case "image_url":
			if rest, ok := strings.CutPrefix(p.ImageURL.URL, "data:"); ok {
				meta, data, _ := strings.Cut(rest, ",")
				out = append(out, obj{"type": "image", "source": obj{"type": "base64",
					"media_type": strings.TrimSuffix(meta, ";base64"), "data": data}})
			} else {
				out = append(out, obj{"type": "image", "source": obj{"type": "url", "url": p.ImageURL.URL}})
			}
		default:
			return nil, fmt.Errorf("provider: unsupported content part %q", p.Type)
		}
	}
	return out, nil
}

var finishReason = map[string]string{"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls", "refusal": "content_filter"}

func chatUsage(in, out, cr, cw int64) obj {
	// OpenAI's prompt_tokens counts cached tokens too.
	return obj{"prompt_tokens": in + cr + cw, "completion_tokens": out, "total_tokens": in + cr + cw + out,
		"prompt_tokens_details": obj{"cached_tokens": cr, "cache_write_tokens": cw}}
}

// MessagesToChat converts a non-streaming Messages response.
func MessagesToChat(body []byte, model string) ([]byte, error) {
	var in struct {
		ID         string `json:"id"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("provider: messages response: %w", err)
	}
	var text strings.Builder
	var calls []obj
	for _, c := range in.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			calls = append(calls, obj{"id": c.ID, "type": "function", "index": len(calls),
				"function": obj{"name": c.Name, "arguments": string(c.Input)}})
		}
	}
	msg := obj{"role": "assistant", "content": text.String()}
	if text.Len() == 0 && len(calls) > 0 {
		msg["content"] = nil
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	fr := finishReason[in.StopReason]
	if fr == "" {
		fr = "stop"
	}
	u := in.Usage
	return json.Marshal(obj{"id": in.ID, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []obj{{"index": 0, "message": msg, "finish_reason": fr}},
		"usage":   chatUsage(u.Input, u.Output, u.CacheRead, u.CacheWrite)})
}

// SSETranslator turns Messages stream events into chat.completion.chunk lines.
// Feed it one event at a time; it returns zero or more complete "data: ...\n\n"
// frames to write to the client.
type SSETranslator struct {
	Model string

	id, created      string
	in, out, cr, cw  int64
	tools            map[int]int // content block index -> tool_call index
	started, stopped bool
}

func NewSSETranslator(model string) *SSETranslator {
	return &SSETranslator{Model: model, tools: map[int]int{}}
}

func (t *SSETranslator) chunk(delta obj, finish any, usage obj) []byte {
	c := obj{"id": t.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": t.Model, "choices": []obj{}}
	if delta != nil {
		c["choices"] = []obj{{"index": 0, "delta": delta, "finish_reason": finish}}
	}
	if usage != nil {
		c["usage"] = usage
	}
	b, _ := json.Marshal(c)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

func (t *SSETranslator) Event(data []byte) [][]byte {
	var e struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string         `json:"id"`
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Block struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage anthropicUsage `json:"usage"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return nil
	}
	switch e.Type {
	case "message_start":
		t.id = e.Message.ID
		u := e.Message.Usage
		t.in, t.cr, t.cw, t.out = u.Input, u.CacheRead, u.CacheWrite, u.Output
		t.started = true
		return [][]byte{t.chunk(obj{"role": "assistant", "content": ""}, nil, nil)}
	case "content_block_start":
		if e.Block.Type == "tool_use" {
			n := len(t.tools)
			t.tools[e.Index] = n
			return [][]byte{t.chunk(obj{"tool_calls": []obj{{"index": n, "id": e.Block.ID, "type": "function",
				"function": obj{"name": e.Block.Name, "arguments": ""}}}}, nil, nil)}
		}
	case "content_block_delta":
		switch e.Delta.Type {
		case "text_delta":
			return [][]byte{t.chunk(obj{"content": e.Delta.Text}, nil, nil)}
		case "input_json_delta":
			return [][]byte{t.chunk(obj{"tool_calls": []obj{{"index": t.tools[e.Index],
				"function": obj{"arguments": e.Delta.PartialJSON}}}}, nil, nil)}
		}
	case "message_delta":
		if e.Usage.Output != 0 {
			t.out = e.Usage.Output
		}
		fr := finishReason[e.Delta.StopReason]
		if fr == "" {
			fr = "stop"
		}
		return [][]byte{t.chunk(obj{}, fr, nil)}
	case "message_stop":
		t.stopped = true
		return [][]byte{t.chunk(nil, nil, chatUsage(t.in, t.out, t.cr, t.cw)), []byte("data: [DONE]\n\n")}
	case "error":
		b, _ := json.Marshal(obj{"error": obj{"type": e.Error.Type, "message": e.Error.Message}})
		return [][]byte{append(append([]byte("data: "), b...), '\n', '\n')}
	}
	return nil
}
