package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/gatecred"
)

// secretVolumeKey makes a key the way a Secret volume presents it: the real
// file under ..data, mode 0440, reached through a symlink.
func secretVolumeKey(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if _, err := gatecred.NewKeyFile(filepath.Join(src, "..data", "gate-cred.key")); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(src, "..data", "gate-cred.key")
	if err := os.Chmod(real, 0o440); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(src, "gate-cred.key")
	if err := os.Symlink(filepath.Join("..data", "gate-cred.key"), link); err != nil {
		t.Fatal(err)
	}
	// The gateway's loader refuses this file as it is: that is why install exists.
	if _, err := gatecred.LoadKeyFile(link, "gate-test"); err == nil {
		t.Fatal("LoadKeyFile accepted a symlinked 0440 file")
	}
	return link
}

func TestGatecredInstall(t *testing.T) {
	from := secretVolumeKey(t)
	to := filepath.Join(t.TempDir(), "gate", "key", "gate-cred.key")
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"gatecred", "install", "--from", from, "--to", to, "--kid", "gate-test"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	fi, err := os.Lstat(to)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want a regular 0600 file", fi.Mode())
	}
	if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != os.Getuid() {
		t.Fatalf("owner uid %d, want %d", st.Uid, os.Getuid())
	}
	di, err := os.Stat(filepath.Dir(to))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v (%v), want 0700", di.Mode(), err)
	}
	want, _ := os.ReadFile(from)
	got, _ := os.ReadFile(to)
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatal("installed seed differs from the source")
	}
	if strings.Contains(out.String(), strings.TrimSpace(string(want))) {
		t.Fatal("the seed was printed")
	}
	// The unchanged strict loader accepts the result.
	if _, err := gatecred.LoadKeyFile(to, "gate-test"); err != nil {
		t.Fatal(err)
	}
	// Running again (a restarted init container) replaces the file in place.
	if err := gatecredInstall(from, to); err != nil {
		t.Fatal(err)
	}
	// A symlink at the target is replaced, not written through.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(to)
	if err := os.Symlink(victim, to); err != nil {
		t.Fatal(err)
	}
	if err := gatecredInstall(from, to); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("install wrote through a symlink at the target")
	}
	if fi, _ := os.Lstat(to); !fi.Mode().IsRegular() {
		t.Fatal("target is still a symlink")
	}
}

func TestGatecredInstallRefusals(t *testing.T) {
	from := secretVolumeKey(t)

	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := gatecredInstall(from, filepath.Join(open, "gate-cred.key")); err == nil || !strings.Contains(err.Error(), "group or others can write") {
		t.Errorf("world-writable dir: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(open, "gate-cred.key")); err == nil {
		t.Error("a key was written into the world-writable dir")
	}

	bad := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(bad, []byte("not-a-seed-0123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(t.TempDir(), "k", "gate-cred.key")
	if err := gatecredInstall(bad, to); err == nil || strings.Contains(err.Error(), "not-a-seed") {
		t.Errorf("bad seed: err = %v (must fail without quoting the file)", err)
	}
	if err := gatecredInstall(filepath.Join(t.TempDir(), "missing"), to); err == nil {
		t.Error("missing source accepted")
	}
	// --kid runs the gateway's loader: a malformed kid fails the install command.
	var errb bytes.Buffer
	if code := run(context.Background(), []string{"gatecred", "install", "--from", from, "--to", to, "--kid", "bad kid!"}, io.Discard, &errb); code == 0 {
		t.Error("install with an invalid kid succeeded")
	}
	if code := run(context.Background(), []string{"gatecred", "install", "--to", to}, io.Discard, &errb); code == 0 {
		t.Error("install without --from succeeded")
	}
}
