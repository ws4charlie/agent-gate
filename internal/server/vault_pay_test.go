package server_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/gatecred"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/server"
	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/vaultsign/vaulttest"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// vaultRig is the payment rig with the QuorumVault signer: the gateway signs through
// the fake QuorumVault, whose wallet holds the test USDC on the simulated chain.
type vaultRig struct {
	*payEnv
	vault   *vaulttest.Vault
	vs      *wallet.VaultSigner
	credPub ed25519.PublicKey
}

const vaultAudience = "qv-test"

// newVaultRig builds the rig; opts may change the vaultsign client's config.
func newVaultRig(t *testing.T, opts ...func(*vaultsign.Config, *vaulttest.Vault)) *vaultRig {
	t.Helper()
	f := vaulttest.New(t, vaulttest.Options{})
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := gatecred.NewKey("gate-test-1", priv)
	if err != nil {
		t.Fatal(err)
	}
	r := &vaultRig{vault: f, credPub: pub}
	r.payEnv = newPayEnvWith(t, 10*usd, func(pe *payEnv) wallet.Signer {
		if err := pe.sim.Mint(context.Background(), f.From, big.NewInt(5_000_000)); err != nil {
			t.Fatal(err)
		}
		cfg := vaultsign.Config{BaseURL: f.URL, WalletID: f.WalletID,
			ClientCert: f.ClientCert, ClientKey: f.ClientKey, CAFile: f.CAFile,
			Token: &vaultsign.ClientCredentials{TokenURL: f.TokenURL, ClientID: vaulttest.ClientID,
				SecretFile: f.SecretFile, HTTPClient: f.HTTPClient()},
			Timeout: 2 * time.Second, RetryFor: 5 * time.Second}
		for _, o := range opts {
			o(&cfg, f)
		}
		cli, err := vaultsign.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		r.vs, err = wallet.NewVaultSigner(wallet.VaultConfig{Client: cli, CredKey: key, Audience: vaultAudience,
			WalletID: f.WalletID, From: f.From, ChainID: x402.LocalChainID})
		if err != nil {
			t.Fatal(err)
		}
		return r.vs
	})
	return r
}

// sent is one signing request the fake Vault received, its credential
// checked against Gate's credential key.
type sent struct {
	cred    gatecred.Credential
	message map[string]string // the EIP-712 message: from, to, value, validAfter, validBefore, nonce
}

