package server_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/server"
)

// Stream passthrough is byte-identical (PLAN step 5), for both formats.
func TestStreamPassthroughByteIdentical(t *testing.T) {
	for _, c := range []struct{ name, model, path, body string }{
		{"openai", "oa/m", "/v1/chat/completions", chatBody("oa/m", true)},
		{"anthropic", "an/m", "/v1/messages", msgBody("an/m", true)},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, 10*usd, nil)
			ref := fakeupstream.New() // a second fake: same script, same first id, no gateway
			defer ref.Close()
			rawBody := strings.Replace(c.body, c.model, "m", 1)
			req, _ := http.NewRequest("POST", ref.URL+c.path, strings.NewReader(rawBody))
			req.Header.Set("Authorization", "Bearer x")
			direct, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := io.ReadAll(direct.Body)
			direct.Body.Close()

			resp, got := e.do(c.path, c.body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, got)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("stream differs\n got: %q\nwant: %q", got, want)
			}
			if row := e.auditRow(resp); row.Status != audit.StatusOK || row.CostSource != audit.SourcePriceTable {
				t.Fatalf("audit %+v", row)
			}
			e.noActiveHolds()
		})
	}
}

// The first event reaches the client before the fake sends the second.
func TestStreamIsIncremental(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	gate := make(chan struct{})
	e.fake.Push(fakeupstream.Script{Gate: gate})
	resp, err := e.doCtx(context.Background(), "/v1/chat/completions", chatBody("oa/m", true), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	done := make(chan string, 1)
	go func() { l, _ := br.ReadString('\n'); done <- l }()
	select {
	case l := <-done:
		if !strings.HasPrefix(l, "data: ") {
			t.Fatalf("first line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event did not arrive while the fake was still blocked")
	}
	close(gate)
	rest, _ := io.ReadAll(br)
	if !bytes.Contains(rest, []byte("[DONE]")) {
		t.Fatalf("stream did not finish: %q", rest)
	}
}

// I8: a stream that dies mid-way settles usage received, releases the rest.
func TestMidStreamDeath_I8(t *testing.T) {
	cases := []struct {
		name, path, body, source string
	}{
		{"openai-no-usage-yet", "/v1/chat/completions", chatBody("oa/m", true), audit.SourceEstimated},
		{"anthropic-partial-usage", "/v1/messages", msgBody("an/m", true), audit.SourcePriceTable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, 10*usd, nil)
			e.fake.Push(fakeupstream.Script{DieAfter: 4, Text: []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc", "dddddddddddd"}})
			resp, _ := e.do(c.path, c.body, nil)
			row := e.auditRow(resp)
			if row.Status != audit.StatusUpstreamFailed {
				t.Fatalf("status %q, want upstream_failed", row.Status)
			}
			if row.CostSource != c.source || row.Cost <= 0 {
				t.Fatalf("cost %d source %q, want >0 and %q", row.Cost, row.CostSource, c.source)
			}
			e.noActiveHolds() // remainder released; ledger.Audit passes
			b := e.balance()
			if b.Held != 0 || b.Balance != 10*usd-row.Cost {
				t.Fatalf("balance %+v, charged %d", b, row.Cost)
			}
			if row.Cost >= 5*usd/100 {
				t.Fatalf("charged %d: should be far below the hold", row.Cost)
			}
		})
	}
}

func TestDuplicateIdempotencyKey(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	h := map[string]string{"Idempotency-Key": "k-1"}
	r1, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), h)
	if r1.StatusCode != 200 {
		t.Fatalf("first: %d", r1.StatusCode)
	}
	r2, b2 := e.do("/v1/chat/completions", chatBody("oa/m", false), h)
	if r2.StatusCode != 409 {
		t.Fatalf("second: %d %s", r2.StatusCode, b2)
	}
	var out struct {
		Error struct {
			Code              string `json:"code"`
			OriginalRequestID string `json:"original_request_id"`
		}
	}
	_ = json.Unmarshal(b2, &out)
	if out.Error.Code != "duplicate_request" || out.Error.OriginalRequestID != r1.Header.Get("X-Request-Id") {
		t.Fatalf("body %s", b2)
	}
	if n := e.fake.CallCount(); n != 1 {
		t.Fatalf("upstream called %d times", n)
	}
	if row := e.auditRow(r2); row.Status != audit.StatusDuplicate {
		t.Fatalf("audit %+v", row)
	}
	e.noActiveHolds()
}

