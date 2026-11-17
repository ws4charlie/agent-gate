package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCedarCheck(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"cedar", "check", "../../policies/cedar"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errb.String())
	}
}

// Editing one rule's threshold in a copy must make the check fail.
func TestCedarCheckCatchesEdit(t *testing.T) {
	src, err := os.ReadFile("../../policies/cedar/agentgate.cedar")
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(src), "context.amount > principal.approval_threshold", "context.amount >= principal.approval_threshold", 1)
	if edited == string(src) {
		t.Fatal("edit did not apply")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agentgate.cedar"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"cedar", "check", dir}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1: %s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "DIFF") {
		t.Fatalf("no DIFF printed: %s", out.String())
	}
}
