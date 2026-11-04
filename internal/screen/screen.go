// Package screen looks at a tool's output before it re-enters the agent's
// context. A tool result is text written by someone else (a web page, a ticket,
// a file), and an agent that reads it may take it for an instruction: that is
// indirect prompt injection.
//
// The screen has two layers. The first is a fixed list of deterministic rules
// (rules.go): instruction-override phrases, role markers, hidden Unicode,
// encoded payloads that decode to one of those, and exfiltration URLs. It is
// cheap, explainable and testable against a corpus. The second is an optional
// Judge, a small model asked "does this text try to instruct its reader?",
// which is off unless the operator wires one.
//
// What this is not: a proof. Rules catch the phrasings they list. A paraphrase,
// another language, or an instruction that reads like ordinary prose passes
// (testdata/evasions holds examples, and a test pins that they pass, so the
// limit is written down rather than hoped away). Screening lowers the odds of
// the cheap attacks; the hard limits against a hijacked agent stay scope,
// budget and approval (internal/policy).
//
// A hit blocks the content. What is logged is the rule id and a SHA-256 of
// the content, never the content (I9).
package screen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ToolResult is the text of one tool output, with where it came from.
type ToolResult struct {
	Server, Tool string
	Texts        []string // every string the result carries, see ExtractTexts
}

// Verdict is the outcome. Hash identifies the content without containing it.
type Verdict struct {
	Blocked bool
	Rule    string // rule id when Blocked
	Detail  string // short reason, never a quote of the content
	Hash    string // SHA-256 hex of the screened text
}

// Judge is the optional second opinion. It returns whether the text looks like
// an attempt to instruct its reader. An error means "no opinion available".
type Judge interface {
	Suspicious(ctx context.Context, r ToolResult) (bool, string, error)
}

// Rule ids that do not come from rules.go.
const (
	RuleJudgeFlagged     = "judge_flagged"
	RuleJudgeUnavailable = "judge_unavailable"
)

// Screener runs the rules and then, if set, the Judge.
type Screener struct {
	Judge Judge // nil: off (the default)
	// JudgeFailOpen lets content through when the Judge errors. The default is
	// to block: an operator who turned the Judge on wants its check, and a
	// check that vanishes when the model is down is not a check.
	JudgeFailOpen bool
}

// Screen decides one tool result. A nil Screener lets everything through, so
// a deployment without screening is explicit (the caller passes nil).
func (s *Screener) Screen(ctx context.Context, r ToolResult) Verdict {
	v := Verdict{Hash: Hash(r.Texts)}
	if s == nil {
		return v
	}
	for _, t := range r.Texts {
		if rule, detail, hit := scanText(t); hit {
			v.Blocked, v.Rule, v.Detail = true, rule, detail
			return v
		}
	}
	if s.Judge != nil {
		sus, why, err := s.Judge.Suspicious(ctx, r)
		switch {
		case err != nil && !s.JudgeFailOpen:
			v.Blocked, v.Rule, v.Detail = true, RuleJudgeUnavailable, "judge error"
		case err == nil && sus:
			v.Blocked, v.Rule, v.Detail = true, RuleJudgeFlagged, why
		}
	}
	return v
}

// Hash is the SHA-256 of the texts, separated so ["ab","c"] and ["a","bc"]
// differ.
func Hash(texts []string) string {
	h := sha256.New()
	for _, t := range texts {
		h.Write([]byte(t))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ExtractTexts collects the strings a JSON value carries, so that nothing in a
// result escapes the screen because it sits in an unexpected field. It skips
// protocol bookkeeping and the binary payload of image and audio blocks
// (base64 of a PNG is not an instruction; an image's *text* cannot be read here,
// which is a stated limit).
func ExtractTexts(raw json.RawMessage) []string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return []string{string(raw)} // unparseable: screen it as text
	}
	var out []string
	walk(v, &out)
	return out
}

func walk(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) != "" {
			*out = append(*out, x)
		}
	case []any:
		for _, e := range x {
			walk(e, out)
		}
	case map[string]any:
		typ, _ := x["type"].(string)
		binary := typ == "image" || typ == "audio"
		for k, e := range x {
			switch k {
			case "type", "resultType", "mimeType", "isError":
				continue
			case "data":
				if binary {
					continue
				}
			case "blob":
				continue
			}
			walk(e, out)
		}
	}
}
