package gatecred

// The verifier below exists only so the whole shared vector file can run in
// this repository. Production Gate never verifies its own credentials, so it
// lives in a _test.go file and is not part of the package's API.

import (
	"crypto/ed25519"
	"encoding/base64"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

type verifyConfig struct {
	audience string
	skew     int64
	maxTTL   int64
	keys     map[string]ed25519.PublicKey
}

type payment struct {
	WalletID    string `json:"wallet_id"`
	Chain       string `json:"chain"`
	Asset       string `json:"asset"`
	Payee       string `json:"payee"`
	Amount      string `json:"amount"`
	ValidAfter  *int64 `json:"valid_after"`
	ValidBefore *int64 `json:"valid_before"`
}

type verifyResult struct {
	code, field string
	signedBytes []byte // set once the credential parsed
}

func refuse(code, field string) verifyResult { return verifyResult{code: code, field: field} }

// verify runs the spec's pure checks 1 to 11 in order and stops at the first
// failure. code "" means accepted.
func verify(cfg verifyConfig, cred []byte, present bool, p payment, now int64) verifyResult {
	if !present || len(cred) == 0 {
		return refuse("credential_missing", "")
	}
	c, sig, perr := parseStrict(cred)
	if perr != nil {
		return refuse("credential_malformed", perr.Field)
	}
	sb := SignedBytes(c)
	out := func(code, field string) verifyResult { return verifyResult{code, field, sb} }
	pub, ok := cfg.keys[c.Kid]
	if !ok {
		return out("kid_unknown", "kid")
	}
	if !ed25519.Verify(pub, sb, sig) {
		return out("signature_invalid", "")
	}
	if c.Audience != cfg.audience {
		return out("audience_mismatch", "audience")
	}
	if c.WalletID != p.WalletID {
		return out("wallet_mismatch", "wallet_id")
	}
	if c.ExpiresAt-c.IssuedAt > cfg.maxTTL {
		return out("ttl_too_long", "expires_at")
	}
	if c.IssuedAt > now+cfg.skew {
		return out("not_yet_valid", "issued_at")
	}
	if now >= c.ExpiresAt+cfg.skew {
		return out("expired", "expires_at")
	}
	for _, f := range []struct{ name, got, want string }{
		{"chain", c.Chain, p.Chain}, {"asset", c.Asset, p.Asset}, {"payee", c.Payee, p.Payee}, {"amount", c.Amount, p.Amount},
	} {
		if f.got != f.want {
			return out("payment_mismatch", f.name)
		}
	}
	if p.ValidAfter != nil || p.ValidBefore != nil {
		if p.ValidAfter == nil || p.ValidBefore == nil ||
			*p.ValidAfter < c.IssuedAt || *p.ValidBefore > c.ExpiresAt || *p.ValidAfter >= *p.ValidBefore {
			return out("authorization_window", "")
		}
	}
	return out("", "")
}

// parseStrict parses the credential text under the spec's strict rules: one
// object, string keys, string or integer values, no duplicate, unknown or
// missing member, no text after the object, valid UTF-8, no lone surrogate.
// A syntax error has no field; a member's error names it.
func parseStrict(b []byte) (Credential, []byte, *FieldError) {
	if len(b) > MaxSize {
		return Credential{}, nil, fieldErr("", "too large")
	}
	if !utf8.Valid(b) {
		return Credential{}, nil, fieldErr("", "not UTF-8")
	}
	p := &parser{b: b}
	type value struct {
		isNum bool
		str   string
		num   int64
		bad   bool // a number that is not a plain integer, or another JSON type
	}
	vals := map[string]value{}
	p.ws()
	if !p.eat('{') {
		return Credential{}, nil, fieldErr("", "not an object")
	}
	p.ws()
	if !p.eat('}') {
		for {
			p.ws()
			k, ok := p.str()
			if !ok {
				return Credential{}, nil, fieldErr("", "bad member name")
			}
			p.ws()
			if !p.eat(':') {
				return Credential{}, nil, fieldErr("", "missing colon")
			}
			p.ws()
			var v value
			switch {
			case p.peek() == '"':
				s, ok := p.str()
				if !ok {
					return Credential{}, nil, fieldErr("", "bad string")
				}
				v.str = s
			case p.peek() == '-' || isDigit(p.peek()):
				tok, ok := p.number()
				if !ok {
					return Credential{}, nil, fieldErr("", "bad number")
				}
				n, err := strconv.ParseInt(tok, 10, 64)
				v = value{isNum: true, num: n, bad: err != nil || tok[0] == '-'}
			default:
				if !p.skipLiteral() {
					return Credential{}, nil, fieldErr("", "bad value")
				}
				v.bad = true
			}
			if _, dup := vals[k]; dup {
				return Credential{}, nil, fieldErr(k, "appears twice")
			}
			vals[k] = v
			p.ws()
			if p.eat(',') {
				continue
			}
			if p.eat('}') {
				break
			}
			return Credential{}, nil, fieldErr("", "expected , or }")
		}
	}
	p.ws()
	if p.i != len(p.b) {
		return Credential{}, nil, fieldErr("", "text after the object")
	}

	numFields := map[string]bool{"v": true, "issued_at": true, "expires_at": true}
	order := []string{"v", "kid", "credential_id", "obligation_id", "audience", "wallet_id", "chain", "asset", "payee",
		"amount", "issued_at", "expires_at", "policy_version", "decision_reason", "sig"}
	known := map[string]bool{}
	for _, f := range order {
		known[f] = true
	}
	for k := range vals {
		if !known[k] {
			return Credential{}, nil, fieldErr(k, "unknown field")
		}
	}
	for _, f := range order {
		v, ok := vals[f]
		if !ok {
			return Credential{}, nil, fieldErr(f, "missing")
		}
		if v.bad || v.isNum != numFields[f] {
			return Credential{}, nil, fieldErr(f, "wrong type")
		}
	}
	s := func(f string) string { return vals[f].str }
	n := func(f string) int64 { return vals[f].num }
	c := Credential{
		V: int(n("v")), Kid: s("kid"), CredentialID: s("credential_id"), ObligationID: s("obligation_id"),
		Audience: s("audience"), WalletID: s("wallet_id"), Chain: s("chain"), Asset: s("asset"), Payee: s("payee"),
		Amount: s("amount"), IssuedAt: n("issued_at"), ExpiresAt: n("expires_at"),
		PolicyVersion: s("policy_version"), DecisionReason: s("decision_reason"),
	}
	if err := validate(c); err != nil {
		return Credential{}, nil, err
	}
	sig, ok := decodeSig(s("sig"))
	if !ok {
		return Credential{}, nil, fieldErr("sig", "not 64 bytes of strict base64url")
	}
	return c, sig, nil
}

// decodeSig checks every byte against the base64url alphabet before decoding:
// many decoders skip CR and LF, so a strict decoder alone is not enough.
func decodeSig(s string) ([]byte, bool) {
	if len(s) != 86 {
		return nil, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(isLower(c) || isUpper(c) || isDigit(c) || c == '-' || c == '_') {
			return nil, false
		}
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return sig, err == nil && len(sig) == ed25519.SignatureSize
}

type parser struct {
	b []byte
	i int
}

func (p *parser) peek() byte {
	if p.i < len(p.b) {
		return p.b[p.i]
	}
	return 0
}

func (p *parser) eat(c byte) bool {
	if p.peek() == c && p.i < len(p.b) {
		p.i++
		return true
	}
	return false
}

func (p *parser) ws() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\t' || p.b[p.i] == '\n' || p.b[p.i] == '\r') {
		p.i++
	}
}