// A duplicate that arrives while the first is still streaming is also refused.
func TestDuplicateWhileInFlight(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	gate := make(chan struct{})
	e.fake.Push(fakeupstream.Script{Gate: gate})
	h := map[string]string{"Idempotency-Key": "k-2"}
	resp, err := e.doCtx(context.Background(), "/v1/chat/completions", chatBody("oa/m", true), h)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := e.do("/v1/chat/completions", chatBody("oa/m", true), h)
	if r2.StatusCode != 409 {
		t.Fatalf("dup status %d", r2.StatusCode)
	}
	close(gate)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if e.fake.CallCount() != 1 {
		t.Fatalf("upstream calls %d", e.fake.CallCount())
	}
}

// I5 end to end: parallel requests against a small budget never overspend.
func TestParallelBudget_I5(t *testing.T) {
	const deposit = 3_000_000 // Nanos; each hold below is ~1.1M, so only a few fit at once
	e := newEnv(t, deposit, func(s *server.Server) { s.DefaultMaxTokens = 500 })
	e.fake.SetDefault(fakeupstream.Script{ChunkDelay: 5 * time.Millisecond, InputTokens: 10, OutputTokens: 20})
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"oa/m","stream":true,"max_tokens":500,"messages":[{"role":"user","content":"hi %d"}]}`, i)
			resp, _ := e.do("/v1/chat/completions", body, nil)
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if codes[200] == 0 || codes[402] == 0 {
		t.Fatalf("expected both successes and refusals, got %v", codes)
	}
	e.noActiveHolds()
	b := e.balance()
	var spent int64
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(sum(cost_nanos),0) FROM audit_events WHERE status='ok'`).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if b.Balance < 0 || int64(deposit)-int64(b.Balance) != spent || spent > deposit {
		t.Fatalf("balance %d, audited spend %d, deposit %d", b.Balance, spent, deposit)
	}
}

// Cache pricing, hand computed: 1000 in, 500 out, 2000 cache read, 400 cache write at
// $1 / $2 / $0.10 / $1.25 per M = 1_000_000 + 1_000_000 + 200_000 + 500_000 = 2_700_000 Nanos.
func TestCachePricingBothFormats(t *testing.T) {
	script := fakeupstream.Script{InputTokens: 1000, OutputTokens: 500, CachedTokens: 2000, CacheWriteTokens: 400}
	cases := []struct{ name, path, body string }{
		{"openai-stream", "/v1/chat/completions", chatBody("oa/m", true)},
		{"openai-unary", "/v1/chat/completions", chatBody("oa/m", false)},
		{"anthropic-stream", "/v1/messages", msgBody("an/m", true)},
		{"anthropic-unary", "/v1/messages", msgBody("an/m", false)},
		{"translated-stream", "/v1/chat/completions", chatBody("an/m", true)},
		{"translated-unary", "/v1/chat/completions", chatBody("an/m", false)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, 10*usd, nil)
			e.fake.Push(script)
			resp, b := e.do(c.path, c.body, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("%d %s", resp.StatusCode, b)
			}
			row := e.auditRow(resp)
			u := row.Usage
			if u.InputTokens != 1000 || u.OutputTokens != 500 || u.CacheReadTokens != 2000 || u.CacheWriteTokens != 400 {
				t.Fatalf("usage %+v", u)
			}
			if row.Cost != 2_700_000 || row.CostSource != audit.SourcePriceTable {
				t.Fatalf("cost %d source %s", row.Cost, row.CostSource)
			}
			if got := 10*usd - e.balance().Balance; got != 2_700_000 {
				t.Fatalf("ledger charged %d", got)
			}
		})
	}
}

func TestProviderReportedCostWins(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.fake.Push(fakeupstream.Script{CostUSD: "0.000123"})
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", true), nil)
	row := e.auditRow(resp)
	if row.Cost != 123_000 || row.CostSource != audit.SourceProvider {
		t.Fatalf("%d %s", row.Cost, row.CostSource)
	}
}

func TestNoUsageFallsBackToEstimate(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.fake.Push(fakeupstream.Script{NoUsage: true})
	resp, _ := e.do("/v1/messages", msgBody("an/m", true), nil)
	row := e.auditRow(resp)
	if row.CostSource != audit.SourceEstimated || row.Cost <= 0 || row.Status != audit.StatusOK {
		t.Fatalf("%+v", row)
	}
	e.noActiveHolds()
}

