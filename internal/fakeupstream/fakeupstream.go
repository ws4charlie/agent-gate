// Package fakeupstream is a scriptable stand-in for OpenAI, OpenRouter and
// Anthropic. Every test and demo in this repo talks to it instead of a real
// provider (hard rule 4): no network, no keys, no spend.
package fakeupstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Script says how one request is answered. The zero value is a fast, healthy
// two-chunk reply with usage 10 in / 5 out.
type Script struct {
	Status     int           // non-zero and != 200: answer with this error (429, 500, ...)
	RetryAfter string        // Retry-After header on an error
	Latency    time.Duration // before the first byte
	ChunkDelay time.Duration // between stream events

	Text   []string // text chunks; default "Hello", " world"
	Chunks int      // if > 0, send this many filler chunks of ChunkBytes (slow-client tests)
	// ChunkBytes is the size of each filler chunk.
	ChunkBytes int

	ToolName, ToolArgs string // adds a tool call after the text

	InputTokens, OutputTokens      int64
	CachedTokens, CacheWriteTokens int64
	CostUSD                        string        // reported cost as a JSON number, e.g. "0.000123"; empty: none
	NoUsage                        bool          // stream carries no usage at all
	DieAfter                       int           // abort the connection after this many stream events (0: never)
	Gate                           chan struct{} // if set, wait for it after the first event
	NoGeneration                   bool          // do not record the call for /generation lookups
}

// Call is one recorded request.
type Call struct {
	Path   string
	Header http.Header
	Body   []byte
}

type Server struct {
	*httptest.Server
	Key string // if set, requests must carry it as Bearer or x-api-key

	mu       sync.Mutex
	def      Script
	queue    []Script
	calls    []Call
	gens     map[string]gen
	n        int
	GenDelay time.Duration // how long a finished call stays invisible to /generation
}

type gen struct {
	usage   Script
	in, out int64
	visible time.Time
	partial bool
}

func New() *Server {
	s := &Server{gens: map[string]gen{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, false) })
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, true) })
	mux.HandleFunc("GET /v1/generation", s.generation)
	s.Server = httptest.NewServer(mux)
	return s
}

// SetDefault sets the script used when the queue is empty.
func (s *Server) SetDefault(sc Script) { s.mu.Lock(); s.def = sc; s.mu.Unlock() }

// Push queues a script for the next request.
func (s *Server) Push(sc Script) { s.mu.Lock(); s.queue = append(s.queue, sc); s.mu.Unlock() }

func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

func (s *Server) CallCount() int { return len(s.Calls()) }

// variants are built-in scripts picked by a model-name suffix when no script is
// queued, so a demo can steer the fake from the client side ("fake-chat-die").
func variant(model string, base Script) Script {
	switch {
	case strings.HasSuffix(model, "-die"):
		base.DieAfter, base.Text = 3, []string{"one ", "two ", "three ", "four ", "five"}
	case strings.HasSuffix(model, "-nousage"):
		base.NoUsage = true
	case strings.HasSuffix(model, "-429"):
		base.Status, base.RetryAfter = 429, "1"
	case strings.HasSuffix(model, "-500"):
		base.Status = 500
	case strings.HasSuffix(model, "-cache"):
		base.InputTokens, base.OutputTokens, base.CachedTokens, base.CacheWriteTokens = 100, 50, 2000, 500
	}
	return base
}

func (s *Server) next(model string) (Script, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if len(s.queue) > 0 {
		sc := s.queue[0]
		s.queue = s.queue[1:]
		return sc, s.n
	}
	return variant(model, s.def), s.n
}

func (sc Script) texts() []string {
	if sc.Chunks > 0 {
		out := make([]string, sc.Chunks)
		for i := range out {
			out[i] = strings.Repeat("x", max(sc.ChunkBytes, 1))
		}
		return out
	}
	if len(sc.Text) > 0 {
		return sc.Text
	}
	return []string{"Hello", " world"}
}