// str reads a JSON string. Raw controls are refused (JSON forbids them); a
// \u escape of a surrogate must be a high one followed by a low one.
func (p *parser) str() (string, bool) {
	if !p.eat('"') {
		return "", false
	}
	var out []byte
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			return string(out), true
		case c < 0x20:
			return "", false
		case c != '\\':
			out = append(out, c)
			p.i++
			continue
		}
		p.i++ // the backslash
		e := p.peek()
		p.i++
		switch e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return "", false
			}
			if utf16.IsSurrogate(r) {
				if r >= 0xdc00 || !p.eat('\\') || !p.eat('u') {
					return "", false
				}
				lo, ok := p.hex4()
				if !ok || lo < 0xdc00 || lo > 0xdfff {
					return "", false
				}
				r = utf16.DecodeRune(r, lo)
			}
			out = utf8.AppendRune(out, r)
		default:
			return "", false
		}
	}
	return "", false
}

func (p *parser) hex4() (rune, bool) {
	if p.i+4 > len(p.b) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	p.i += 4
	return rune(n), true
}

// number reads a JSON number token (RFC 8259 grammar) and returns its text.
func (p *parser) number() (string, bool) {
	start := p.i
	p.eat('-')
	switch {
	case p.eat('0'):
	case p.i < len(p.b) && p.b[p.i] >= '1' && p.b[p.i] <= '9':
		p.digits()
	default:
		return "", false
	}
	if p.eat('.') {
		if p.digits() == 0 {
			return "", false
		}
	}
	if p.eat('e') || p.eat('E') {
		if !p.eat('+') {
			p.eat('-')
		}
		if p.digits() == 0 {
			return "", false
		}
	}
	return string(p.b[start:p.i]), true
}

func (p *parser) digits() int {
	n := 0
	for p.i < len(p.b) && isDigit(p.b[p.i]) {
		p.i++
		n++
	}
	return n
}

// skipLiteral accepts true, false, null and nested arrays/objects only far
// enough to call them the wrong type; the credential has no such member.
func (p *parser) skipLiteral() bool {
	for _, lit := range []string{"true", "false", "null"} {
		if p.i+len(lit) <= len(p.b) && string(p.b[p.i:p.i+len(lit)]) == lit {
			p.i += len(lit)
			return true
		}
	}
	return false
}
