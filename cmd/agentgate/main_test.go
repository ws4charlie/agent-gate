package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "agentgate ") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"nope"}, &out, &errb); code != 2 {
		t.Fatalf("code=%d", code)
	}
}

// `wallet new` prints the address only; the key stays in the 0600 file.
func TestWalletNewPrintsAddressOnly(t *testing.T) {
	file := t.TempDir() + "/w.key"
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"wallet", "new", "--file", file}, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	key, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimSpace(out.String())
	if !strings.HasPrefix(addr, "0x") || len(addr) != 42 || strings.Contains(out.String()+errb.String(), strings.TrimSpace(string(key))) {
		t.Fatalf("output %q", out.String())
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	// A second run must not overwrite the key.
	if code := run(context.Background(), []string{"wallet", "new", "--file", file}, &out, &errb); code == 0 {
		t.Fatal("overwrote an existing key")
	}
}
