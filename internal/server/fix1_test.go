package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/pricing"
	"github.com/ws4charlie/agent-gate/internal/server"
)

func (e *env) holdUpstreamID(id string) string {
	e.t.Helper()
	var up string
	if err := e.pool.QueryRow(context.Background(), `SELECT upstream_id FROM holds WHERE account_id=$1 ORDER BY created_at DESC LIMIT 1`, e.acct).Scan(&up); err != nil {
		e.t.Fatal(err)
	}
	return up
}

// The Anthropic surface's body id (msg_...) cannot be looked up; the header id can.
func TestGenerationHeaderBeatsBodyID(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e := newEnv(t, 10*usd, nil)
		resp, b := e.do("/v1/messages", msgBody("an/m", stream), nil)
		if !strings.Contains(string(b), "msg_1") {
			t.Fatalf("fake should carry a msg_ body id: %s", b)
		}
		row := e.auditRow(resp)
		if got := e.holdUpstreamID(""); got != "gen-1" || row.Usage.UpstreamID != "gen-1" {
			t.Fatalf("stream=%v hold upstream_id %q audit %q, want gen-1", stream, got, row.Usage.UpstreamID)
		}
	}
}

// Kill scenario: the process died after the upstream call; the reconciler
// settles from /generation using the header id, and audits it.
func TestCrashedHoldReconciledFromGenerationHeader(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.fake.Push(fakeupstream.Script{CostUSD: "0.0005"})
	h, err := e.led.Reserve(context.Background(), ledger.ReserveReq{AccountID: e.acct, Amount: usd / 10, IdemKey: "dead", Kind: ledger.KindModel, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", e.fake.URL+"/v1/messages", strings.NewReader(msgBody("m", false)))
	req.Header.Set("X-Api-Key", upstreamKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := e.led.AttachUpstream(context.Background(), h.ID, "fakeoa", resp.Header.Get("X-Generation-Id")); err != nil {
		t.Fatal(err)
	}
	rep, err := server.Reconcile(context.Background(), e.led, (&provider_lookup{e}).l(), e.audit, time.Now().Add(time.Hour))
	if err != nil || rep.Settled != 1 || rep.Errors != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	rows, _ := e.audit.ByRequest(context.Background(), "reconcile:"+h.ID)
	if len(rows) != 1 || rows[0].Status != audit.StatusReconciledSettled || rows[0].Cost != 500_000 || rows[0].AccountID != e.acct || rows[0].HoldID != h.ID {
		t.Fatalf("%+v", rows)
	}
	e.noActiveHolds()
}

// A 400 "Invalid generation ID" can never resolve: release the hold, no retry loop,
// and audit it as unmetered; the report shows the count and amount.
func TestLookup400ReleasesAndAuditsUnmetered(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	h, _ := e.led.Reserve(context.Background(), ledger.ReserveReq{AccountID: e.acct, Amount: usd / 10, IdemKey: "old", Kind: ledger.KindModel, TTL: time.Minute})
	if err := e.led.AttachUpstream(context.Background(), h.ID, "fakeoa", "msg_old"); err != nil {
		t.Fatal(err)
	}
	rep, err := server.Reconcile(context.Background(), e.led, (&provider_lookup{e}).l(), e.audit, time.Now().Add(time.Hour))
	if err != nil || rep.Errors != 0 || rep.Released != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	rows, _ := e.audit.ByRequest(context.Background(), "reconcile:"+h.ID)
	if len(rows) != 1 || rows[0].Status != audit.StatusReconciledUnmetered || !rows[0].Unmetered || rows[0].UnmeteredNanos != usd/10 {
		t.Fatalf("%+v", rows)
	}
	lines, err := audit.Report(context.Background(), e.pool, time.Now().Add(-time.Hour), "account", nil)
	if err != nil || len(lines) != 1 || lines[0].Unmetered != 1 || lines[0].UnmeteredNanos != usd/10 || lines[0].Requests != 0 {
		t.Fatalf("%+v %v", lines, err)
	}
	var sb strings.Builder
	audit.Print(&sb, "account", lines)
	if !strings.Contains(sb.String(), "unmetered: 1 holds") {
		t.Fatalf("report text:\n%s", sb.String())
	}
	e.noActiveHolds()
}

func TestUnaryTruncatedBodyIs502(t *testing.T) {
	for _, c := range []struct{ path, body string }{
		{"/v1/chat/completions", chatBody("oa/m", false)},
		{"/v1/messages", msgBody("an/m", false)},
	} {
		e := newEnv(t, 10*usd, nil)
		e.fake.Push(fakeupstream.Script{TruncateBody: true})
		resp, b := e.do(c.path, c.body, nil)
		if resp.StatusCode != 502 || !strings.Contains(string(b), "upstream_failed") {
			t.Fatalf("%s: %d %s", c.path, resp.StatusCode, b)
		}
		if row := e.auditRow(resp); row.Status != audit.StatusUpstreamFailed {
			t.Fatalf("%+v", row)
		}
		e.noActiveHolds()
	}
}

// In-band errors and clean EOF before the terminal event are failures (I8): charged
// for what arrived, remainder released.
func TestStreamFailuresAreNotOK(t *testing.T) {
	cases := []struct {
		name, path, model string
		script            fakeupstream.Script
	}{
		{"openai-inband", "/v1/chat/completions", "oa/m", fakeupstream.Script{InBandError: true}},
		{"anthropic-inband", "/v1/messages", "an/m", fakeupstream.Script{InBandError: true}},
		{"openai-noterm", "/v1/chat/completions", "oa/m", fakeupstream.Script{NoTerminal: true}},
		{"anthropic-noterm", "/v1/messages", "an/m", fakeupstream.Script{NoTerminal: true}},
		{"translated-inband", "/v1/chat/completions", "an/m", fakeupstream.Script{InBandError: true}},
		{"translated-noterm", "/v1/chat/completions", "an/m", fakeupstream.Script{NoTerminal: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, 10*usd, nil)
			e.fake.Push(c.script)
			body := msgBody(c.model, true)
			resp, _ := e.do(c.path, body, nil)
			row := e.auditRow(resp)
			if row.Status != audit.StatusUpstreamFailed || row.Cost <= 0 {
				t.Fatalf("%+v", row)
			}
			e.noActiveHolds()
			if b := e.balance(); b.Held != 0 || b.Balance != 10*usd-row.Cost {
				t.Fatalf("balance %+v charged %d", b, row.Cost)
			}
		})
	}
}

// A pricing failure after the body was sent must not write to the response, and
// leaves the hold for the reconciler.
func TestSettlePricingErrorDoesNotTouchResponse(t *testing.T) {
	e := newEnv(t, 10*usd, func(s *server.Server) {
		s.Prices.Models["big"] = pricing.Price{Input: 1, Output: 9_000_000_000_000_000_000}
	})
	e.fake.Push(fakeupstream.Script{OutputTokens: 2_000_000})
	resp, b := e.do("/v1/chat/completions", chatBody("oa/big", true), nil)
	if resp.StatusCode != 200 || !strings.HasSuffix(strings.TrimSpace(string(b)), "[DONE]") || strings.Contains(string(b), "internal_error") {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
	row := e.auditRow(resp)
	if row.Status != audit.StatusInternal || row.Err != "price_failed" {
		t.Fatalf("%+v", row)
	}
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM holds WHERE status='active'`).Scan(&n)
	if n != 1 {
		t.Fatalf("hold should stay active for the reconciler, %d active", n)
	}
	rep, err := server.Reconcile(context.Background(), e.led, noLookup{}, e.audit, time.Now().Add(time.Hour))
	if err != nil || rep.Released != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	e.noActiveHolds()
}
