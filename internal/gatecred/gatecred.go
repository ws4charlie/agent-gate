// Package gatecred mints the Gate credential: the short-lived, Ed25519-signed
// permission QuorumVault requires before it signs one payment from a
// Gate-governed wallet.
//
// The contract is Vault's spec (docs/gate-credential.md in quorum-vault) and
// its shared test vectors, copied unchanged into testdata/gate-credential.
// Nothing here imports Vault code. Gate only mints; it never verifies its own
// credentials in production (the verifier lives in the tests, to run the
// vectors).
package gatecred

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"
)

// The contract's fixed values.
const (
	// DomainPrefix is prepended to the canonical form before signing, so these
	// signatures cannot be mistaken for any other use of the key.
	DomainPrefix = "qv-gate-credential-v1\n"
	// Version is the only credential version.
	Version = 1
	// MaxTTL is the longest lifetime Vault accepts by default. Mint refuses
	// anything longer. Gate itself should mint with DefaultTTL.
	MaxTTL = 600 * time.Second
	// DefaultTTL is what Gate mints with: room to call Vault and retry once,
	// short enough that a stolen credential is worth little.
	DefaultTTL = 90 * time.Second
	// MaxSize is the largest credential (with sig) Vault accepts, in bytes.
	MaxSize = 4096
	// BaseSepolia is the only chain Gate pays on today.
	BaseSepolia = "eip155:84532"

	maxSafeInt = 1<<53 - 1 // integers stay below 2^53 so both sides hold them exactly
)

// Credential is every field of the credential except sig.
type Credential struct {
	V              int    // 1; Mint fills 0 with 1
	Kid            string // the signing key's id; Mint fills "" with the key's kid
	CredentialID   string // lower-case UUID; Mint fills "" with a fresh one
	ObligationID   string // opaque UTF-8, 1-128 bytes
	Audience       string // Vault deployment name, e.g. qv-staging
	WalletID       string // lower-case UUID of the paying wallet in Vault
	Chain          string // eip155:<id>
	Asset          string // native, or the token contract as lower-case 0x hex
	Payee          string // lower-case 0x hex
	Amount         string // integer minor units, decimal
	IssuedAt       int64  // unix seconds
	ExpiresAt      int64  // unix seconds, after IssuedAt
	PolicyVersion  string // opaque UTF-8, 1-64 bytes
	DecisionReason string // opaque UTF-8, 1-256 bytes
}

// Signed is a minted credential, ready to embed as a JSON object (not a
// string) in the signing request.
type Signed struct {
	// JSON is the credential object with sig, in canonical (RFC 8785) form.
	JSON         json.RawMessage
	CredentialID string
	ExpiresAt    int64
}

// FieldError says which field is wrong and why. Field is empty when the
// problem is not one field's.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string {
	if e.Field == "" {
		return "gatecred: " + e.Reason
	}
	return "gatecred: field " + e.Field + ": " + e.Reason
}

func fieldErr(f, format string, a ...any) *FieldError {
	return &FieldError{Field: f, Reason: fmt.Sprintf(format, a...)}
}

// Key is the Gate credential signing key. It exists only for this purpose:
// not the x402 wallet key, not a SPIFFE key. Build it with NewKey or LoadKey.
//
// The private key is held only inside the sign closure. fmt and every other
// reflection-based printer show a func as an address, never what it captures,
// so a Key prints no key bytes even where its methods cannot run (as an
// unexported field of another struct, or through fmt's bad-verb path, which
// dereferences pointers to structs, arrays and slices).
type Key struct {
	Kid  string
	pub  ed25519.PublicKey
	sign func(msg []byte) []byte
}

// keyFrom wraps priv without the checks NewKey makes (tests use it for the
// vectors' TEST ONLY keys). priv is copied.
func keyFrom(kid string, priv ed25519.PrivateKey) Key {
	p := append(ed25519.PrivateKey(nil), priv...)
	return Key{
		Kid:  kid,
		pub:  append(ed25519.PublicKey(nil), p[ed25519.SeedSize:]...),
		sign: func(msg []byte) []byte { return ed25519.Sign(p, msg) },
	}
}

