package gatecred

// Tests for the PR #93 review findings (T17-fix2). They use only the exported
// API, so each one also runs against the code before the fix.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedForms is every way a formatting path could print the seed or the
// private key: raw, hex, base64, a decimal list (%v of []byte) and a Go
// literal (%#v of []byte).
func seedForms(seed []byte) []string {
	priv := ed25519.NewKeyFromSeed(seed)
	forms := []string{string(seed[:8])}
	for _, b := range [][]byte{seed, priv} {
		dec := strings.Trim(fmt.Sprint([]byte(b[:8])), "[]")
		lit := strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%#v", []byte(b[:8])), "[]byte{"), "}")
		forms = append(forms, dec, lit, hex.EncodeToString(b[:8]), strings.ToUpper(hex.EncodeToString(b[:8])))
	}
	return forms
}

func assertNoSeedForms(t *testing.T, label, out string, seed []byte) {
	t.Helper()
	for _, f := range seedForms(seed) {
		if strings.Contains(out, f) {
			t.Errorf("%s prints key bytes (%q):\n%s", label, f, out)
			return
		}
	}
}

// Review item 1: a Key inside another struct, as an unexported field, is
// printed by reflection (fmt cannot call its methods). It must show no key
// bytes in any form.
func TestKeyInsideStructNeverPrintsSeed(t *testing.T) {
	seed := sha256.Sum256([]byte("embedded " + t.Name()))
	k, err := NewKey("gate-embed", ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	type holder struct {
		name string
		k    Key
		p    *Key
	}
	type exported struct {
		Name string
		K    Key
		P    *Key
		k    Key
	}
	h := holder{name: "minter", k: k, p: &k}
	e := exported{Name: "minter", K: k, P: &k, k: k}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x"} {
		for _, arg := range []any{h, &h, e, &e, []holder{h}, map[string]holder{"a": h}} {
			assertNoSeedForms(t, fmt.Sprintf("Sprintf(%q, %T)", verb, arg), fmt.Sprintf(verb, arg), seed[:])
		}
	}
	j, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSeedForms(t, "json", string(j), seed[:])
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "h", h, "hp", &h, "e", e)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "h", h, "hp", &h, "e", e)
	assertNoSeedForms(t, "slog", logs.String(), seed[:])
}

// Review item 3: NewKey must judge a key by the public half it derives from
// the seed, not by the half the caller passed. A test seed with a forged
// public half is still a test key.
func TestNewKeyRefusesTestSeedWithForgedPublicHalf(t *testing.T) {
	for _, s := range testSeedStrings {
		priv := ed25519.NewKeyFromSeed(testSeed(s))
		forged := append(ed25519.PrivateKey(nil), priv...)
		other := ed25519.NewKeyFromSeed(testSeed("not a test key"))
		copy(forged[32:], other[32:])
		if _, err := NewKey("gate-1", forged); err == nil {
			t.Errorf("%s with a forged public half accepted", s)
		}
	}
	// A real seed with a mismatched public half is refused too: it would sign
	// with one key and claim another.
	seed := sha256.Sum256([]byte("real " + t.Name()))
	priv := ed25519.NewKeyFromSeed(seed[:])
	bad := append(ed25519.PrivateKey(nil), priv...)
	bad[40] ^= 1
	if _, err := NewKey("gate-1", bad); err == nil {
		t.Error("mismatched public half accepted")
	}
}

// Review item 2: key file checks.
func TestLoadKeyFileRefusesSymlink(t *testing.T) {
	seed := sha256.Sum256([]byte("link " + t.Name()))
	target := writeSeed(t, seed[:], 0o600)
	link := filepath.Join(t.TempDir(), "gate-cred.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(link, "gate-1"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink: got %v, want a refusal naming the symlink", err)
	}
}

func TestLoadKeyFileRefusesWritableDir(t *testing.T) {
	seed := sha256.Sum256([]byte("dir " + t.Name()))
	for _, mode := range []os.FileMode{0o770, 0o757, 0o777} {
		p := writeSeed(t, seed[:], 0o600)
		if err := os.Chmod(filepath.Dir(p), mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKeyFile(p, "gate-1")
		os.Chmod(filepath.Dir(p), 0o700)
		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Errorf("dir mode %04o: got %v, want a refusal naming the directory", mode, err)
		}
	}
	// A readable but not writable directory is fine (the file itself is 0600).
	p := writeSeed(t, seed[:], 0o600)
	os.Chmod(filepath.Dir(p), 0o755)
	defer os.Chmod(filepath.Dir(p), 0o700)
	if _, err := LoadKeyFile(p, "gate-1"); err != nil {
		t.Fatalf("0755 dir refused: %v", err)
	}
}

func TestKeygenRefusesWritableDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o777)
	defer os.Chmod(dir, 0o700)
	var out strings.Builder
	p := filepath.Join(dir, "gate-cred.key")
	if err := Keygen(&out, p, "gate-1"); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("got %v, want a refusal naming the directory", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("key written into a world-writable directory")
	}
}

func TestKeygenWarnsReadableDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	var out strings.Builder
	if err := Keygen(&out, filepath.Join(dir, "gate-cred.key"), "gate-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0755") {
		t.Fatalf("no note about the 0755 directory:\n%s", out.String())
	}
}

// Review item 2: a key file owned by another user is refused. Changing a
// file's owner needs root, so the test moves "this user" instead.
func TestLoadKeyFileRefusesOtherOwner(t *testing.T) {
	seed := sha256.Sum256([]byte("owner " + t.Name()))
	p := writeSeed(t, seed[:], 0o600)
	if _, err := LoadKeyFile(p, "gate-1"); err != nil {
		t.Fatalf("own file refused: %v", err)
	}
	old := currentUID
	currentUID = func() int { return old() + 1 }
	defer func() { currentUID = old }()
	if _, err := LoadKeyFile(p, "gate-1"); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("got %v, want a refusal naming the owner", err)
	}
}
