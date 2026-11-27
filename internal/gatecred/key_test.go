package gatecred

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertNoSeed fails if out holds the seed or private key in any common
// encoding.
func assertNoSeed(t *testing.T, out string, seed []byte) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(seed)
	for _, enc := range []string{
		hex.EncodeToString(seed), strings.ToUpper(hex.EncodeToString(seed)),
		base64.StdEncoding.EncodeToString(seed), base64.RawURLEncoding.EncodeToString(seed),
		hex.EncodeToString(priv), base64.RawURLEncoding.EncodeToString(priv), base64.StdEncoding.EncodeToString(priv),
		string(seed),
	} {
		if strings.Contains(out, enc) {
			t.Fatalf("output holds the seed or private key:\n%s", out)
		}
	}
}

func writeSeed(t *testing.T, seed []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gate-cred.key")
	if err := os.WriteFile(p, []byte(hex.EncodeToString(seed)+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask may have narrowed it
		t.Fatal(err)
	}
	return p
}

func TestLoadKey(t *testing.T) {
	seed := sha256.Sum256([]byte("a real-looking key for " + t.Name()))
	p := writeSeed(t, seed[:], 0o600)
	t.Setenv(EnvKeyFile, p)
	t.Setenv(EnvKid, "gate-2026-11")
	k, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if k.Kid != "gate-2026-11" || !k.Public().Equal(ed25519.NewKeyFromSeed(seed[:]).Public()) {
		t.Fatalf("wrong key: %v", k)
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(); err != nil {
		t.Fatalf("0400 refused: %v", err)
	}
}

func TestLoadKeyRefuses(t *testing.T) {
	seed := sha256.Sum256([]byte("a real-looking key for " + t.Name()))
	ok := writeSeed(t, seed[:], 0o600)
	for _, tc := range []struct {
		name, path, kid, want string
	}{
		{"missing kid", ok, "", "no kid"},
		{"bad kid", ok, "gate key", "AG_GATE_CRED_KID"},
		{"group-readable", writeSeed(t, seed[:], 0o640), "gate-1", "mode 0640"},
		{"world-readable", writeSeed(t, seed[:], 0o644), "gate-1", "mode 0644"},
		{"group-writable", writeSeed(t, seed[:], 0o620), "gate-1", "mode 0620"},
		{"missing file", filepath.Join(t.TempDir(), "nope.key"), "gate-1", "no key file"},
		{"short seed", writeSeed(t, seed[:31], 0o600), "gate-1", "32-byte seed"},
		{"test key 1", writeSeed(t, testSeed("TEST-ONLY-qv-gate-credential-key-1"), 0o600), "gate-1", "TEST ONLY"},
		{"test key 2", writeSeed(t, testSeed("TEST-ONLY-qv-gate-credential-key-2"), 0o600), "gate-1", "TEST ONLY"},
		{"test key 2 under its own kid", writeSeed(t, testSeed("TEST-ONLY-qv-gate-credential-key-2"), 0o600), "test-only-2", "TEST ONLY"},
		{"test key 3", writeSeed(t, testSeed("TEST-ONLY-qv-gate-credential-key-3"), 0o600), "gate-1", "TEST ONLY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvKeyFile, tc.path)
			t.Setenv(EnvKid, tc.kid)
			_, err := LoadKey()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	// A bad file's error never quotes its content.
	p := filepath.Join(t.TempDir(), "k")
	os.WriteFile(p, []byte("not-hex-"+hex.EncodeToString(seed[:])), 0o600)
	_, err := LoadKeyFile(p, "gate-1")
	if err == nil {
		t.Fatal("accepted a bad file")
	}
	assertNoSeed(t, err.Error(), seed[:])
}

func testSeed(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// TestTestKeysAreTheVectors: the refusal list derives exactly the vectors'
// two listed public keys.
func TestTestKeysAreTheVectors(t *testing.T) {
	v := loadVectors(t)
	for _, k := range v.Config.Keys {
		pub, _ := base64.RawURLEncoding.DecodeString(k.PublicKey)
		if !IsTestKey(pub) {
			t.Fatalf("%s is not refused", k.Kid)
		}
	}
	if IsTestKey(ed25519.NewKeyFromSeed(testSeed("anything else")).Public().(ed25519.PublicKey)) {
		t.Fatal("a non-test key is refused")
	}
}

func TestNewKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "gate-cred.key")
	pub, err := NewKeyFile(p)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", fi.Mode(), err)
	}
	k, err := LoadKeyFile(p, "gate-1")
	if err != nil || !k.Public().Equal(pub) {
		t.Fatalf("written key does not load back: %v", err)
	}
	before, _ := os.ReadFile(p)
	if _, err := NewKeyFile(p); err == nil {
		t.Fatal("overwrote an existing key")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("key file changed")
	}
}

// TestKeygen: prints the kid and the public key, never the seed; refuses to
// overwrite; a bad kid creates no file.
func TestKeygen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "data", "gate-cred.key")
	var out strings.Builder
	if err := Keygen(&out, p, "gate-20261126"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != 32 {
		t.Fatalf("file does not hold a hex seed")
	}
	assertNoSeed(t, out.String(), seed)
	pub := base64.RawURLEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	if !strings.Contains(out.String(), "kid:        gate-20261126\n") || !strings.Contains(out.String(), "public_key: "+pub+"\n") {
		t.Fatalf("output lacks kid or public key:\n%s", out.String())
	}
	if dec, err := base64.RawURLEncoding.Strict().DecodeString(pub); err != nil || len(dec) != 32 || strings.ContainsAny(pub, "=+/") {
		t.Fatalf("public key not strict base64url of 32 bytes: %q", pub)
	}
	var again strings.Builder
	err = Keygen(&again, p, "gate-20261126")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("overwrite: %v", err)
	}
	assertNoSeed(t, again.String()+err.Error(), seed)
	if b2, _ := os.ReadFile(p); string(b2) != string(b) {
		t.Fatal("key file changed")
	}
	q := filepath.Join(t.TempDir(), "k")
	if err := Keygen(&again, q, "bad kid"); err == nil {
		t.Fatal("bad kid accepted")
	}
	if _, err := os.Stat(q); !os.IsNotExist(err) {
		t.Fatal("bad kid created a file")
	}
}