// The formatting paths a Key's own methods can reach print only the kid.
func (k Key) String() string   { return "gatecred.Key(" + k.Kid + ")" }
func (k Key) GoString() string { return k.String() }

// Format handles every fmt verb. String alone covers only %v %s %x %X %q.
func (k Key) Format(f fmt.State, _ rune) { io.WriteString(f, k.String()) }

func (k Key) LogValue() slog.Value         { return slog.StringValue(k.String()) }
func (k Key) MarshalJSON() ([]byte, error) { return json.Marshal(k.String()) }
func (k Key) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// Public is the key's public half, nil for a zero Key.
func (k Key) Public() ed25519.PublicKey { return append(ed25519.PublicKey(nil), k.pub...) }

func (k Key) PublicBase64() string               { return EncodePublicKey(k.Public()) }
func EncodePublicKey(p ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(p) }

// NewCredentialID returns a fresh random (v4) UUID in lower case.
func NewCredentialID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("gatecred: no randomness: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Mint validates c against the contract, signs it with key and returns the
// credential with sig. It refuses anything Vault would refuse as malformed,
// and a lifetime above MaxTTL: Gate never sends a credential that is wrong by
// construction.
func Mint(key Key, c Credential) (Signed, error) {
	if key.sign == nil || len(key.pub) != ed25519.PublicKeySize {
		return Signed{}, errors.New("gatecred: no signing key")
	}
	if err := validKid(key.Kid); err != nil {
		return Signed{}, fmt.Errorf("gatecred: key kid: %s", err.Reason)
	}
	if c.V == 0 {
		c.V = Version
	}
	if c.Kid == "" {
		c.Kid = key.Kid
	}
	if c.Kid != key.Kid {
		return Signed{}, fieldErr("kid", "%q is not the signing key's kid %q", c.Kid, key.Kid)
	}
	if c.CredentialID == "" {
		c.CredentialID = NewCredentialID()
	}
	if err := validate(c); err != nil {
		return Signed{}, err
	}
	if c.ExpiresAt-c.IssuedAt > int64(MaxTTL/time.Second) {
		return Signed{}, fieldErr("expires_at", "lifetime %ds is above the maximum %ds", c.ExpiresAt-c.IssuedAt, int64(MaxTTL/time.Second))
	}
	sig := key.sign(SignedBytes(c))
	out := encodeJCS(append(members(c), member{key: "sig", str: base64.RawURLEncoding.EncodeToString(sig)}))
	if err := checkSize(out); err != nil {
		return Signed{}, err
	}
	return Signed{JSON: out, CredentialID: c.CredentialID, ExpiresAt: c.ExpiresAt}, nil
}

func checkSize(b []byte) error {
	if len(b) > MaxSize {
		return fieldErr("", "credential is %d bytes, above the maximum %d", len(b), MaxSize)
	}
	return nil
}

// SignedBytes is what is signed: DomainPrefix, then the RFC 8785 canonical
// form of the credential without sig.
func SignedBytes(c Credential) []byte {
	return append([]byte(DomainPrefix), encodeJCS(members(c))...)
}

func members(c Credential) []member {
	return []member{
		{key: "v", num: int64(c.V), isNum: true},
		{key: "kid", str: c.Kid},
		{key: "credential_id", str: c.CredentialID},
		{key: "obligation_id", str: c.ObligationID},
		{key: "audience", str: c.Audience},
		{key: "wallet_id", str: c.WalletID},
		{key: "chain", str: c.Chain},
		{key: "asset", str: c.Asset},
		{key: "payee", str: c.Payee},
		{key: "amount", str: c.Amount},
		{key: "issued_at", num: c.IssuedAt, isNum: true},
		{key: "expires_at", num: c.ExpiresAt, isNum: true},
		{key: "policy_version", str: c.PolicyVersion},
		{key: "decision_reason", str: c.DecisionReason},
	}
}

// validate checks every field's format, in the spec's table order, and returns
// the first that fails. It does not check the lifetime against MaxTTL (that
// is a verification check with its own code; Mint applies it separately).
func validate(c Credential) *FieldError {
	if c.V != Version {
		return fieldErr("v", "version %d is not %d", c.V, Version)
	}
	if err := validKid(c.Kid); err != nil {
		return err
	}
	if !isUUID(c.CredentialID) {
		return fieldErr("credential_id", "not a lower-case UUID")
	}
	if err := opaque("obligation_id", c.ObligationID, 128); err != nil {
		return err
	}
	if !validAudience(c.Audience) {
		return fieldErr("audience", "need 1-64 of a-z 0-9 . _ -, starting with a letter or digit")
	}
	if !isUUID(c.WalletID) {
		return fieldErr("wallet_id", "not a lower-case UUID")
	}
	if !validChain(c.Chain) {
		return fieldErr("chain", "need eip155:<decimal id> without a leading zero")
	}
	if c.Asset != "native" && !isAddress(c.Asset) {
		return fieldErr("asset", "need native or 0x and 40 lower-case hex digits")
	}
	if !isAddress(c.Payee) {
		return fieldErr("payee", "need 0x and 40 lower-case hex digits")
	}
	if !validAmount(c.Amount) {
		return fieldErr("amount", "need decimal digits, no leading zero, from 1 to 2^256-1")
	}
	if c.IssuedAt < 1 || c.IssuedAt > maxSafeInt {
		return fieldErr("issued_at", "need unix seconds from 1 to 2^53-1")
	}
	if c.ExpiresAt <= c.IssuedAt || c.ExpiresAt > maxSafeInt {
		return fieldErr("expires_at", "need unix seconds after issued_at and below 2^53")
	}
	if err := opaque("policy_version", c.PolicyVersion, 64); err != nil {
		return err
	}
	return opaque("decision_reason", c.DecisionReason, 256)
}

func validKid(s string) *FieldError {
	if len(s) < 1 || len(s) > 64 {
		return fieldErr("kid", "need 1-64 characters")
	}
	for i := 0; i < len(s); i++ {
		if !(isLower(s[i]) || isUpper(s[i]) || isDigit(s[i]) || s[i] == '.' || s[i] == '_' || s[i] == '-') {
			return fieldErr("kid", "only A-Z a-z 0-9 . _ -")
		}
	}
	return nil
}

// opaque: valid UTF-8 (which excludes surrogates), no control characters,
// 1..max bytes. Kept byte for byte; never normalised.
func opaque(field, s string, max int) *FieldError {
	if len(s) < 1 || len(s) > max {
		return fieldErr(field, "%d bytes; need 1-%d bytes of UTF-8", len(s), max)
	}
	if !utf8.ValidString(s) {
		return fieldErr(field, "not valid UTF-8")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 {
			return fieldErr(field, "control character U+%04X", s[i])
		}
	}
	return nil
}

func validAudience(s string) bool {
	if len(s) < 1 || len(s) > 64 || !(isLower(s[0]) || isDigit(s[0])) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(isLower(s[i]) || isDigit(s[i]) || s[i] == '.' || s[i] == '_' || s[i] == '-') {
			return false
		}
	}
	return true
}

func validChain(s string) bool {
	id, ok := strings.CutPrefix(s, "eip155:")
	return ok && len(id) <= 32 && isDecimal(id)
}

var maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

func validAmount(s string) bool {
	if !isDecimal(s) || len(s) > 78 {
		return false
	}
	n, ok := new(big.Int).SetString(s, 10)
	return ok && n.Cmp(maxUint256) <= 0
}

// isDecimal: digits, at least 1, no leading zero (so "0" is refused too).
func isDecimal(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func isAddress(s string) bool {
	h, ok := strings.CutPrefix(s, "0x")
	return ok && len(h) == 40 && isLowerHex(h)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
		} else if !isLowerHex(s[i : i+1]) {
			return false
		}
	}
	return true
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !(isDigit(s[i]) || (s[i] >= 'a' && s[i] <= 'f')) {
			return false
		}
	}
	return true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func isLower(b byte) bool { return b >= 'a' && b <= 'z' }
func isUpper(b byte) bool { return b >= 'A' && b <= 'Z' }