func (r *vaultRig) requests() []sent {
	r.t.Helper()
	var out []sent
	for _, b := range r.vault.Bodies() {
		var req struct {
			Payload struct {
				Message map[string]string `json:"message"`
			} `json:"payload"`
			Credential map[string]json.RawMessage `json:"credential"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			r.t.Fatalf("body: %v", err)
		}
		out = append(out, sent{cred: r.verify(req.Credential), message: req.Payload.Message})
	}
	return out
}

// verify rebuilds the credential and checks its sig the way Vault does:
// ed25519 over the domain prefix and the canonical form.
func (r *vaultRig) verify(m map[string]json.RawMessage) gatecred.Credential {
	r.t.Helper()
	str := func(k string) string { var s string; _ = json.Unmarshal(m[k], &s); return s }
	num := func(k string) int64 { var n int64; _ = json.Unmarshal(m[k], &n); return n }
	c := gatecred.Credential{V: int(num("v")), Kid: str("kid"), CredentialID: str("credential_id"), ObligationID: str("obligation_id"),
		Audience: str("audience"), WalletID: str("wallet_id"), Chain: str("chain"), Asset: str("asset"), Payee: str("payee"),
		Amount: str("amount"), IssuedAt: num("issued_at"), ExpiresAt: num("expires_at"),
		PolicyVersion: str("policy_version"), DecisionReason: str("decision_reason")}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(str("sig"))
	if err != nil || !ed25519.Verify(r.credPub, gatecred.SignedBytes(c), sig) {
		r.r().Fatalf("credential sig does not verify: %v", err)
	}
	return c
}

func (r *vaultRig) r() *testing.T { return r.t }

// row is the request's one audit row, which carries its Vault attempt.
func (r *vaultRig) row(rid string) audit.Row {
	r.t.Helper()
	row := r.auditRowID(rid) // exactly one row: the Vault attempt is not another call
	if row.Vault == nil {
		r.t.Fatalf("no vault fields on %+v", row)
	}
	return row
}

// AC2: an allowed payment is signed by Vault, settles as before, and the
// one credential Vault saw verifies and names exactly this payment.
func TestVaultPaymentSigned(t *testing.T) {
	r := newVaultRig(t)
	code, m, rid := r.pay("echo", "v-1")
	if code != 200 || m["transaction"] == "" || !strings.Contains(string(mustJSON(m["result"])), "HELLO") {
		t.Fatalf("%d %v", code, m)
	}
	if r.chainBal(r.payTo) != 10_000 || r.chainBal(r.vault.From) != 5_000_000-10_000 {
		t.Fatalf("chain: payee %d vault %d", r.chainBal(r.payTo), r.chainBal(r.vault.From))
	}
	if r.w.SignCount() != 0 || r.vault.Signs() != 1 || r.vs.Attempts() != 1 {
		t.Fatalf("local signs %d, vault signs %d, attempts %d", r.w.SignCount(), r.vault.Signs(), r.vs.Attempts())
	}
	reqs := r.requests()
	if len(reqs) != 1 {
		t.Fatalf("vault saw %d requests", len(reqs))
	}
	c, msg := reqs[0].cred, reqs[0].message
	v := r.row(rid)
	if v.Status != audit.StatusOK || v.Kind != "payment" {
		t.Fatalf("audit %+v", v)
	}
	hold := v.HoldID
	want := gatecred.Credential{V: 1, Kid: "gate-test-1", CredentialID: c.CredentialID, ObligationID: hold, Audience: vaultAudience,
		WalletID: r.vault.WalletID, Chain: x402.NetworkLocal, Asset: strings.ToLower(r.sim.Token.Hex()),
		Payee: strings.ToLower(r.payTo.Hex()), Amount: "10000", IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt,
		PolicyVersion: server.DefaultPolicyVersion, DecisionReason: "allow"}
	if c != want {
		t.Fatalf("credential\n got %+v\nwant %+v", c, want)
	}
	// Starts 10 s back (as the local wallet), ends at the paid tool's
	// maxTimeoutSeconds (60 s) from now, which is sooner than issued_at + 90 s.
	if c.ExpiresAt-c.IssuedAt != 70 {
		t.Fatalf("window %d s", c.ExpiresAt-c.IssuedAt)
	}
	nonce := x402.PaymentNonce(hold)
	if !strings.EqualFold(msg["from"], r.vault.From.Hex()) || !strings.EqualFold(msg["to"], r.payTo.Hex()) || msg["value"] != "10000" ||
		msg["validAfter"] != itoa(c.IssuedAt) || msg["validBefore"] != itoa(c.ExpiresAt) || !strings.EqualFold(msg["nonce"], common.Hash(nonce).Hex()) {
		t.Fatalf("authorization %v vs credential %+v", msg, c)
	}
	if *v.Vault != (audit.Vault{CredentialID: c.CredentialID, WalletID: r.vault.WalletID, Payee: c.Payee, Amount: "10000", Outcome: wallet.OutcomeSigned}) {
		t.Fatalf("vault fields %+v", v.Vault)
	}
	if got := strings.Join(r.entryKinds(), ","); got != "hold,settle" {
		t.Fatalf("ledger %s", got)
	}
	r.noActiveHolds()
}

func itoa(n int64) string { return big.NewInt(n).String() }

// AC3, I10 with Vault: deny, approval needed and budget exceeded never reach Vault.
func TestVaultNotAskedWhenPolicyRefuses(t *testing.T) {
	cases := []struct {
		name  string
		scope func(*policy.Scope, common.Address)
		code  int
	}{
		{"payee not allowed", func(s *policy.Scope, _ common.Address) {
			s.Payees = []string{"0x000000000000000000000000000000000000dEaD"}
		}, 403},
		{"approval needed", func(s *policy.Scope, p common.Address) { s.Payees = []string{p.Hex()}; s.ApprovalThreshold = 5_000_000 }, 202},
		{"daily budget", func(s *policy.Scope, p common.Address) { s.Payees = []string{p.Hex()}; s.DailyBudget = 5_000_000 }, 429},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newVaultRig(t)
			r.setScope(func(s *policy.Scope) { tc.scope(s, r.payTo) })
			code, m, _ := r.pay("echo", "v-deny")
			if code != tc.code {
				t.Fatalf("%d %v", code, m)
			}
			if n := len(r.vault.Bodies()); n != 0 || r.vs.Attempts() != 0 || r.vault.TokenFetches() != 0 {
				t.Fatalf("vault saw %d requests (%d attempts, %d token fetches)", n, r.vs.Attempts(), r.vault.TokenFetches())
			}
			if r.chainBal(r.payTo) != 0 {
				t.Fatal("money moved")
			}
		})
	}
}

// AC4: every refusal class fails the payment with Vault's code, releases the
// hold and is audited; the agent's retry is a new hold, a new nonce and a new
// credential.
func TestVaultRefusalReleasesAndRetryIsNew(t *testing.T) {
	cases := []struct {
		status      int
		code, field string
	}{
		{403, vaultsign.CodeFastLaneCap, "amount"},
		{403, vaultsign.CodePaymentMismatch, "payee"},
		{409, vaultsign.CodeNonceReused, ""},
		{503, vaultsign.CodeSigningFailed, ""},
		{403, vaultsign.CodeWalletNotGateGoverned, ""},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			r := newVaultRig(t)
			r.vault.Script(vaulttest.Step{Status: tc.status, Code: tc.code, Field: tc.field})
			code, body, rid := r.pay("echo", "v-ref-1")
			m, _ := body["error"].(map[string]any)
			if code != 403 || m["type"] != server.CodeVaultRefused || m["code"] != tc.code || m["field"] != tc.field {
				t.Fatalf("%d %v", code, body)
			}
			if tc.code == vaultsign.CodeFastLaneCap && !strings.Contains(string(mustJSON(m)), "fast-lane limit") {
				t.Fatalf("fast_lane_cap must say so plainly: %v", m)
			}
			if r.activeHolds() != 0 || r.holdStatus() != "released" {
				t.Fatalf("hold %s, %d active", r.holdStatus(), r.activeHolds())
			}
			if b := r.balance(); b.Held != 0 || b.Balance != 10*usd {
				t.Fatalf("balance %+v", b)
			}
			final := r.row(rid)
			if final.Status != server.StatusPaymentFailed || final.Err != server.CodeVaultRefused || final.Vault.Code != tc.code ||
				final.Vault.Field != tc.field || final.Vault.Outcome != wallet.OutcomeRefused || final.Vault.CredentialID == "" ||
				final.Vault.CredentialID != m["credential_id"] {
				t.Fatalf("audit %+v %+v", final, final.Vault)
			}

			code, m, rid2 := r.pay("echo", "v-ref-2") // the agent tries again: the fake signs this time
			if code != 200 {
				t.Fatalf("retry %d %v", code, m)
			}
			reqs := r.requests()
			if len(reqs) != 2 {
				t.Fatalf("vault saw %d requests", len(reqs))
			}
			first, second := reqs[0], reqs[1]
			final2 := r.row(rid2)
			if first.cred.ObligationID != final.HoldID || second.cred.ObligationID != final2.HoldID || final.HoldID == final2.HoldID {
				t.Fatalf("holds %s %s / credentials %s %s", final.HoldID, final2.HoldID, first.cred.ObligationID, second.cred.ObligationID)
			}
			if first.message["nonce"] == second.message["nonce"] || first.cred.CredentialID == second.cred.CredentialID {
				t.Fatal("retry reused the nonce or the credential")
			}
			if r.chainBal(r.payTo) != 10_000 {
				t.Fatalf("payee %d", r.chainBal(r.payTo))
			}
		})
	}
}

// AC5: retry_later twice, then a signature: one payment, one signature, one
// hold settled, and Vault saw the identical request three times.
func TestVaultRetryLaterThenSigned(t *testing.T) {
	r := newVaultRig(t)
	r.vault.Script(vaulttest.Step{Status: 503, Code: vaultsign.CodeRetryLater}, vaulttest.Step{Status: 503, Code: vaultsign.CodeRetryLater})
	code, m, rid := r.pay("echo", "v-later")
	if code != 200 {
		t.Fatalf("%d %v", code, m)
	}
	bodies := r.vault.Bodies()
	if len(bodies) != 3 || string(bodies[0]) != string(bodies[2]) || r.vault.Signs() != 1 || r.vs.Attempts() != 1 {
		t.Fatalf("bodies %d, signs %d, attempts %d", len(bodies), r.vault.Signs(), r.vs.Attempts())
	}
	if v := r.row(rid); v.Status != audit.StatusOK || v.Vault.Outcome != wallet.OutcomeSigned {
		t.Fatalf("audit %+v %+v", v, v.Vault)
	}
	if got := strings.Join(r.entryKinds(), ","); got != "hold,settle" || r.chainBal(r.payTo) != 10_000 {
		t.Fatalf("ledger %s payee %d", got, r.chainBal(r.payTo))
	}
	r.noActiveHolds()
}

// AC6: a 200 whose signature does not verify keeps the hold (Vault may have
// signed): the reconciler releases it only after validBefore plus the margin.
func TestVaultBadSignatureKeepsHoldUntilWindowCloses(t *testing.T) {
	r := newVaultRig(t)
	r.vault.Script(vaulttest.Step{Mutate: func(sig []byte) []byte { sig[5] ^= 0xff; return sig }})
	code, m, rid := r.pay("echo", "v-bad")
	if code != 202 || m["status"] != "settlement_pending" || m["reason"] != "vault_bad_signature" {
		t.Fatalf("%d %v", code, m)
	}
	if v := r.row(rid); v.Vault.Outcome != wallet.OutcomeBadSignature || v.Status != server.StatusSettlementPending {
		t.Fatalf("audit %+v %+v", v, v.Vault)
	}
	if r.activeHolds() != 1 {
		t.Fatal("hold released while the nonce may be spent")
	}
	c := r.requests()[0].cred
	validBefore := time.Unix(c.ExpiresAt, 0)
	// Hold expired, but the authorization could still execute: stays.
	if rep := r.reconcile(time.Now().Add(time.Hour), validBefore); rep.Released+rep.Settled != 0 || r.activeHolds() != 1 {
		t.Fatalf("closed inside the window: %+v", rep)
	}
	rep := r.reconcile(time.Now().Add(time.Hour), validBefore.Add(x402.DefaultAuthMargin+time.Second))
	if rep.Released != 1 || rep.Settled != 0 {
		t.Fatalf("report %+v", rep)
	}
	if b := r.balance(); b.Held != 0 || b.Balance != 10*usd {
		t.Fatalf("balance %+v", b)
	}
	r.noActiveHolds()
}

// An approved payment tells Vault who approved it.
func TestVaultApprovedPaymentNamesApprover(t *testing.T) {
	r := newVaultRig(t)
	r.setScope(func(s *policy.Scope) { s.Payees = []string{r.payTo.Hex()}; s.ApprovalThreshold = 5_000_000 })
	_, m, _ := r.pay("echo", "v-appr")
	id, _ := m["approval_id"].(string)
	if id == "" || len(r.vault.Bodies()) != 0 {
		t.Fatalf("%v, vault bodies %d", m, len(r.vault.Bodies()))
	}
	if _, err := r.srv.Approvals.Allow(context.Background(), id, "alice"); err != nil {
		t.Fatal(err)
	}
	if code, m, _ := r.pay("echo", "v-appr"); code != 200 {
		t.Fatalf("%d %v", code, m)
	}
	if reqs := r.requests(); len(reqs) != 1 || reqs[0].cred.DecisionReason != "approved_by:alice" {
		t.Fatalf("requests %+v", reqs)
	}
}

// The paid tool's window is too short for a credential and a Vault call:
// refused before Vault, hold released.
func TestVaultWindowTooShort(t *testing.T) {
	r := newVaultRig(t)
	pl, _ := r.srv.Payments.Wallet.(*wallet.VaultSigner)
	if pl == nil {
		t.Fatal("not the vault signer")
	}
	// The rig's paid tool asks for 60 s; a 15 s minimum passes, so check the
	// signer refuses a shorter one directly (Prepare is shared with the route).
	req := x402.Requirements{Network: x402.NetworkLocal, Amount: "10000", Asset: r.sim.Token.Hex(), PayTo: r.payTo.Hex(), MaxTimeoutSeconds: 10}
	d := policy.Evaluate(policy.Request{AgentID: "a", Kind: policy.KindPayment, Tool: "t", Payee: r.payTo.Hex(), Network: x402.NetworkLocal, Amount: 10_000_000, HoldID: "h"},
		policy.Snapshot{AgentID: "a", Status: policy.StatusActive, Scope: policy.Scope{Tools: []string{"*"}, Payees: []string{r.payTo.Hex()}}})
	a, ok := d.Allowed()
	if !ok {
		t.Fatal(d.Rule)
	}
	if _, err := pl.Prepare(context.Background(), a, "h", x402.Resource{}, req); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("got %v", err)
	}
	req.MaxTimeoutSeconds = 15 // exactly the minimum passes, so the refusal did not spend the approval
	if _, err := pl.Prepare(context.Background(), a, "h", x402.Resource{}, req); err != nil {
		t.Fatalf("approval burned: %v", err)
	}
}

// fix1 #2: whatever the approver's name, an approved payment still gets a
// decision_reason Mint accepts.
func TestApprovedByIsAlwaysAValidReason(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := gatecred.NewKey("gate-test-1", priv)
	for _, name := range []string{"alice", "ali\nce", "a\x00b", "a\xffb\xfe c", "\t\r ", "", strings.Repeat("é", 150), strings.Repeat("x", 300)} {
		reason := server.ApprovedBy(name)
		if len(reason) > 256 || !strings.HasPrefix(reason, "approved_by:") || !utf8.ValidString(reason) {
			t.Errorf("%q -> %q", name, reason)
		}
		_, err := gatecred.Mint(key, gatecred.Credential{ObligationID: "h", Audience: "qv-test", WalletID: "6f1c2b3a-0d4e-4f5a-8b6c-7d8e9f0a1b2c",
			Chain: "eip155:1337", Asset: "0x5fbdb2315678afecb367f032d93f642f64180aa3", Payee: "0x70997970c51812dc3a010c7d01b50e0d17dc79c8",
			Amount: "1", IssuedAt: 1_800_000_000, ExpiresAt: 1_800_000_060, PolicyVersion: "go1", DecisionReason: reason})
		if err != nil {
			t.Errorf("%q -> %q: Mint refuses: %v", name, reason, err)
		}
	}
	if got := server.ApprovedBy("\x01\x02"); got != "approved_by:__" {
		t.Errorf("control chars: %q", got)
	}
	if got := server.ApprovedBy(" "); got != "approved_by:unknown" {
		t.Errorf("empty: %q", got)
	}
}

// fix1 #2 end to end: an approver name with a newline (what Postgres will
// store) still pays after approval.
func TestVaultApproverWithControlCharPays(t *testing.T) {
	r := newVaultRig(t)
	r.setScope(func(s *policy.Scope) { s.Payees = []string{r.payTo.Hex()}; s.ApprovalThreshold = 5_000_000 })
	_, m, _ := r.pay("echo", "v-appr-nl")
	id, _ := m["approval_id"].(string)
	if _, err := r.srv.Approvals.Allow(context.Background(), id, "ali\nce"+strings.Repeat("x", 300)); err != nil {
		t.Fatal(err)
	}
	if code, m, _ := r.pay("echo", "v-appr-nl"); code != 200 {
		t.Fatalf("%d %v", code, m)
	}
	if reqs := r.requests(); len(reqs) != 1 || !strings.HasPrefix(reqs[0].cred.DecisionReason, "approved_by:ali_ce") || len(reqs[0].cred.DecisionReason) > 256 {
		t.Fatalf("requests %+v", reqs)
	}
}

// fix1 #3: Vault never decided. The signature cannot have left Vault, so the
// hold is released and the agent gets 502 vault_unavailable; a configuration
// failure (TLS, token) is also logged at ERROR for the operator.
func TestVaultUnavailableReleases(t *testing.T) {
	cases := []struct {
		name     string
		opts     []func(*vaultsign.Config, *vaulttest.Vault)
		setup    func(r *vaultRig)
		cancel   bool // the client gives up while Vault holds the answer
		errorLog bool
	}{
		{name: "retries exhausted", opts: []func(*vaultsign.Config, *vaulttest.Vault){func(c *vaultsign.Config, _ *vaulttest.Vault) { c.RetryFor = 600 * time.Millisecond }},
			setup: func(r *vaultRig) {
				steps := make([]vaulttest.Step, 20)
				for i := range steps {
					steps[i] = vaulttest.Step{Status: 503, Code: vaultsign.CodeRetryLater}
				}
				r.vault.Script(steps...)
			}},
		{name: "expired", setup: func(r *vaultRig) { r.vs.SetClock(func() time.Time { return time.Now().Add(-5 * time.Minute) }) }},
		{name: "client cancel", cancel: true, setup: func(r *vaultRig) { r.vault.Script(vaulttest.Step{Delay: 3 * time.Second}) }},
		{name: "tls", errorLog: true, opts: []func(*vaultsign.Config, *vaulttest.Vault){func(c *vaultsign.Config, _ *vaulttest.Vault) {
			c.CAFile = vaulttest.New(t, vaulttest.Options{}).CAFile // trusts another CA: Vault's certificate fails
		}}},
		{name: "token", errorLog: true, opts: []func(*vaultsign.Config, *vaulttest.Vault){func(c *vaultsign.Config, f *vaulttest.Vault) {
			bad := filepath.Join(t.TempDir(), "secret")
			_ = os.WriteFile(bad, []byte("wrong-secret\n"), 0o600)
			c.Token = &vaultsign.ClientCredentials{TokenURL: f.TokenURL, ClientID: vaulttest.ClientID, SecretFile: bad, HTTPClient: f.HTTPClient()}
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newVaultRig(t, tc.opts...)
			if tc.setup != nil {
				tc.setup(r)
			}
			var rid string
			if tc.cancel {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				resp, err := r.doCtx(ctx, "/v1/payments", `{"tool":"echo","input":{"text":"hello"}}`, map[string]string{"Idempotency-Key": "v-un"})
				cancel()
				if err == nil {
					resp.Body.Close()
					t.Fatalf("client got an answer: %d", resp.StatusCode)
				}
				deadline := time.Now().Add(10 * time.Second) // the gateway finishes on its own after the client left
				for r.activeHolds() != 0 && time.Now().Before(deadline) {
					time.Sleep(50 * time.Millisecond)
				}
			} else {
				code, body, id := r.pay("echo", "v-un")
				m, _ := body["error"].(map[string]any)
				if code != 502 || m["type"] != server.CodeVaultUnavailable {
					t.Fatalf("%d %v", code, body)
				}
				rid = id
			}
			if r.activeHolds() != 0 || r.holdStatus() != "released" {
				t.Fatalf("hold %s, %d active", r.holdStatus(), r.activeHolds())
			}
			if b := r.balance(); b.Held != 0 || b.Balance != 10*usd {
				t.Fatalf("balance %+v", b)
			}
			if rid != "" {
				if v := r.row(rid); v.Vault.Outcome != wallet.OutcomeUnavailable || v.Status != server.StatusPaymentFailed {
					t.Fatalf("audit %+v %+v", v, v.Vault)
				}
			}
			logs := r.logs.String()
			if hasErr := strings.Contains(logs, "level=ERROR msg=\"vault unavailable\""); hasErr != tc.errorLog {
				t.Fatalf("ERROR log %v, want %v:\n%s", hasErr, tc.errorLog, logs)
			}
		})
	}
}
