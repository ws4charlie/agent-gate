package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/pricing"
	"github.com/ws4charlie/agent-gate/internal/server"
)

// setScope edits the scope of the test key's agent. The engine reads it fresh on every request.
func (e *env) setScope(f func(*policy.Scope)) {
	e.t.Helper()
	sc := policy.DefaultScope()
	f(&sc)
	b, _ := json.Marshal(sc)
	if _, err := e.pool.Exec(context.Background(), `UPDATE agents SET scope=$1 WHERE account_id=$2`, b, e.acct); err != nil {
		e.t.Fatal(err)
	}
}

// estimate mirrors the gateway's worst-case price for chatBody (max_tokens 200).
func estimate(body string) money.Nanos {
	in, _ := money.Cost(pricing.EstimateTokens(len(body)), max(testPrice.Input, testPrice.CacheWrite))
	out, _ := money.Cost(200, testPrice.Output)
	return in + out
}

func jsonBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %q", b)
	}
	return m
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func (e *env) decisions(resp *http.Response) []agents.Decision {
	e.t.Helper()
	d, err := e.srv.Agents.Decisions(context.Background(), resp.Header.Get("X-Request-Id"))
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *env) snapshot(task string) policy.Snapshot {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM agents WHERE account_id=$1`, e.acct).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	s, err := e.srv.Agents.Snapshot(context.Background(), id, task)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// Every decision is logged with its rule, allow included, and joins to the
// audit row by request id. A refusal never calls upstream and never holds money.
func TestScopeRefusalAndDecisionLog(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.setScope(func(s *policy.Scope) { s.Models = []string{"oa/m"} })

	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	d := e.decisions(resp)
	if len(d) != 1 || d[0].Verdict != policy.Allow || d[0].Rule != policy.RuleAllow || d[0].Subject != "oa/m" || d[0].Kind != "model" || d[0].Amount <= 0 {
		t.Fatalf("allow decision: %+v", d)
	}
	calls := e.fake.CallCount()

	resp, b = e.do("/v1/messages", msgBody("an/m", false), nil) // out of scope
	if resp.StatusCode != 403 || errCode(jsonBody(t, b)) != policy.RuleScopeModel {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if e.fake.CallCount() != calls {
		t.Fatal("upstream was called for a refused request")
	}
	row := e.auditRow(resp)
	if row.Status != server.StatusPolicyDenied || row.Err != policy.RuleScopeModel || row.HoldID != "" || row.AgentID == "" {
		t.Fatalf("audit %+v", row)
	}
	d = e.decisions(resp)
	if len(d) != 1 || d[0].Verdict != policy.Deny || d[0].Rule != policy.RuleScopeModel || d[0].AgentID != row.AgentID {
		t.Fatalf("deny decision: %+v", d)
	}
	e.noActiveHolds()
}

// The daily budget burns down as requests are admitted, and goes back up to the
// real cost when they settle, so the window tracks spend, not estimates.
func TestDailyBudgetBurnsAndTracksRealCost(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.DailyBudget = est * 5 / 2 }) // room for two estimates, not three
	e.fake.SetDefault(fakeupstream.Script{InputTokens: 10, OutputTokens: 10, CostUSD: "0.000001"})

	for i := 0; i < 2; i++ {
		if resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil); resp.StatusCode != 200 {
			t.Fatalf("call %d: %d %s", i, resp.StatusCode, b)
		}
	}
	e.noActiveHolds()
	// The real cost was 1000 nanos a call, so the window holds 2000, not 2*est.
	if s := e.snapshot(""); s.DailySpent != 2000 {
		t.Fatalf("daily spent %d, want the real cost 2000", s.DailySpent)
	}
	// Burn it for real: a budget below one estimate refuses.
	e.setScope(func(s *policy.Scope) { s.DailyBudget = est - 1 })
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 429 || errCode(jsonBody(t, b)) != policy.RuleBudgetDaily {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if row := e.auditRow(resp); row.Status != server.StatusPolicyDenied || row.Err != policy.RuleBudgetDaily {
		t.Fatalf("%+v", row)
	}
	if d := e.decisions(resp); len(d) != 1 || d[0].Rule != policy.RuleBudgetDaily {
		t.Fatalf("%+v", d)
	}
}

// Parallel requests against a daily budget never exceed it (I5 for budgets).
func TestDailyBudgetRaceNeverExceeded(t *testing.T) {
	e := newEnv(t, 100*usd, nil)
	est := estimate(chatBody("oa/m", false))
	const room, n = 6, 40
	e.setScope(func(s *policy.Scope) { s.DailyBudget = est * room })
	// Every call is still in flight while the others arrive: nothing frees budget early.
	e.fake.SetDefault(fakeupstream.Script{Latency: 400 * time.Millisecond, InputTokens: 10, OutputTokens: 10, CostUSD: "0.000001"})

	var ok, over atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": fmt.Sprint("race-", i)})
			switch {
			case resp.StatusCode == 200:
				ok.Add(1)
			case resp.StatusCode == 429 && errCode(jsonBody(t, b)) == policy.RuleBudgetDaily:
				over.Add(1)
			default:
				t.Errorf("%d %s", resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != room || ok.Load()+over.Load() != n {
		t.Fatalf("admitted %d of %d, want exactly %d", ok.Load(), n, room)
	}
	e.noActiveHolds()
	if s := e.snapshot(""); s.DailySpent != money.Nanos(room*1000) {
		t.Fatalf("daily spent %d after settling %d calls", s.DailySpent, room)
	}
	// The losers left nothing behind: no stray holds, no charges for them.
	var charges int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM budget_charges`).Scan(&charges)
	if charges != room {
		t.Fatalf("%d charge rows, want %d", charges, room)
	}
}