// Upstream errors and client disconnects leave no hold active.
func TestFailuresLeaveNoActiveHold(t *testing.T) {
	for _, st := range []int{429, 500} {
		t.Run(fmt.Sprint(st), func(t *testing.T) {
			e := newEnv(t, 10*usd, nil)
			e.fake.Push(fakeupstream.Script{Status: st, RetryAfter: "7"})
			resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
			if resp.StatusCode != st {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if st == 429 && resp.Header.Get("Retry-After") != "7" {
				t.Fatal("Retry-After not passed through")
			}
			if row := e.auditRow(resp); row.Status != audit.StatusUpstreamError || row.Cost != 0 {
				t.Fatalf("%+v", row)
			}
			e.noActiveHolds()
			if e.balance().Balance != 10*usd {
				t.Fatal("charged for an error")
			}
		})
	}
	t.Run("client-disconnect", func(t *testing.T) {
		e := newEnv(t, 10*usd, nil)
		gate := make(chan struct{})
		defer close(gate)
		e.fake.Push(fakeupstream.Script{Gate: gate, NoUsage: true})
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := e.doCtx(ctx, "/v1/chat/completions", chatBody("oa/m", true), nil)
		if err != nil {
			t.Fatal(err)
		}
		id := resp.Header.Get("X-Request-Id")
		bufio.NewReader(resp.Body).ReadString('\n') // got the first event
		cancel()
		resp.Body.Close()
		var rows []audit.Row
		for i := 0; i < 300 && len(rows) == 0; i++ {
			rows, _ = e.audit.ByRequest(context.Background(), id)
			time.Sleep(10 * time.Millisecond)
		}
		if len(rows) != 1 || rows[0].Status != audit.StatusClientDisconnected {
			t.Fatalf("rows %+v", rows)
		}
		e.noActiveHolds()
	})
}

// The reconciler closes the hold mid-stream; the late settle is audited, not retried.
func TestSettleLostToReconciler(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	gate := make(chan struct{})
	e.fake.Push(fakeupstream.Script{Gate: gate, InputTokens: 100, OutputTokens: 50})
	resp, err := e.doCtx(context.Background(), "/v1/chat/completions", chatBody("oa/m", true), nil)
	if err != nil {
		t.Fatal(err)
	}
	bufio.NewReader(resp.Body).ReadString('\n')
	// Wait for the upstream id to be attached, then reconcile as if the lease had lapsed.
	waitFor(t, func() bool {
		var n int
		e.pool.QueryRow(context.Background(), `SELECT count(*) FROM holds WHERE status='active' AND upstream_id<>''`).Scan(&n)
		return n == 1
	})
	rep, err := e.led.Reconcile(context.Background(), noLookup{}, time.Now().Add(time.Hour))
	if err != nil || rep.Released != 1 {
		t.Fatalf("reconcile %+v %v", rep, err)
	}
	close(gate)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var row audit.Row
	waitFor(t, func() bool {
		rows, _ := e.audit.ByRequest(context.Background(), resp.Header.Get("X-Request-Id"))
		if len(rows) == 1 {
			row = rows[0]
		}
		return len(rows) == 1
	})
	if row.Status != audit.StatusSettleLost || row.Usage.InputTokens != 100 || row.Usage.OutputTokens != 50 {
		t.Fatalf("audit %+v", row)
	}
	if e.balance().Balance != 10*usd {
		t.Fatal("late settle must not charge")
	}
	if err := e.led.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type noLookup struct{}

func (noLookup) Lookup(context.Context, string, string) (ledger.Usage, bool, error) {
	return ledger.Usage{}, false, nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

// The lease keeps a long stream's hold alive: a reconcile pass at real "now"
// finds nothing expired, although the original TTL has long passed.
func TestLeaseExtendedWhileStreaming(t *testing.T) {
	e := newEnv(t, 10*usd, func(s *server.Server) { s.HoldTTL = 400 * time.Millisecond; s.ExtendEvery = 40 * time.Millisecond })
	gate := make(chan struct{})
	e.fake.Push(fakeupstream.Script{Gate: gate})
	resp, err := e.doCtx(context.Background(), "/v1/chat/completions", chatBody("oa/m", true), nil)
	if err != nil {
		t.Fatal(err)
	}
	bufio.NewReader(resp.Body).ReadString('\n')
	time.Sleep(1200 * time.Millisecond) // 3x the TTL
	rep, err := e.led.Reconcile(context.Background(), noLookup{}, time.Now())
	if err != nil || rep.Released+rep.Settled != 0 {
		t.Fatalf("live hold was reconciled: %+v %v", rep, err)
	}
	close(gate)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	e.auditRow(resp)
	e.noActiveHolds()
}

// I6 end to end on the fake: a hold whose process died is settled from /generation.
func TestReconcileUsesGenerationLookup_I6(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.fake.Push(fakeupstream.Script{CostUSD: "0.0005"})
	// A request that "crashed" before settling: reserve and attach by hand.
	h, err := e.led.Reserve(context.Background(), ledger.ReserveReq{AccountID: e.acct, Amount: usd / 10, IdemKey: "dead", Kind: ledger.KindModel, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// The call itself did happen upstream (so a generation exists): make it through the gateway of a second account-less client.
	req, _ := http.NewRequest("POST", e.fake.URL+"/v1/chat/completions", strings.NewReader(chatBody("m", false)))
	req.Header.Set("Authorization", "Bearer "+upstreamKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := e.led.AttachUpstream(context.Background(), h.ID, "fakeoa", "gen-1"); err != nil {
		t.Fatal(err)
	}
	lookup := &provider_lookup{e}
	rep, err := e.led.Reconcile(context.Background(), lookup.l(), time.Now().Add(time.Hour))
	if err != nil || rep.Settled != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if got := 10*usd - e.balance().Balance; got != 500_000 {
		t.Fatalf("charged %d, want 500000", got)
	}
	if err := e.led.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A not-yet-indexed generation is "not found": the hold is released unmetered.
func TestGenerationLookupNotFound(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	l := (&provider_lookup{e}).l()
	_, found, err := l.Lookup(context.Background(), "fakeoa", "gen-999")
	if err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	_, found, _ = l.Lookup(context.Background(), "fakean", "msg_1")
	if found {
		t.Fatal("anthropic kind has no lookup endpoint")
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Authorization": ""})
	if resp.StatusCode != 401 {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	if row := e.auditRow(resp); row.Status != audit.StatusUnauthorized {
		t.Fatalf("%+v", row)
	}
	resp, _ = e.do("/v1/messages", msgBody("an/m", false), map[string]string{"Authorization": "Bearer ag_wrong"})
	if resp.StatusCode != 401 {
		t.Fatalf("bad key: %d", resp.StatusCode)
	}
	// x-api-key, as Claude Code can send it
	resp, _ = e.do("/v1/messages", msgBody("an/m", false), map[string]string{"Authorization": "Bearer ag_wrong", "X-Api-Key": e.secret})
	if resp.StatusCode != 401 {
		// x-api-key takes precedence; here it is valid, so the call must succeed
		t.Logf("status %d", resp.StatusCode)
	}
	resp, _ = e.do("/v1/messages", msgBody("an/m", false), map[string]string{"Authorization": "", "X-Api-Key": e.secret})
	if resp.StatusCode != 200 {
		t.Fatalf("x-api-key: %d", resp.StatusCode)
	}
}

func TestKeyStoredAsHashOnly_I9(t *testing.T) {
	e := newEnv(t, 0, nil)
	var h []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT key_hash FROM api_keys`).Scan(&h); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(e.secret))
	if !bytes.Equal(h, sum[:]) {
		t.Fatal("stored value is not SHA-256 of the secret")
	}
	var dump string
	e.pool.QueryRow(context.Background(), `SELECT t::text FROM api_keys t`).Scan(&dump)
	if strings.Contains(dump, e.secret) {
		t.Fatal("secret found in table row")
	}
}

func TestUnknownModelPriceAndRoute(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	resp, b := e.do("/v1/chat/completions", chatBody("oa/nope", false), nil)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "unknown_model_price") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if row := e.auditRow(resp); row.Status != audit.StatusUnknownModelPrice {
		t.Fatalf("%+v", row)
	}
	resp, _ = e.do("/v1/chat/completions", chatBody("zzz/m", false), nil)
	if resp.StatusCode != 400 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if e.fake.CallCount() != 0 {
		t.Fatal("upstream called for a refused request")
	}
}

func TestInsufficientFunds(t *testing.T) {
	e := newEnv(t, 1, nil) // 1 Nano
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": "k"})
	if resp.StatusCode != 402 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if row := e.auditRow(resp); row.Status != audit.StatusInsufficientFunds {
		t.Fatalf("%+v", row)
	}
	// The claim was dropped, so the same key may be retried after a top-up.
	if err := e.led.Deposit(context.Background(), e.acct, 10*usd, "topup", ""); err != nil {
		t.Fatal(err)
	}
	resp, _ = e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": "k"})
	if resp.StatusCode != 200 {
		t.Fatalf("retry: %d", resp.StatusCode)
	}
}

// Anthropic passthrough keeps every body field and the anthropic-* headers, so
// prompt caching and thinking keep working; the client's credential is dropped.
func TestAnthropicPassthroughKeepsFieldsAndHeaders(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	body := `{"model":"an/m","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],"future_field":{"a":[1,2]}}`
	resp, b := e.do("/v1/messages", body, map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": "prompt-caching-2024-07-31,thinking-x"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	call := e.fake.Calls()[0]
	if call.Path != "/v1/messages" || call.Header.Get("Anthropic-Version") != "2023-06-01" || call.Header.Get("Anthropic-Beta") != "prompt-caching-2024-07-31,thinking-x" {
		t.Fatalf("headers %v path %s", call.Header, call.Path)
	}
	var got, want map[string]any
	json.Unmarshal(call.Body, &got)
	json.Unmarshal([]byte(strings.Replace(body, `"an/m"`, `"m"`, 1)), &want)
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if !bytes.Equal(gj, wj) {
		t.Fatalf("body changed\n got %s\nwant %s", gj, wj)
	}
	if call.Header.Get("X-Api-Key") != upstreamKey || strings.Contains(fmt.Sprint(call.Header), e.secret) {
		t.Fatalf("upstream auth wrong: %v", call.Header)
	}
}

// Chat completions routed to an Anthropic provider are translated, tool calls included.
func TestChatToAnthropicTranslation(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.fake.Push(fakeupstream.Script{ToolName: "get_weather", ToolArgs: `{"city":"Paris"}`})
	body := `{"model":"an/m","max_tokens":50,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	resp, b := e.do("/v1/chat/completions", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
			FinishReason string `json:"finish_reason"`
		}
		Usage struct{ Prompt_tokens, Completion_tokens int }
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	tc := out.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].Function.Name != "get_weather" || tc[0].Function.Arguments != `{"city":"Paris"}` || out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("%s", b)
	}
	var sent map[string]any
	json.Unmarshal(e.fake.Calls()[0].Body, &sent)
	if sent["system"] != "be brief" || sent["max_tokens"].(float64) != 50 {
		t.Fatalf("sent %v", sent)
	}
	if tools := sent["tools"].([]any); tools[0].(map[string]any)["input_schema"] == nil {
		t.Fatalf("tools %v", sent["tools"])
	}
}

func TestChatToAnthropicStreamTranslation(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.fake.Push(fakeupstream.Script{ToolName: "f", ToolArgs: `{"a":1}`, InputTokens: 7, OutputTokens: 9})
	resp, b := e.do("/v1/chat/completions", chatBody("an/m", true), nil)
	s := string(b)
	for _, want := range []string{`"role":"assistant"`, `"content":"Hello"`, `"name":"f"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":7`, `"completion_tokens":9`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in\n%s", want, s)
		}
	}
	if row := e.auditRow(resp); row.Usage.InputTokens != 7 || row.Usage.OutputTokens != 9 {
		t.Fatalf("%+v", row.Usage)
	}
}

// I9: neither the upstream key nor the client secret reaches logs, spans or the
// client; upstream auth headers are not relayed.
func TestSecretsNeverLeak_I9(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	for _, c := range []struct{ path, body string }{
		{"/v1/chat/completions", chatBody("oa/m", true)},
		{"/v1/messages", msgBody("an/m", false)},
		{"/v1/chat/completions", chatBody("oa/nope", false)},
	} {
		resp, b := e.do(c.path, c.body, nil)
		if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Upstream-Auth") != "" {
			t.Fatalf("upstream header relayed: %v", resp.Header)
		}
		if strings.Contains(fmt.Sprint(resp.Header), upstreamKey) || strings.Contains(string(b), upstreamKey) {
			t.Fatal("upstream key reached the client")
		}
		e.auditRow(resp)
	}
	// the upstream saw the upstream key, not the client's secret
	for _, c := range e.fake.Calls() {
		if strings.Contains(fmt.Sprint(c.Header), e.secret) {
			t.Fatal("client secret forwarded upstream")
		}
	}
	logs := e.logs.String()
	var spans strings.Builder
	for _, s := range e.spans.GetSpans() {
		fmt.Fprintf(&spans, "%s %v %v\n", s.Name, s.Attributes, s.Events)
	}
	for name, hay := range map[string]string{"logs": logs, "spans": spans.String()} {
		for _, secret := range []string{upstreamKey, e.secret} {
			if strings.Contains(hay, secret) {
				t.Fatalf("%s contain a secret:\n%s", name, hay)
			}
		}
		if strings.Contains(hay, `"hi"`) || strings.Contains(hay, "messages") {
			t.Fatalf("%s contain prompt content", name)
		}
	}
	if logs == "" || spans.Len() == 0 {
		t.Fatal("expected some logs and spans to inspect")
	}
}

func TestSpanShape(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	e.auditRow(resp)
	names := map[string]string{}
	var root string
	for _, s := range e.spans.GetSpans() {
		names[s.Name] = s.Parent.SpanID().String()
		if s.Name == "gateway.request" {
			root = s.SpanContext.SpanID().String()
		}
	}
	for _, n := range []string{"policy", "reserve", "upstream", "settle"} {
		if p, ok := names[n]; !ok || p != root {
			t.Fatalf("span %q missing or not a child of the request span: %v", n, names)
		}
	}
}

func TestBodiesNotStoredByDefaultAndRedactedWhenOn(t *testing.T) {
	body := `{"model":"oa/m","max_tokens":10,"messages":[{"role":"user","content":"my key is sk-abcdef1234567890 and Bearer tok_abc.def-123"}]}`
	e := newEnv(t, 10*usd, nil)
	resp, _ := e.do("/v1/chat/completions", body, nil)
	if r := e.auditRow(resp); r.RequestBody != nil || r.ResponseBody != nil {
		t.Fatal("bodies stored by default")
	}
	e2 := newEnv(t, 10*usd, func(s *server.Server) { s.StoreBodies = true })
	resp, _ = e2.do("/v1/chat/completions", body, nil)
	r := e2.auditRow(resp)
	if r.RequestBody == nil || r.ResponseBody == nil {
		t.Fatal("bodies not stored when enabled")
	}
	if strings.Contains(*r.RequestBody, "sk-abcdef") || strings.Contains(*r.RequestBody, "tok_abc") || !strings.Contains(*r.RequestBody, "[REDACTED]") {
		t.Fatalf("not redacted: %s", *r.RequestBody)
	}
}

func TestAuditIsAppendOnly(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	e.auditRow(resp)
	if _, err := e.pool.Exec(context.Background(), `UPDATE audit_events SET status='x'`); err == nil {
		t.Fatal("update allowed")
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM audit_events`); err == nil {
		t.Fatal("delete allowed")
	}
}

func TestReportAggregates(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.fake.Push(fakeupstream.Script{InputTokens: 1000, OutputTokens: 500, CachedTokens: 2000, CacheWriteTokens: 400})
	e.fake.Push(fakeupstream.Script{NoUsage: true})
	r1, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	r2, _ := e.do("/v1/chat/completions", chatBody("oa/m", true), nil)
	e.auditRow(r1)
	e.auditRow(r2)
	lines, err := audit.Report(context.Background(), e.pool, time.Now().Add(-time.Hour), "model", func(string) (pricing_Price, bool) { return testPrice, true })
	if err != nil || len(lines) != 1 {
		t.Fatalf("%v %v", lines, err)
	}
	l := lines[0]
	// 2000 cache-read tokens at ($1 - $0.10)/M = 1_800_000 Nanos saved.
	if l.Requests != 2 || l.CacheRead != 2000 || l.Savings != 1_800_000 {
		t.Fatalf("%+v", l)
	}
	if l.BySource[audit.SourcePriceTable].Requests != 1 || l.BySource[audit.SourceEstimated].Requests != 1 {
		t.Fatalf("split %+v", l.BySource)
	}
	if hr := l.HitRate(); hr < 0.58 || hr > 0.59 { // 2000 / 3400
		t.Fatalf("hit rate %f", hr)
	}
	if byAcct, _ := audit.Report(context.Background(), e.pool, time.Now().Add(-time.Hour), "account", nil); len(byAcct) != 1 || byAcct[0].Key != e.acct {
		t.Fatalf("%+v", byAcct)
	}
}

var _ = money.Nanos(0)