func (sc Script) tokens() (in, out int64) {
	in, out = sc.InputTokens, sc.OutputTokens
	if in == 0 {
		in = 10
	}
	if out == 0 {
		out = 5
	}
	return
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, anthropic bool) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	sc, n := s.next(req.Model)
	s.mu.Lock()
	s.calls = append(s.calls, Call{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	s.mu.Unlock()

	if s.Key != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != s.Key && r.Header.Get("X-Api-Key") != s.Key {
			http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
			return
		}
	}
	if sc.Latency > 0 {
		select {
		case <-time.After(sc.Latency):
		case <-r.Context().Done():
			return
		}
	}
	// Echo a secret-looking header so tests can prove the gateway never relays it (I9).
	w.Header().Set("Set-Cookie", "upstream_session=secret")
	w.Header().Set("X-Upstream-Auth", "Bearer upstream-secret")
	if sc.Status != 0 && sc.Status != 200 {
		if sc.RetryAfter != "" {
			w.Header().Set("Retry-After", sc.RetryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(sc.Status)
		fmt.Fprintf(w, `{"error":{"message":"scripted %d"}}`, sc.Status)
		return
	}

	id := "gen-" + strconv.Itoa(n)
	if anthropic {
		id = "msg_" + strconv.Itoa(n)
	}
	in, out := sc.tokens()
	record := func(sent int64, partial bool) {
		if sc.NoGeneration {
			return
		}
		s.mu.Lock()
		s.gens[id] = gen{usage: sc, in: in, out: sent, visible: time.Now().Add(s.GenDelay), partial: partial}
		s.mu.Unlock()
	}

	if !req.Stream {
		record(out, false)
		w.Header().Set("Content-Type", "application/json")
		var b []byte
		if anthropic {
			b = sc.anthropicBody(id, req.Model)
		} else {
			b = sc.openAIBody(id, req.Model)
		}
		_, _ = w.Write(b)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	events := sc.events(anthropic, id, req.Model)
	for i, ev := range events {
		if sc.DieAfter > 0 && i == sc.DieAfter {
			record(int64(i), true)
			panic(http.ErrAbortHandler) // drops the TCP connection mid-chunk
		}
		_, _ = io.WriteString(w, ev)
		fl.Flush()
		if i == 0 && sc.Gate != nil {
			select {
			case <-sc.Gate:
			case <-r.Context().Done():
				record(0, true)
				return
			}
		}
		if sc.ChunkDelay > 0 && i < len(events)-1 {
			select {
			case <-time.After(sc.ChunkDelay):
			case <-r.Context().Done():
				record(int64(i), true)
				return
			}
		}
		if r.Context().Err() != nil {
			record(int64(i), true)
			return
		}
	}
	record(out, false)
}

// generation mimics OpenRouter's GET /generation?id=. A call is visible only
// after GenDelay, like the real endpoint's indexing lag.
func (s *Server) generation(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	g, ok := s.gens[r.URL.Query().Get("id")]
	s.mu.Unlock()
	if !ok || time.Now().Before(g.visible) {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	cost := g.usage.CostUSD
	if cost == "" {
		cost = "0"
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"data":{"id":%q,"total_cost":%s,"tokens_prompt":%d,"tokens_completion":%d}}`,
		r.URL.Query().Get("id"), cost, g.in, g.out)
}

// ---- body builders ----

func (sc Script) usageOpenAI() string {
	in, out := sc.tokens()
	cost := ""
	if sc.CostUSD != "" {
		cost = `,"cost":` + sc.CostUSD
	}
	return fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d}%s}`,
		in+sc.CachedTokens+sc.CacheWriteTokens, out, in+out+sc.CachedTokens+sc.CacheWriteTokens, sc.CachedTokens, sc.CacheWriteTokens, cost)
}

func (sc Script) usageAnthropic(withInput bool) string {
	in, out := sc.tokens()
	if withInput {
		return fmt.Sprintf(`{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":1}`,
			in, sc.CacheWriteTokens, sc.CachedTokens)
	}
	cost := ""
	if sc.CostUSD != "" {
		cost = `,"cost":` + sc.CostUSD
	}
	return fmt.Sprintf(`{"output_tokens":%d%s}`, out, cost)
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func (sc Script) openAIBody(id, model string) []byte {
	msg := map[string]any{"role": "assistant", "content": strings.Join(sc.texts(), "")}
	finish := "stop"
	if sc.ToolName != "" {
		msg["tool_calls"] = []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": sc.ToolName, "arguments": sc.ToolArgs}}}
		finish = "tool_calls"
	}
	usage := ""
	if !sc.NoUsage {
		usage = `,"usage":` + sc.usageOpenAI()
	}
	return []byte(fmt.Sprintf(`{"id":%q,"object":"chat.completion","model":%s,"choices":[{"index":0,"message":%s,"finish_reason":%q}]%s}`,
		id, js(model), js(msg), finish, usage))
}

func (sc Script) anthropicBody(id, model string) []byte {
	content := []any{map[string]any{"type": "text", "text": strings.Join(sc.texts(), "")}}
	stop := "end_turn"
	if sc.ToolName != "" {
		var in any = map[string]any{}
		_ = json.Unmarshal([]byte(sc.ToolArgs), &in)
		content = append(content, map[string]any{"type": "tool_use", "id": "toolu_1", "name": sc.ToolName, "input": in})
		stop = "tool_use"
	}
	in, out := sc.tokens()
	usage := ""
	if !sc.NoUsage {
		cost := ""
		if sc.CostUSD != "" {
			cost = `,"cost":` + sc.CostUSD
		}
		usage = fmt.Sprintf(`,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d%s}`,
			in, sc.CacheWriteTokens, sc.CachedTokens, out, cost)
	}
	return []byte(fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","model":%s,"content":%s,"stop_reason":%q%s}`,
		id, js(model), js(content), stop, usage))
}

func sse(event, data string) string {
	if event == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// events returns the stream as complete SSE frames, one per element.
func (sc Script) events(anthropic bool, id, model string) []string {
	var ev []string
	if !anthropic {
		chunk := func(delta any, finish any) string {
			return sse("", js(map[string]any{"id": id, "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}))
		}
		ev = append(ev, chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		for _, t := range sc.texts() {
			ev = append(ev, chunk(map[string]any{"content": t}, nil))
		}
		finish := "stop"
		if sc.ToolName != "" {
			finish = "tool_calls"
			ev = append(ev, chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": sc.ToolName, "arguments": ""}}}}, nil))
			ev = append(ev, chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0,
				"function": map[string]any{"arguments": sc.ToolArgs}}}}, nil))
		}
		ev = append(ev, chunk(map[string]any{}, finish))
		if !sc.NoUsage {
			ev = append(ev, sse("", fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","model":%s,"choices":[],"usage":%s}`, id, js(model), sc.usageOpenAI())))
		}
		return append(ev, sse("", "[DONE]"))
	}
	start := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{}}
	startRaw := js(map[string]any{"type": "message_start", "message": start})
	if !sc.NoUsage {
		startRaw = strings.Replace(startRaw, `"content":[]`, `"content":[],"usage":`+sc.usageAnthropic(true), 1)
	}
	ev = append(ev, sse("message_start", startRaw))
	ev = append(ev, sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	for _, t := range sc.texts() {
		ev = append(ev, sse("content_block_delta", js(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": t}})))
	}
	ev = append(ev, sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	stop := "end_turn"
	if sc.ToolName != "" {
		stop = "tool_use"
		ev = append(ev, sse("content_block_start", js(map[string]any{"type": "content_block_start", "index": 1,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": sc.ToolName, "input": map[string]any{}}})))
		ev = append(ev, sse("content_block_delta", js(map[string]any{"type": "content_block_delta", "index": 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": sc.ToolArgs}})))
		ev = append(ev, sse("content_block_stop", `{"type":"content_block_stop","index":1}`))
	}
	md := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q}`, stop)
	if !sc.NoUsage {
		md += `,"usage":` + sc.usageAnthropic(false)
	}
	ev = append(ev, sse("message_delta", md+"}"))
	return append(ev, sse("message_stop", `{"type":"message_stop"}`))
}