func TestTaskBudgetRaceNeverExceeded(t *testing.T) {
	e := newEnv(t, 100*usd, nil)
	est := estimate(chatBody("oa/m", false))
	const room, n = 4, 24
	e.setScope(func(s *policy.Scope) { s.TaskBudget = est * room })
	e.fake.SetDefault(fakeupstream.Script{Latency: 400 * time.Millisecond, InputTokens: 10, OutputTokens: 10, CostUSD: "0.000001"})
	var ok, over [2]atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task := i % 2
			resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false),
				map[string]string{"Idempotency-Key": fmt.Sprint("t-", i), server.HeaderTask: fmt.Sprint("task-", task)})
			switch {
			case resp.StatusCode == 200:
				ok[task].Add(1)
			case resp.StatusCode == 429 && errCode(jsonBody(t, b)) == policy.RuleBudgetTask:
				over[task].Add(1)
			default:
				t.Errorf("%d %s", resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	for task := 0; task < 2; task++ { // the two tasks have separate windows
		if ok[task].Load() != room {
			t.Fatalf("task %d admitted %d, want %d", task, ok[task].Load(), room)
		}
	}
	e.noActiveHolds()
	if s := e.snapshot("task-0"); s.TaskSpent != room*1000 {
		t.Fatalf("task spent %d", s.TaskSpent)
	}
}

func TestTaskHeaderTooLong(t *testing.T) {
	e := newEnv(t, usd, nil)
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{server.HeaderTask: strings.Repeat("x", 129)})
	if resp.StatusCode != 400 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

// Velocity: a sliding minute, in Postgres.
func TestVelocityTripsAndSlides(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.VelocityPerMinute = est * 2 })
	now := time.Now()
	e.srv.Agents.SetClock(func() time.Time { return now })
	// Settled cost is tiny, so the minute only fills if charges keep their
	// estimate: use calls that stay in flight.
	e.fake.SetDefault(fakeupstream.Script{Latency: 300 * time.Millisecond, InputTokens: 10, OutputTokens: 10, CostUSD: "0.000001"})
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": fmt.Sprint("v-", i)})
			codes[i] = resp.StatusCode
		}()
		time.Sleep(30 * time.Millisecond)
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("codes %v", codes)
	}
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": "v-late"})
	// Settled spend is 2000 nanos, well inside the limit.
	if resp.StatusCode != 200 {
		t.Fatalf("after settling: %d %s", resp.StatusCode, b)
	}
}

