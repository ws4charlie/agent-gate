package provider

import (
	"bytes"
	"encoding/json"
	"math/big"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// Format is a wire format. The client's format picks the endpoint; the
// upstream's format picks the usage parser (they differ when we translate).
type Format string

const (
	FormatOpenAI    Format = "openai"    // /v1/chat/completions
	FormatAnthropic Format = "anthropic" // /v1/messages
)

// Parser reads usage out of an upstream response without owning it: feed it
// the same bytes the client gets (it is an io.Writer for io.TeeReader / MultiWriter).
// It understands SSE streams; for a plain JSON body use ParseBody.
type Parser struct {
	format Format
	buf    []byte
	u      Usage
	seen   bool
	// OutChars counts generated characters (text, tool-argument JSON). It feeds
	// the token estimate when the upstream sends no usage (I8).
	OutChars int

	terminal bool // saw [DONE] (OpenAI format) or message_stop (Anthropic)
	inband   bool // saw an in-band error
	pinned   bool
}

const maxLine = 1 << 20 // a line longer than this is not an SSE event we care about

func NewParser(f Format) *Parser { return &Parser{format: f} }

// Write splits input into lines and handles each "data:" line. It never fails:
// a parser error must not break the client's stream.
func (p *Parser) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(p.buf[:i])
		p.buf = p.buf[i+1:]
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			p.object(bytes.TrimSpace(rest))
		}
	}
	if len(p.buf) > maxLine {
		p.buf = nil
	}
	return len(b), nil
}

// Failed reports a stream that did not end cleanly: an in-band error arrived,
// or the bytes stopped before the format's terminal event ([DONE] for OpenAI
// format, message_stop for Anthropic). A clean TCP close alone proves nothing (I8).
func (p *Parser) Failed() bool { return p.inband || !p.terminal }

// SetUpstreamID pins the id (from a response header); body ids no longer replace it.
func (p *Parser) SetUpstreamID(id string) { p.u.UpstreamID, p.pinned = id, id != "" }

// Usage returns what was seen; ok is false when no usage object ever arrived.
func (p *Parser) Usage() (Usage, bool) { return p.u, p.seen }

// ParseBody parses a non-streaming JSON response body.
func ParseBody(f Format, body []byte) (Usage, int, bool) {
	p := NewParser(f)
	p.object(bytes.TrimSpace(body))
	return p.u, p.OutChars, p.seen
}

// ---- JSON shapes ----

type openAIUsage struct {
	PromptTokens     int64       `json:"prompt_tokens"`
	CompletionTokens int64       `json:"completion_tokens"`
	Cost             json.Number `json:"cost"`
	Details          struct {
		Cached     int64 `json:"cached_tokens"`
		CacheWrite int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
}

type anthropicUsage struct {
	Input      int64       `json:"input_tokens"`
	Output     int64       `json:"output_tokens"`
	CacheRead  int64       `json:"cache_read_input_tokens"`
	CacheWrite int64       `json:"cache_creation_input_tokens"`
	Cost       json.Number `json:"cost"`
}

type event struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Usage   json.RawMessage `json:"usage"`
	Message *struct {
		ID      string          `json:"id"`
		Usage   json.RawMessage `json:"usage"`
		Content []contentPart   `json:"content"`
	} `json:"message"`
	Delta   json.RawMessage `json:"delta"`
	Content []contentPart   `json:"content"` // anthropic non-stream
	Error   json.RawMessage `json:"error"`
	Choices []struct {
		FinishReason *string                    `json:"finish_reason"`
		Delta        *struct{ Content *string } `json:"delta"`
		Message      *struct{ Content *string } `json:"message"`
	} `json:"choices"`
}

type contentPart struct {
	Text      string          `json:"text"`
	Input     json.RawMessage `json:"input"`
	PartialJS string          `json:"partial_json"`
}

func (p *Parser) object(raw []byte) {
	if string(raw) == "[DONE]" {
		p.terminal = true
		return
	}
	if len(raw) == 0 || raw[0] != '{' {
		return // comments, blanks
	}
	var e event
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	if len(e.Error) > 0 && string(e.Error) != "null" || e.Type == "error" {
		p.inband = true
	}
	if e.Type == "message_stop" {
		p.terminal = true
	}
	for _, c := range e.Choices {
		if c.FinishReason != nil && *c.FinishReason == "error" {
			p.inband = true // OpenRouter reports mid-stream failures this way
		}
	}
	if e.ID != "" && p.u.UpstreamID == "" && !p.pinned {
		p.u.UpstreamID = e.ID
	}
	switch p.format {
	case FormatOpenAI:
		if len(e.Usage) > 0 && string(e.Usage) != "null" {
			var u openAIUsage
			if json.Unmarshal(e.Usage, &u) == nil {
				p.setOpenAI(u)
			}
		}
		for _, c := range e.Choices {
			if c.Delta != nil && c.Delta.Content != nil {
				p.OutChars += len(*c.Delta.Content)
			}
			if c.Message != nil && c.Message.Content != nil {
				p.OutChars += len(*c.Message.Content)
			}
		}
	case FormatAnthropic:
		if e.Message != nil {
			if e.Message.ID != "" && !p.pinned {
				p.u.UpstreamID = e.Message.ID
			}
			p.mergeAnthropic(e.Message.Usage)
		}
		p.mergeAnthropic(e.Usage)
		for _, c := range e.Content {
			p.OutChars += len(c.Text) + len(c.Input)
		}
		if e.Type == "content_block_delta" && len(e.Delta) > 0 {
			var d contentPart
			if json.Unmarshal(e.Delta, &d) == nil {
				p.OutChars += len(d.Text) + len(d.PartialJS)
			}
		}
	}
}

func (p *Parser) setOpenAI(u openAIUsage) {
	p.seen = true
	// OpenAI's prompt_tokens includes cached tokens; ours is the non-cached
	// part so each class is priced once.
	in := u.PromptTokens - u.Details.Cached - u.Details.CacheWrite
	if in < 0 {
		in = 0
	}
	p.u.InputTokens, p.u.OutputTokens = in, u.CompletionTokens
	p.u.CacheReadTokens, p.u.CacheWriteTokens = u.Details.Cached, u.Details.CacheWrite
	if c, ok := costNanos(u.Cost); ok {
		p.u.CostNanos = &c
	}
}

// mergeAnthropic folds one usage object in. message_start carries input and
// cache counts; message_delta carries the cumulative output count. Later
// non-zero values win, so a delta that omits a field cannot erase it.
func (p *Parser) mergeAnthropic(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var u anthropicUsage
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	p.seen = true
	set := func(dst *int64, v int64) {
		if v != 0 {
			*dst = v
		}
	}
	set(&p.u.InputTokens, u.Input)
	set(&p.u.OutputTokens, u.Output)
	set(&p.u.CacheReadTokens, u.CacheRead)
	set(&p.u.CacheWriteTokens, u.CacheWrite)
	if c, ok := costNanos(u.Cost); ok {
		p.u.CostNanos = &c
	}
}

// costNanos converts a provider-reported USD cost (a JSON number such as
// 0.0000123 or 1.5e-05) to Nanos exactly, rounding up. big.Rat, not float64.
func costNanos(n json.Number) (money.Nanos, bool) {
	if n == "" {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(n.String())
	if !ok || r.Sign() < 0 {
		return 0, false
	}
	r.Mul(r, big.NewRat(1_000_000_000, 1))
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, false
	}
	return money.Nanos(q.Int64()), true
}
