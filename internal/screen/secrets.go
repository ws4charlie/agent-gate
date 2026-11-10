package screen

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The screen above reads what comes back from a tool. This file reads what an
// agent sends out: a key or password in a tool call's arguments is on its way
// to a third party, and the gateway is the last place that can stop it.
//
// Detection is by format, as secret scanners do: providers give their keys a
// fixed prefix and shape. It is deterministic and cheap, and it misses
// secrets with no recognisable shape (a plain password in a free-text field),
// which is a stated limit.

// Secret is one finding. Seen is safe to show and log: the start and the last
// four characters, never the value.
type Secret struct {
	What  string // e.g. "anthropic_api_key"
	Where string // JSON path inside the arguments, e.g. "arguments.query"
	Seen  string // e.g. "sk-ant-…a1f2"
}

var secretShapes = []struct {
	what string
	re   *regexp.Regexp
	keep int // characters of the start to show
}{
	{"private_key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY(?: BLOCK)?-----`), 11},
	{"anthropic_api_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`), 7},
	{"openai_api_key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{32,}`), 3},
	{"aws_access_key_id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 4},
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})`), 4},
	{"slack_token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{20,}`), 5},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), 4},
	{"stripe_secret_key", regexp.MustCompile(`\b(?:sk|rk)_live_[0-9A-Za-z]{20,}`), 8},
	{"agentgate_agent_key", regexp.MustCompile(`\bag_[A-Za-z0-9_-]{40,}`), 3},
	{"password_in_url", regexp.MustCompile(`[a-z][a-z0-9+.-]*://[^\s/:@"']+:[^\s/@"']{6,}@`), 0},
}

// FindSecret reports the first key or password in a tool call's arguments,
// in a stable order (by path). ok is false when there is none.
func FindSecret(args json.RawMessage) (Secret, bool) {
	var v any
	if json.Unmarshal(args, &v) != nil {
		v = string(args)
	}
	var found []Secret
	walkPaths(v, "arguments", func(path, s string) {
		for _, sh := range secretShapes {
			if m := sh.re.FindString(s); m != "" {
				found = append(found, Secret{What: sh.what, Where: path, Seen: mask(m, sh.keep)})
				return
			}
		}
	})
	if len(found) == 0 {
		return Secret{}, false
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Where < found[j].Where })
	return found[0], true
}

func walkPaths(v any, path string, fn func(path, s string)) {
	switch x := v.(type) {
	case string:
		fn(path, x)
	case []any:
		for i, e := range x {
			walkPaths(e, path+"["+strconv.Itoa(i)+"]", fn)
		}
	case map[string]any:
		for k, e := range x {
			walkPaths(e, path+"."+k, fn)
		}
	}
}

// mask keeps the first keep characters and the last four.
func mask(s string, keep int) string {
	if keep == 0 || len(s) < keep+8 {
		return "…"
	}
	return s[:keep] + "…" + s[len(s)-4:]
}

// StripKey removes a top-level key from a JSON object. When the key is there
// it returns the object without it, re-encoded with sorted keys, the removed
// value as a string, and found = true; otherwise args unchanged.
func StripKey(args json.RawMessage, key string) (out json.RawMessage, val string, found bool) {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil || m == nil {
		return args, "", false
	}
	v, ok := m[key]
	if !ok {
		return args, "", false
	}
	val, _ = v.(string)
	delete(m, key)
	b, _ := json.Marshal(m)
	return b, strings.TrimSpace(val), true
}