func TestVelocityRefusalNamesTheRule(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.VelocityPerMinute = est - 1 })
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 429 || errCode(jsonBody(t, b)) != policy.RuleVelocity {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if d := e.decisions(resp); len(d) != 1 || d[0].Rule != policy.RuleVelocity {
		t.Fatalf("%+v", d)
	}
}

// ---- approvals ----

func (e *env) pending() []approval.Approval {
	e.t.Helper()
	l, err := e.srv.Approvals.List(context.Background(), approval.Pending)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *env) heldNanos() money.Nanos { return e.balance().Held }

// Over the threshold: 202 approval_required with an id, the money is held, the
// upstream is not called. Approval runs the request once, with the same key.
func TestApprovalQueueAllowThenReplayRunsOnce(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.ApprovalThreshold = est - 1 })
	hdr := map[string]string{"Idempotency-Key": "big-1"}

	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	m := jsonBody(t, b)
	if resp.StatusCode != 202 || m["status"] != "approval_required" || m["approval_id"] == "" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	id := m["approval_id"].(string)
	if e.fake.CallCount() != 0 {
		t.Fatal("upstream called before approval")
	}
	if e.heldNanos() != est {
		t.Fatalf("held %d, want %d: the money must be locked while waiting", e.heldNanos(), est)
	}
	if row := e.auditRow(resp); row.Status != server.StatusApprovalRequired || row.HoldID == "" {
		t.Fatalf("audit %+v", row)
	}
	if d := e.decisions(resp); len(d) != 1 || d[0].Verdict != policy.RequireApproval || d[0].Rule != policy.RuleApprovalThreshold {
		t.Fatalf("%+v", d)
	}

	// Asking again while pending is the same entry, not a second one.
	resp, b = e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	if resp.StatusCode != 202 || jsonBody(t, b)["approval_id"] != id || len(e.pending()) != 1 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if e.fake.CallCount() != 0 {
		t.Fatal("upstream called while pending")
	}

	// A human approves. The approver is recorded.
	a, err := e.srv.Approvals.Allow(context.Background(), id, "alice")
	if err != nil || a.Status != approval.Approved || a.DecidedBy != "alice" || a.DecidedAt == nil {
		t.Fatalf("%+v %v", a, err)
	}
	resp, b = e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("replay after approval: %d %s", resp.StatusCode, b)
	}
	if e.fake.CallCount() != 1 {
		t.Fatalf("upstream called %d times", e.fake.CallCount())
	}
	if d := e.decisions(resp); len(d) != 1 || d[0].Verdict != policy.Allow || d[0].ApprovalID != id {
		t.Fatalf("approved replay decision %+v", d)
	}
	// One execution: the approval is spent.
	resp, b = e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	if resp.StatusCode != 409 || errCode(jsonBody(t, b)) != "duplicate_request" {
		t.Fatalf("second replay: %d %s", resp.StatusCode, b)
	}
	if e.fake.CallCount() != 1 {
		t.Fatal("approval ran twice")
	}
	got, _ := e.srv.Approvals.Get(context.Background(), id)
	if got.Status != approval.Consumed {
		t.Fatalf("status %s", got.Status)
	}
	e.noActiveHolds()
	if e.balance().Balance >= 10*usd { // it was charged, once
		t.Fatal("approved request was free")
	}
}

