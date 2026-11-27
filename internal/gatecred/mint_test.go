package gatecred

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// realKey is a throwaway key that is not one of the vectors' test keys.
func realKey(t *testing.T) Key {
	t.Helper()
	seed := sha256.Sum256([]byte("gatecred unit test " + t.Name()))
	return Key{Kid: "gate-test", Priv: ed25519.NewKeyFromSeed(seed[:])}
}

func good() Credential {
	return Credential{
		ObligationID:   "obl-1",
		Audience:       "qv-staging",
		WalletID:       "7c1f0a52-3b8e-4d6a-9f21-5e0b7a4c8d13",
		Chain:          BaseSepolia,
		Asset:          "0x036cbd53842c5426634e7929541ec2318f3dcf7e",
		Payee:          "0x70997970c51812dc3a010c7d01b50e0d17dc79c8",
		Amount:         "2500000",
		IssuedAt:       1790000000,
		ExpiresAt:      1790000090,
		PolicyVersion:  "policy-v1",
		DecisionReason: "within budget",
	}
}

func TestMintRoundTrip(t *testing.T) {
	k := realKey(t)
	s, err := Mint(k, good())
	if err != nil {
		t.Fatal(err)
	}
	if !isUUID(s.CredentialID) || s.CredentialID[14] != '4' {
		t.Fatalf("credential_id %q is not a fresh v4 UUID", s.CredentialID)
	}
	c, sig, perr := parseStrict(s.JSON)
	if perr != nil {
		t.Fatalf("minted credential does not parse strictly: %v\n%s", perr, s.JSON)
	}
	if c.V != 1 || c.Kid != "gate-test" || c.CredentialID != s.CredentialID {
		t.Fatalf("defaults not filled: %+v", c)
	}
	if !ed25519.Verify(k.Public(), SignedBytes(c), sig) {
		t.Fatal("signature does not verify")
	}
	// It is a JSON object, ready to embed as one.
	var m map[string]any
	if err := json.Unmarshal(s.JSON, &m); err != nil || len(m) != 15 {
		t.Fatalf("not a 15-member object: %v %d", err, len(m))
	}
	s2, _ := Mint(k, good())
	if s2.CredentialID == s.CredentialID {
		t.Fatal("two mints share a credential_id")
	}
}

