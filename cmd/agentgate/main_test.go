package main

import (
	"bytes"
	"context"
	"io"
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

func TestBankSimulateReturnRefusedOutsideSandbox(t *testing.T) {
	err := bankCmd(context.Background(), []string{"simulate-return", "bp_x", "--mercury-base-url", "https://api.mercury.com/api/v1/"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not the sandbox") {
		t.Fatalf("want a refusal, got %v", err)
	}
}