// An approval is for one exact request: a different body, model or task with
// the same Idempotency-Key is refused, and does not spend the approval.
func TestApprovalNeverAuthorizesADifferentRequest(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.ApprovalThreshold = est - 1 })
	hdr := map[string]string{"Idempotency-Key": "big-2"}
	_, b := e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	id := jsonBody(t, b)["approval_id"].(string)
	if _, err := e.srv.Approvals.Allow(context.Background(), id, "alice"); err != nil {
		t.Fatal(err)
	}

	bigger := `{"model":"oa/m","stream":false,"max_tokens":900,"messages":[{"role":"user","content":"hi"}]}` // another amount
	other := `{"model":"oa/m","stream":false,"max_tokens":200,"messages":[{"role":"user","content":"hello there"}]}`
	for name, tc := range map[string]struct {
		body string
		hdr  map[string]string
	}{
		"amount": {bigger, hdr},
		"body":   {other, hdr},
		"model":  {strings.Replace(chatBody("oa/m", false), "oa/m", "an/m", 1), hdr},
		"task":   {chatBody("oa/m", false), map[string]string{"Idempotency-Key": "big-2", server.HeaderTask: "other-task"}},
	} {
		path := "/v1/chat/completions"
		resp, b := e.do(path, tc.body, tc.hdr)
		if resp.StatusCode != 422 || errCode(jsonBody(t, b)) != "approval_mismatch" {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, b)
		}
		if row := e.auditRow(resp); row.Status != server.StatusApprovalMismatch {
			t.Fatalf("%s: audit %+v", name, row)
		}
	}
	if e.fake.CallCount() != 0 {
		t.Fatal("a changed request reached upstream")
	}
	if got, _ := e.srv.Approvals.Get(context.Background(), id); got.Status != approval.Approved {
		t.Fatalf("mismatches spent the approval: %s", got.Status)
	}
	// The original request still runs.
	if resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), hdr); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

// Parallel replays of one approval: exactly one runs.
func TestApprovalReplayRaceRunsOnce(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.ApprovalThreshold = est - 1 })
	hdr := map[string]string{"Idempotency-Key": "big-3"}
	_, b := e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	id := jsonBody(t, b)["approval_id"].(string)
	if _, err := e.srv.Approvals.Allow(context.Background(), id, "alice"); err != nil {
		t.Fatal(err)
	}
	var ok, dup atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
			switch resp.StatusCode {
			case 200:
				ok.Add(1)
			case 409:
				dup.Add(1)
			default:
				t.Errorf("%d %s", resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || e.fake.CallCount() != 1 {
		t.Fatalf("ran %d times (upstream %d)", ok.Load(), e.fake.CallCount())
	}
	e.noActiveHolds()
}

// Deny releases the hold; a replay is refused.
func TestApprovalDenyReleasesHold(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.ApprovalThreshold = est - 1 })
	hdr := map[string]string{"Idempotency-Key": "big-4"}
	_, b := e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	id := jsonBody(t, b)["approval_id"].(string)
	if e.heldNanos() == 0 {
		t.Fatal("nothing held while pending")
	}
	a, err := e.srv.Approvals.Deny(context.Background(), id, "bob")
	if err != nil || a.Status != approval.Denied || a.DecidedBy != "bob" {
		t.Fatalf("%+v %v", a, err)
	}
	if e.heldNanos() != 0 || e.balance().Balance != 10*usd {
		t.Fatalf("hold not released: %+v", e.balance())
	}
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	if resp.StatusCode != 403 || errCode(jsonBody(t, b)) != "approval_denied" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if e.fake.CallCount() != 0 {
		t.Fatal("denied request reached upstream")
	}
	// A decision is final.
	if _, err := e.srv.Approvals.Allow(context.Background(), id, "alice"); err == nil {
		t.Fatal("allowed after deny")
	}
	e.noActiveHolds()
}