// TestJCSStrings covers the escapes no vector can reach (controls are refused
// before signing) as well as those it does.
func TestJCSStrings(t *testing.T) {
	for in, want := range map[string]string{
		"a\"b\\c":      `"a\"b\\c"`,
		"\b\f\n\r\t":   `"\b\f\n\r\t"`,
		"\x00\x01\x1f": `"\u0000\u0001\u001f"`,
		"\x7f/<>&":     "\"\x7f/<>&\"",
		"é  😀":         "\"é  😀\"",
	} {
		if got := string(appendJCSString(nil, in)); got != want {
			t.Errorf("%q: got %s, want %s", in, got, want)
		}
	}
	got := string(encodeJCS([]member{{key: "b", num: 2, isNum: true}, {key: "a", str: "x"}, {key: "aa", num: 9007199254740991, isNum: true}}))
	if want := `{"a":"x","aa":9007199254740991,"b":2}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// TestMintRefusesMalformed: one row per malformed class in the vectors (and a
// few Gate-only ones). Mint must refuse each and name the field.
func TestMintRefusesMalformed(t *testing.T) {
	cases := []struct {
		name, field string
		edit        func(*Credential)
	}{
		{"obligation_id 129 bytes (43 x 3-byte rune)", "obligation_id", func(c *Credential) { c.ObligationID = strings.Repeat("中", 43) }},
		{"policy_version 65 bytes", "policy_version", func(c *Credential) { c.PolicyVersion = strings.Repeat("é", 32) + "x" }},
		{"decision_reason 257 bytes", "decision_reason", func(c *Credential) { c.DecisionReason = strings.Repeat("a", 257) }},
		{"opaque empty", "obligation_id", func(c *Credential) { c.ObligationID = "" }},
		{"control character", "decision_reason", func(c *Credential) { c.DecisionReason = "a\x01b" }},
		{"newline", "decision_reason", func(c *Credential) { c.DecisionReason = "line one\nline two" }},
		{"invalid UTF-8", "decision_reason", func(c *Credential) { c.DecisionReason = "a\xc3(" }},
		{"surrogate as UTF-8", "policy_version", func(c *Credential) { c.PolicyVersion = "\xed\xa0\x80" }},
		{"upper-case payee", "payee", func(c *Credential) { c.Payee = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8" }},
		{"upper-case asset", "asset", func(c *Credential) { c.Asset = "0x036CBD53842c5426634e7929541ec2318f3dcf7e" }},
		{"asset NATIVE", "asset", func(c *Credential) { c.Asset = "NATIVE" }},
		{"short payee", "payee", func(c *Credential) { c.Payee = "0x70997970c51812dc3a010c7d01b50e0d17dc79c" }},
		{"upper-case credential_id", "credential_id", func(c *Credential) { c.CredentialID = "0B6C1D52-8A4F-4E17-B3D9-5F2A7C9E2C0D" }},
		{"bad uuid credential_id", "credential_id", func(c *Credential) { c.CredentialID = "0b6c1d528a4f4e17b3d95f2a7c9e2c0d" }},
		{"bad uuid wallet_id", "wallet_id", func(c *Credential) { c.WalletID = "7c1f0a52-3b8e-4d6a-9f21-5e0b7a4c8d1" }},
		{"chain leading zero", "chain", func(c *Credential) { c.Chain = "eip155:084532" }},
		{"chain other namespace", "chain", func(c *Credential) { c.Chain = "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp" }},
		{"amount zero", "amount", func(c *Credential) { c.Amount = "0" }},
		{"amount leading zero", "amount", func(c *Credential) { c.Amount = "01000" }},
		{"amount negative", "amount", func(c *Credential) { c.Amount = "-5" }},
		{"amount decimal point", "amount", func(c *Credential) { c.Amount = "1.5" }},
		{"amount above uint256", "amount", func(c *Credential) {
			c.Amount = "115792089237316195423570985008687907853269984665640564039457584007913129639936"
		}},
		{"audience upper case", "audience", func(c *Credential) { c.Audience = "QV-staging" }},
		{"kid bad character", "kid", func(c *Credential) { c.Kid = "test only" }},
		{"kid not the key's", "kid", func(c *Credential) { c.Kid = "other" }},
		{"version 2", "v", func(c *Credential) { c.V = 2 }},
		{"issued_at zero", "issued_at", func(c *Credential) { c.IssuedAt = 0; c.ExpiresAt = 60 }},
		{"expires = issued", "expires_at", func(c *Credential) { c.ExpiresAt = c.IssuedAt }},
		{"expires < issued", "expires_at", func(c *Credential) { c.ExpiresAt = c.IssuedAt - 1 }},
		{"expires at 2^53", "expires_at", func(c *Credential) { c.ExpiresAt = 1 << 53 }},
		{"TTL 601s", "expires_at", func(c *Credential) { c.ExpiresAt = c.IssuedAt + 601 }},
		{"TTL overflow", "expires_at", func(c *Credential) { c.ExpiresAt = 11790000000 }},
	}
	k := realKey(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			tc.edit(&c)
			_, err := Mint(k, c)
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Field != tc.field {
				t.Fatalf("got %v, want a refusal of field %s", err, tc.field)
			}
		})
	}
	// TTL exactly the maximum is allowed.
	c := good()
	c.ExpiresAt = c.IssuedAt + 600
	if _, err := Mint(k, c); err != nil {
		t.Fatalf("TTL 600s refused: %v", err)
	}
}

// TestMintMaxSize: no combination of valid fields reaches 4096 bytes (the
// largest is about 1.6 KB, opaque fields full of escaped quotes), so the size
// check is tested on its own, and the largest valid credential is shown to fit.
func TestMintMaxSize(t *testing.T) {
	if err := checkSize(make([]byte, MaxSize)); err != nil {
		t.Fatalf("4096 bytes refused: %v", err)
	}
	if err := checkSize(make([]byte, MaxSize+1)); err == nil {
		t.Fatal("4097 bytes accepted")
	}
	c := good()
	c.Kid = strings.Repeat("k", 64)
	c.Audience = strings.Repeat("a", 64)
	c.Amount = maxUint256.String()
	c.ObligationID = strings.Repeat(`"`, 128)
	c.PolicyVersion = strings.Repeat(`\`, 64)
	c.DecisionReason = strings.Repeat(`"`, 256)
	s, err := Mint(Key{Kid: c.Kid, Priv: realKey(t).Priv}, c)
	if err != nil || len(s.JSON) > MaxSize {
		t.Fatalf("largest valid credential: %v, %d bytes", err, len(s.JSON))
	}
}

func TestMintNoKey(t *testing.T) {
	if _, err := Mint(Key{Kid: "x"}, good()); err == nil {
		t.Fatal("minted without a key")
	}
}

// TestKeyNeverPrintsSeed: every formatting path prints only the kid.
func TestKeyNeverPrintsSeed(t *testing.T) {
	k := realKey(t)
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("minting", "key", k)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("minting", "key", k, "ptr", &k)
	j, _ := json.Marshal(struct{ K Key }{k})
	out := fmt.Sprintf("%v %+v %#v %s", k, k, k, k) + fmt.Sprint(&k) + string(j) + logs.String()
	assertNoSeed(t, out, k.Priv.Seed())
	if !strings.Contains(out, "gate-test") {
		t.Fatalf("kid missing from output: %s", out)
	}
}
