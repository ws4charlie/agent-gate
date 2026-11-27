package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGatecredKeygen runs the command path end to end: exit 0, file mode
// 0600, kid and public key printed, seed never printed, a second run refused.
func TestGatecredKeygen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "data", "gate-cred.key")
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"gatecred", "keygen", "--file", file, "--kid", "gate-cmd-test"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	st, err := os.Stat(file)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", st, err)
	}
	b, _ := os.ReadFile(file)
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatal("key file does not hold a hex seed")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	if !strings.Contains(out.String(), "kid:        gate-cmd-test\n") || !strings.Contains(out.String(), "public_key: "+pub+"\n") {
		t.Fatalf("output lacks kid or public key:\n%s", out.String())
	}
	assertNoSeedCmd(t, out.String()+errb.String(), seed)

	out.Reset()
	errb.Reset()
	if code := run(context.Background(), []string{"gatecred", "keygen", "--file", file, "--kid", "gate-cmd-test"}, &out, &errb); code == 0 {
		t.Fatal("overwrote an existing key")
	}
	if !strings.Contains(errb.String(), "refusing to overwrite") {
		t.Fatalf("stderr %q", errb.String())
	}
	assertNoSeedCmd(t, out.String()+errb.String(), seed)
	if b2, _ := os.ReadFile(file); !bytes.Equal(b, b2) {
		t.Fatal("key file changed")
	}
}

func assertNoSeedCmd(t *testing.T, out string, seed []byte) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(seed)
	for _, enc := range []string{
		hex.EncodeToString(seed), strings.ToUpper(hex.EncodeToString(seed)), hex.EncodeToString(priv),
		base64.StdEncoding.EncodeToString(seed), base64.RawURLEncoding.EncodeToString(seed),
		base64.RawStdEncoding.EncodeToString(seed), base64.RawURLEncoding.EncodeToString(priv),
	} {
		if strings.Contains(out, enc) {
			t.Fatalf("output holds the seed:\n%s", out)
		}
	}
}