// An approval that nobody decides expires and frees the hold; one that was
// approved but not used expires too.
func TestApprovalExpires(t *testing.T) {
	e := newEnv(t, 10*usd, func(s *server.Server) { s.ApprovalTTL = 300 * time.Millisecond })
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.ApprovalThreshold = est - 1 })

	_, b := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": "exp-1"})
	id1 := jsonBody(t, b)["approval_id"].(string)
	_, b = e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": "exp-2"})
	id2 := jsonBody(t, b)["approval_id"].(string)
	if _, err := e.srv.Approvals.Allow(context.Background(), id2, "alice"); err != nil { // approved, never used
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)

	// Too late to approve.
	if _, err := e.srv.Approvals.Allow(context.Background(), id1, "alice"); err != approval.ErrExpired {
		t.Fatalf("allow after expiry: %v", err)
	}
	// Replays are refused, and the holds are free.
	for _, key := range []string{"exp-1", "exp-2"} {
		resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": key})
		if resp.StatusCode != 410 || errCode(jsonBody(t, b)) != "approval_expired" {
			t.Fatalf("%s: %d %s", key, resp.StatusCode, b)
		}
	}
	if e.fake.CallCount() != 0 {
		t.Fatal("expired approval reached upstream")
	}
	if e.balance().Balance != 10*usd {
		t.Fatalf("balance %+v", e.balance())
	}
	e.noActiveHolds()
	for _, id := range []string{id1, id2} {
		if got, _ := e.srv.Approvals.Get(context.Background(), id); got.Status != approval.Expired {
			t.Fatalf("%s is %s", id, got.Status)
		}
	}
}

// The sweeper (what `serve` runs on a timer) expires and releases without any replay.
func TestApprovalSweepReleasesHolds(t *testing.T) {
	e := newEnv(t, 10*usd, func(s *server.Server) { s.ApprovalTTL = 100 * time.Millisecond })
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.ApprovalThreshold = est - 1 })
	e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": "sw-1"})
	time.Sleep(150 * time.Millisecond)
	n, err := e.srv.Approvals.Sweep(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	e.noActiveHolds()
}

// Approval lifts the threshold and nothing else: a budget that ran out while
// the request waited still refuses it.
func TestApprovedRequestStillMeetsTheBudget(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.ApprovalThreshold = est - 1; s.DailyBudget = est * 3 })
	hdr := map[string]string{"Idempotency-Key": "big-5"}
	_, b := e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	id := jsonBody(t, b)["approval_id"].(string)
	if _, err := e.srv.Approvals.Allow(context.Background(), id, "alice"); err != nil {
		t.Fatal(err)
	}
	e.setScope(func(s *policy.Scope) { s.ApprovalThreshold = est - 1; s.DailyBudget = est - 1 }) // the limit shrank
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), hdr)
	if resp.StatusCode != 429 || errCode(jsonBody(t, b)) != policy.RuleBudgetDaily {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if e.fake.CallCount() != 0 {
		t.Fatal("reached upstream")
	}
	e.noActiveHolds() // the refused replay released the hold
}

// There is no HTTP way to approve: an agent credential cannot approve itself.
func TestNoHTTPRouteToApprove(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.ApprovalThreshold = est - 1 })
	_, b := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Idempotency-Key": "self-1"})
	id := jsonBody(t, b)["approval_id"].(string)
	for _, path := range []string{"/v1/approvals/" + id + "/allow", "/v1/approvals/" + id, "/v1/approvals", "/approve/" + id, "/v1/agents"} {
		for _, method := range []string{"POST", "PUT", "PATCH", "GET"} {
			req, _ := http.NewRequest(method, e.gw.URL+path, strings.NewReader(`{"status":"approved"}`))
			req.Header.Set("Authorization", "Bearer "+e.secret)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 404 && resp.StatusCode != 405 {
				t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
			}
		}
	}
	if got, _ := e.srv.Approvals.Get(context.Background(), id); got.Status != approval.Pending {
		t.Fatalf("status %s", got.Status)
	}
}

// ---- kill switch and revoke (I12) ----

