package screen

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Rule ids. They are stable: they appear in audit and screen_events rows.
const (
	RuleInstructionOverride = "instruction_override"
	RuleRoleMarker          = "role_marker"
	RuleHiddenUnicode       = "hidden_unicode"
	RuleEncodedPayload      = "encoded_payload"
	RuleExfilURL            = "exfil_url"
)

var (
	// Phrases that try to replace or hide the agent's instructions. Matched on
	// lower-cased text with whitespace collapsed.
	overrideRes = []*regexp.Regexp{
		re(`\b(ignore|disregard|forget|override|bypass|discard)\b[^.\n]{0,40}\b(previous|prior|above|earlier|preceding|all|any|your|these|those|system|safety)\b[^.\n]{0,30}\b(instructions?|prompts?|rules?|directives?|guidelines?|constraints?|restrictions?)\b`),
		re(`\b(new|updated|revised|real) (system )?(instructions?|directive|prompt)\s*:`),
		re(`\byou are (now|no longer) (an? |the )?(unrestricted|jailbroken|different|evil|dan\b|free of)`),
		re(`\b(enter|switch to|activate) (developer|debug|admin|god|dan) mode\b`),
		re(`\bfrom now on,? (you|ignore|always|only)\b`),
		re(`\b(do not|don'?t|never) (tell|inform|mention|reveal|show|let)( this| that| it| them)?( to)?( the)? (user|human|operator)\b`),
		re(`\bwithout (telling|informing|notifying|alerting) (the )?(user|human|operator)\b`),
		re(`\b(reveal|print|output|repeat|show|leak|disclose) (me )?(your|the) (system|initial|hidden|secret) (prompt|instructions?)\b`),
		re(`(^|\n)\s*(important|attention|note|message|instructions?)\s*(to|for)\s*(the )?(ai|assistant|llm|model|agent)\b`),
		re(`\bbefore (answering|responding|continuing|you answer)[^.\n]{0,20}\b(you must|first|always)\b[^.\n]{0,40}\b(call|use|read|send|run|execute|fetch)\b`),
	}

	// Markers that make text look like a different speaker or a chat template.
	roleRes = []*regexp.Regexp{
		re(`(?im)^\s*(system|developer|assistant|human)\s*:\s*.*\b(you|your|must|should|ignore|instructions?|assistant|ai)\b`),
		re(`<\|(im_start|im_end|system|assistant|user|endoftext)\|>`),
		re(`\[/?INST\]`),
		re(`<<\s*/?SYS\s*>>`),
		re(`(?i)</?(system|assistant|tool_result|function_results|instructions?)>`),
	}

	// A URL that carries data out: an image whose query has one long encoded
	// value (see mdExfil), or "send <secret> to <url>" in either order.
	mdImageRe     = re(`!\[[^\]]*\]\(\s*https?://[^)\s?]*\?([^)\s#]*)`)
	payloadRe     = regexp.MustCompile(`^[A-Za-z0-9%+/_.=-]{16,}$`)
	mdTemplateRe  = re(`!\[[^\]]*\]\(\s*https?://[^)\s]*(\{\{|%7B%7B|\$\{)`)
	secretQueryRe = re(`https?://[^\s"')]*[?&](?:token|secret|password|api_?key|auth|session|cookie|data|payload)=[A-Za-z0-9%+/=_.-]{24,}`)
	sendThenURLRe = re(`\b(send|post|upload|forward|exfiltrate|leak|transmit|email|submit|append)\b[^\n]{0,120}\b(secrets?|tokens?|passwords?|credentials?|api[ _-]?keys?|private keys?|\.env|cookies?|conversation|chat history|system prompt|ssh keys?)\b[^\n]{0,120}https?://`)
	urlThenSendRe = re(`https?://[^\n]{0,120}\b(send|post|upload|forward|exfiltrate|leak|transmit|submit)\b[^\n]{0,120}\b(secrets?|tokens?|passwords?|credentials?|api[ _-]?keys?|private keys?|\.env|cookies?|conversation|chat history|system prompt|ssh keys?)\b`)

	base64Re = regexp.MustCompile(`[A-Za-z0-9+/_-]{40,}={0,2}`)
	hexRe    = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}){24,}\b`)
	pctRe    = regexp.MustCompile(`(?:%[0-9A-Fa-f]{2}){20,}`)
	spaceRe  = regexp.MustCompile(`\s+`)
)

func re(p string) *regexp.Regexp { return regexp.MustCompile(p) }

// zeroWidthRun is how many zero-width characters in a row count as hiding
// something. Real pages carry them one at a time (documentation sites put one
// in every heading link), while data encoded in them needs them side by side.
const zeroWidthRun = 2

// zeroWidth reports the characters that are harmless alone and render as
// nothing. They are removed before the text rules run.
func zeroWidth(r rune) bool {
	return r == 0x200B || r == 0x200C || r == 0x2060 || r == 0x180E || r == 0xFEFF
}

// hidden is the set of characters that render as nothing (or reorder text) and
// so let a human reviewer see one thing while the model reads another. ZWJ
// (U+200D) is left out: emoji sequences need it.
func hidden(r rune) bool {
	switch {
	case r >= 0x200E && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true // direction marks and bidi overrides / isolates
	case r >= 0x2061 && r <= 0x2064:
		return true // invisible math operators
	case r >= 0xE0000 && r <= 0xE007F:
		return true // Unicode tag characters: invisible ASCII
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF:
		return false // variation selectors are normal in emoji; not flagged
	}
	return false
}

// scanText runs every deterministic rule over one string. Order: hidden
// characters first (they also hide the others), then phrases, markers, URLs,
// then encoded forms, which are decoded once and re-scanned.
func scanText(s string) (rule, detail string, hit bool) {
	run := 0
	for i, r := range s {
		if r == 0xFEFF && i == 0 {
			continue // a byte-order mark at the start is an encoding artefact
		}
		if zeroWidth(r) {
			if run++; run >= zeroWidthRun {
				return RuleHiddenUnicode, "a run of invisible characters", true
			}
			continue
		}
		run = 0
		if hidden(r) {
			return RuleHiddenUnicode, "invisible or direction-changing character", true
		}
	}
	// Rules read the text a model would act on, so a zero-width character
	// cannot split a flagged phrase and slip past them.
	s = strings.Map(func(r rune) rune {
		if zeroWidth(r) {
			return -1
		}
		return r
	}, s)
	if rule, detail, hit = scanPlain(s); hit {
		return
	}
	for _, d := range decodedForms(s) {
		if _, _, h := scanPlain(d); h {
			return RuleEncodedPayload, "encoded text decodes to a flagged phrase", true
		}
		for _, r := range d {
			if hidden(r) {
				return RuleEncodedPayload, "encoded text hides invisible characters", true
			}
		}
	}
	return "", "", false
}

func scanPlain(s string) (rule, detail string, hit bool) {
	norm := spaceRe.ReplaceAllString(strings.ToLower(s), " ")
	for _, r := range overrideRes {
		if r.MatchString(norm) {
			return RuleInstructionOverride, "matches an instruction-override phrase", true
		}
	}
	for _, r := range roleRes {
		if r.MatchString(s) {
			return RuleRoleMarker, "looks like another speaker or a chat template", true
		}
	}
	switch {
	case mdExfil(s), mdTemplateRe.MatchString(s), secretQueryRe.MatchString(s),
		sendThenURLRe.MatchString(norm), urlThenSendRe.MatchString(norm):
		return RuleExfilURL, "a URL that would carry data out", true
	}
	return "", "", false
}

// decodedForms returns the readable text hidden in base64, hex and percent
// encoded runs. Only text that decodes to valid, mostly printable UTF-8 counts:
// a hash or an image is not text and is left alone.
func decodedForms(s string) []string {
	var out []string
	add := func(b []byte) {
		if len(out) >= 8 || !utf8.Valid(b) || len(b) < 12 {
			return
		}
		printable := 0
		for _, r := range string(b) {
			if unicode.IsPrint(r) || unicode.IsSpace(r) {
				printable++
			}
		}
		if printable*10 >= utf8.RuneCount(b)*9 {
			out = append(out, string(b))
		}
	}
	for _, m := range base64Re.FindAllString(s, 8) {
		m = strings.TrimRight(m, "=")
		for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
			if b, err := enc.DecodeString(m); err == nil {
				add(b)
				break
			}
		}
	}
	for _, m := range hexRe.FindAllString(s, 8) {
		if b, err := hex.DecodeString(m); err == nil {
			add(b)
		}
	}
	for _, m := range pctRe.FindAllString(s, 8) {
		if d, err := url.PathUnescape(m); err == nil {
			add([]byte(d))
		}
	}
	return out
}

// mdExfil reports an image whose query carries a long encoded value. A run
// of short parameters (an image CDN's ?w=3840&q=90&fm=webp), a resize recipe
// (?x-oss-process=image/resize,p_40) and an image proxy's encoded address
// (?url=https%3A%2F%2F...) are not payloads. Every parameter is checked, so
// adding a harmless one does not hide a payload.
func mdExfil(s string) bool {
	for _, m := range mdImageRe.FindAllStringSubmatch(s, -1) {
		for _, kv := range strings.Split(m[1], "&") {
			_, v, ok := strings.Cut(kv, "=")
			if !ok || !payloadRe.MatchString(v) {
				continue
			}
			lv := strings.ToLower(v)
			if strings.HasPrefix(lv, "http%3a%2f%2f") || strings.HasPrefix(lv, "https%3a%2f%2f") {
				continue
			}
			return true
		}
	}
	return false
}
