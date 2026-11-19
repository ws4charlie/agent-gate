package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/policy"
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

func TestServeCedarEnforceNeedsPolicies(t *testing.T) {
	newCLI(t) // a database, so the failure below is the flag check and not a missing database
	t.Setenv("AG_CEDAR_POLICIES", "")
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"serve", "--cedar-enforce"}, &out, &errb); code == 0 ||
		!strings.Contains(errb.String(), "--cedar-enforce needs --cedar-policies") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// `cedar shadow` compares Cedar with the Go engine in both modes: shadow rows
// carry Go's answer in decision/rule, enforce rows in go_verdict/go_rule.
func TestCedarShadowReadsBothModes(t *testing.T) {
	c := newCLI(t)
	if _, err := c.run("agent", "create", "--account", c.acct, "--name", "bot"); err != nil {
		t.Fatal(err)
	}
	ags, err := c.st.List(context.Background())
	if err != nil || len(ags) != 1 {
		t.Fatalf("%v %v", ags, err)
	}
	log := func(d agents.Decision) {
		t.Helper()
		d.AgentID, d.AccountID, d.Kind, d.Subject = ags[0].ID, c.acct, "model", "m"
		d.RequestID = uuid.NewString()
		if err := c.st.LogDecision(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	log(agents.Decision{Verdict: policy.Allow, Rule: "allow", CedarVerdict: "allow", CedarRule: "allow"})                                                          // shadow, agree
	log(agents.Decision{Verdict: policy.Allow, Rule: "allow", CedarVerdict: "deny", CedarRule: "scope_model"})                                                     // shadow, differ
	log(agents.Decision{Verdict: policy.Deny, Rule: "scope_model", CedarVerdict: "deny", CedarRule: "scope_model", GoVerdict: policy.Deny, GoRule: "scope_model"}) // enforce, agree
	log(agents.Decision{Verdict: policy.Deny, Rule: "scope_model", CedarVerdict: "deny", CedarRule: "scope_model", GoVerdict: policy.Allow, GoRule: "allow"})      // enforce, differ
	log(agents.Decision{Verdict: policy.Deny, Rule: "cedar_error", CedarVerdict: "error", GoVerdict: policy.Allow, GoRule: "allow"})                               // enforce, error
	out, err := c.run("cedar", "shadow")
	if err != nil || !strings.Contains(out, "5 decisions with Cedar on, 2 agree, 2 differ, 1 cedar errors") {
		t.Fatalf("%q %v", out, err)
	}
}