func (e *env) watchKills() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.srv.WatchKills(ctx); close(done) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

func (e *env) agentID() string {
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM agents WHERE account_id=$1`, e.acct).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// I12: kill during a stream. The stream is cut within 1s, the next request is
// refused, the hold settles for what was received, and the ledger audit passes.
// The kill is a database write only, like the CLI's: the gateway finds it.
func TestKillMidStream_I12(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.watchKills()
	est := estimate(chatBody("oa/m", true))
	e.setScope(func(s *policy.Scope) { s.DailyBudget = est * 10 })
	// ~10s of slow output if nobody stops it.
	e.fake.SetDefault(fakeupstream.Script{Chunks: 200, ChunkBytes: 40, ChunkDelay: 50 * time.Millisecond, NoUsage: true})

	resp, err := e.doCtx(context.Background(), "/v1/chat/completions", chatBody("oa/m", true), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil { // the stream is flowing
		t.Fatal(err)
	}
	var lines int
	ended := make(chan time.Time, 1)
	go func() {
		for {
			if _, err := br.ReadString('\n'); err != nil {
				ended <- time.Now()
				return
			}
			lines++
		}
	}()
	time.Sleep(200 * time.Millisecond)
	killedAt := time.Now()
	if _, err := e.srv.Agents.Kill(context.Background(), e.agentID()); err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-ended:
		if d := at.Sub(killedAt); d > time.Second {
			t.Fatalf("stream cancelled after %v, want within 1s", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream was not cancelled")
	}

	row := e.auditRow(resp)
	if row.Status != server.StatusKilled || row.Err != policy.RuleKilled {
		t.Fatalf("audit %+v", row)
	}
	if row.Cost <= 0 || row.Cost >= est || row.CostSource != audit.SourceEstimated { // I8: what arrived, flagged
		t.Fatalf("cost %d (estimate %d) source %q", row.Cost, est, row.CostSource)
	}
	e.noActiveHolds() // settled, and ledger.Audit passes
	if b := e.balance(); b.Balance != 10*usd-row.Cost || b.Held != 0 {
		t.Fatalf("balance %+v for cost %d", b, row.Cost)
	}
	if s := e.snapshot(""); s.DailySpent != row.Cost { // the budget window holds what was spent
		t.Fatalf("window %d, cost %d", s.DailySpent, row.Cost)
	}

	// Refused on the very next request, with the rule id, before anything else.
	calls := e.fake.CallCount()
	for _, body := range []string{chatBody("oa/m", false), `not json`} {
		r2, b := e.do("/v1/chat/completions", body, nil)
		if r2.StatusCode != 403 || errCode(jsonBody(t, b)) != policy.RuleKilled {
			t.Fatalf("after kill: %d %s", r2.StatusCode, b)
		}
		if d := e.decisions(r2); len(d) != 1 || d[0].Rule != policy.RuleKilled || d[0].Verdict != policy.Deny {
			t.Fatalf("%+v", d)
		}
	}
	if e.fake.CallCount() != calls {
		t.Fatal("a killed agent reached upstream")
	}
	// Resume restores it.
	if _, err := e.srv.Agents.Resume(context.Background(), e.agentID()); err != nil {
		t.Fatal(err)
	}
	e.fake.SetDefault(fakeupstream.Script{})
	if r3, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil); r3.StatusCode != 200 {
		t.Fatalf("after resume: %d %s", r3.StatusCode, b)
	}
}

// The same for a non-streaming call waiting on a slow upstream, and for the
// in-process Kill.
func TestKillCancelsUnaryCall(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.watchKills()
	e.fake.SetDefault(fakeupstream.Script{Latency: 5 * time.Second})
	type result struct {
		resp *http.Response
		b    []byte
		at   time.Time
	}
	done := make(chan result, 1)
	go func() {
		resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
		done <- result{resp, b, time.Now()}
	}()
	for i := 0; i < 100 && e.fake.CallCount() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	killedAt := time.Now()
	if err := e.srv.Kill(context.Background(), e.agentID()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if d := r.at.Sub(killedAt); d > time.Second || r.resp.StatusCode != 403 || errCode(jsonBody(t, r.b)) != policy.RuleKilled {
			t.Fatalf("%v %d %s", d, r.resp.StatusCode, r.b)
		}
		if row := e.auditRow(r.resp); row.Status != server.StatusKilled {
			t.Fatalf("%+v", row)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call was not cancelled")
	}
	e.noActiveHolds()
	if e.balance().Balance != 10*usd {
		t.Fatal("charged for a cancelled call")
	}
	if s := e.snapshot(""); s.DailySpent != 0 {
		t.Fatalf("window %d", s.DailySpent)
	}
}

// Revoke: refused at once on the next request, and in-flight calls are cancelled.
func TestRevokeRefusesImmediately(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	e.watchKills()
	if resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	e.fake.SetDefault(fakeupstream.Script{Latency: 5 * time.Second})
	inflight := make(chan int, 1)
	go func() {
		resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
		inflight <- resp.StatusCode
	}()
	for i := 0; i < 100 && e.fake.CallCount() < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := e.srv.Agents.Revoke(context.Background(), e.agentID()); err != nil {
		t.Fatal(err)
	}
	resp, b := e.do("/v1/chat/completions", chatBody("oa/m", false), nil)
	if resp.StatusCode != 403 || errCode(jsonBody(t, b)) != policy.RuleRevoked {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if d := e.decisions(resp); len(d) != 1 || d[0].Rule != policy.RuleRevoked {
		t.Fatalf("%+v", d)
	}
	select {
	case code := <-inflight:
		if code != 403 {
			t.Fatalf("in-flight call ended with %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight call survived the revoke")
	}
	// Resume cannot undo a revoke.
	if _, err := e.srv.Agents.Resume(context.Background(), e.agentID()); err == nil {
		t.Fatal("resumed a revoked agent")
	}
	e.noActiveHolds()
}

// An unknown or wrong credential is still a plain 401.
func TestUnknownCredentialIs401(t *testing.T) {
	e := newEnv(t, usd, nil)
	resp, _ := e.do("/v1/chat/completions", chatBody("oa/m", false), map[string]string{"Authorization": "Bearer ag_nope"})
	if resp.StatusCode != 401 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

// The reconciler gives the budget back for a hold whose process died (it never
// ran CloseCharge), and charges the real amount when the hold is settled from usage.
func TestReconcilerClosesBudgetCharge(t *testing.T) {
	e := newEnv(t, 10*usd, nil)
	ctx := context.Background()
	est := estimate(chatBody("oa/m", false))
	e.setScope(func(s *policy.Scope) { s.DailyBudget = est * 4 })
	aid := e.agentID()
	snap := e.snapshot("")

	// A request that died after admission: hold + charge, nothing else.
	h, err := e.led.Reserve(ctx, ledger.ReserveReq{AccountID: e.acct, Amount: est, IdemKey: "dead-1", Kind: ledger.KindModel, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rule, err := e.srv.Agents.Charge(ctx, agents.ChargeReq{HoldID: h.ID, AgentID: aid, Amount: est, Scope: snap.Scope}); rule != "" || err != nil {
		t.Fatalf("%s %v", rule, err)
	}
	before := e.snapshot("").DailySpent
	if before < est {
		t.Fatalf("charge not counted: %d", before)
	}
	rep, err := server.Reconcile(ctx, e.led, noLookup{}, e.audit, e.srv.Agents, time.Now().Add(time.Hour))
	if err != nil || rep.Released != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if got := e.snapshot("").DailySpent; got != before-est {
		t.Fatalf("daily spent %d after reconcile, want %d", got, before-est)
	}
	e.noActiveHolds()
}
