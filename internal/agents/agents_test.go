package agents_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

type rig struct {
	t    *testing.T
	pool *pgxpool.Pool
	led  *ledger.Postgres
	st   *agents.Store
	acct string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pool := dbtest.New(t)
	r := &rig{t: t, pool: pool, led: ledger.New(pool), st: agents.NewStore(pool)}
	var err error
	if r.acct, err = r.led.CreateAccount(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := r.led.Deposit(context.Background(), r.acct, 1000*money.Nanos(1e9), "dep", ""); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) agent(name string, sc policy.Scope) (string, agents.Agent) {
	r.t.Helper()
	secret, a, err := r.st.Create(context.Background(), r.acct, name, sc)
	if err != nil {
		r.t.Fatal(err)
	}
	return secret, a
}

var holdSeq atomic.Int64

// hold makes a real ledger hold: budget_charges references holds.
func (r *rig) hold(amount money.Nanos) string {
	r.t.Helper()
	h, err := r.led.Reserve(context.Background(), ledger.ReserveReq{AccountID: r.acct, Amount: amount,
		IdemKey: fmt.Sprintf("k-%d", holdSeq.Add(1)), Kind: ledger.KindModel, TTL: time.Hour})
	if err != nil {
		r.t.Fatal(err)
	}
	return h.ID
}

func (r *rig) charge(a agents.Agent, task string, amount money.Nanos) (string, error) {
	return r.st.Charge(context.Background(), agents.ChargeReq{HoldID: r.hold(amount), AgentID: a.ID, TaskID: task, Amount: amount, Scope: a.Scope})
}

func TestCreateAuthenticateAndStatus(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	secret, a := r.agent("bot", policy.Scope{Models: []string{"m"}, DailyBudget: 5})
	if secret[:3] != "ag_" || len(secret) < 40 {
		t.Fatalf("secret %q", secret)
	}
	got, err := r.st.Authenticate(ctx, secret)
	if err != nil || got.ID != a.ID || got.AccountID != r.acct || got.Scope.DailyBudget != 5 || got.Status != policy.StatusActive {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := r.st.Authenticate(ctx, secret+"x"); err != agents.ErrUnauthorized {
		t.Fatalf("wrong secret: %v", err)
	}
	if _, err := r.st.Authenticate(ctx, ""); err != agents.ErrUnauthorized {
		t.Fatalf("empty secret: %v", err)
	}
	if _, _, err := r.st.Create(ctx, r.acct, "bot", policy.Scope{}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, _, err := r.st.Create(ctx, r.acct, "bad name!", policy.Scope{}); err != agents.ErrBadName {
		t.Fatalf("bad name: %v", err)
	}
	// I9: only the hash is stored.
	var dump string
	_ = r.pool.QueryRow(ctx, `SELECT string_agg(t::text, ' ') FROM agents t`).Scan(&dump)
	if dump == "" || contains(dump, secret) {
		t.Fatal("secret found in the agents table")
	}

	// kill -> still authenticates (policy refuses it), resume, revoke is final.
	if _, err := r.st.Kill(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.st.Authenticate(ctx, secret); got.Status != policy.StatusKilled {
		t.Fatalf("status %s", got.Status)
	}
	if _, err := r.st.Kill(ctx, a.ID); err != nil { // idempotent
		t.Fatal(err)
	}
	if ids, _ := r.st.NotActive(ctx, []string{a.ID}); len(ids) != 1 {
		t.Fatalf("NotActive %v", ids)
	}
	if _, err := r.st.Resume(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	if ids, _ := r.st.NotActive(ctx, []string{a.ID}); len(ids) != 0 {
		t.Fatalf("NotActive after resume %v", ids)
	}
	if _, err := r.st.Revoke(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.Revoke(ctx, "bot"); err != nil { // idempotent
		t.Fatal(err)
	}
	if _, err := r.st.Resume(ctx, "bot"); err == nil {
		t.Fatal("a revoked agent was resumed")
	}
	if _, err := r.st.Kill(ctx, "bot"); err == nil {
		t.Fatal("a revoked agent was 'killed' back to killed")
	}
	if _, err := r.st.Get(ctx, "nobody"); err != agents.ErrNotFound {
		t.Fatalf("get: %v", err)
	}
	if list, _ := r.st.List(ctx); len(list) != 1 || list[0].Status != policy.StatusRevoked {
		t.Fatalf("list %+v", list)
	}
}

func TestSetScope(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	secret, a := r.agent("bot", policy.Scope{Models: []string{"m"}, DailyBudget: 5})
	got, err := r.st.SetScope(ctx, "bot", policy.Scope{Models: []string{"m", "n"}, Tools: []string{"t"}, DailyBudget: 9})
	if err != nil || got.ID != a.ID || got.Scope.DailyBudget != 9 || len(got.Scope.Models) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	snap, err := r.st.Snapshot(ctx, a.ID, "")
	if err != nil || snap.Scope.DailyBudget != 9 || snap.Scope.Tools[0] != "t" {
		t.Fatalf("the next request does not see the new scope: %+v %v", snap.Scope, err)
	}
	if au, _ := r.st.Authenticate(ctx, secret); au.Scope.DailyBudget != 9 {
		t.Fatalf("authenticate scope %+v", au.Scope)
	}
	if _, err := r.st.SetScope(ctx, "nobody", policy.Scope{}); err != agents.ErrNotFound {
		t.Fatalf("missing agent: %v", err)
	}
	if _, err := r.st.Revoke(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.SetScope(ctx, "bot", policy.DefaultScope()); !errors.Is(err, agents.ErrBadState) {
		t.Fatalf("a revoked agent's scope changed: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// A migrated T2 key keeps working: same secret, default scope.
func TestMigrationSeedsAgentsFromAPIKeys(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// Rebuild the world as it was before 0005: drop the new tables, add two
	// legacy keys, and run the migration file again.
	if _, err := r.pool.Exec(ctx, `DROP TABLE policy_decisions, approvals, budget_charges, budget_windows, agents CASCADE;
		DROP FUNCTION policy_decisions_append_only()`); err != nil {
		t.Fatal(err)
	}
	h1, h2 := []byte("0123456789abcdef0123456789abcdef"), []byte("fedcba9876543210fedcba9876543210")
	if _, err := r.pool.Exec(ctx, `INSERT INTO api_keys (id, account_id, key_hash, label) VALUES ('k1', $1, $2, 'live')`, r.acct, h1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO api_keys (id, account_id, key_hash, revoked_at) VALUES ('k2', $1, $2, now())`, r.acct, h2); err != nil {
		t.Fatal(err)
	}
	body := readMigration(t)
	if _, err := r.pool.Exec(ctx, body, simpleProtocol); err != nil {
		t.Fatal(err)
	}
	rows, err := r.pool.Query(ctx, `SELECT id, account_id, status, scope::text FROM agents ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, acct, st, sc string
		_ = rows.Scan(&id, &acct, &st, &sc)
		if acct != r.acct {
			t.Fatalf("account %s", acct)
		}
		got = append(got, id+":"+st+":"+sc)
	}
	want := []string{`k1:active:{"tools": ["*"], "models": ["*"]}`, `k2:revoked:{"tools": ["*"], "models": ["*"]}`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

// I5 for budgets: N parallel charges against one daily window never push it
// over the limit, and the counter equals what was admitted.
func TestChargeDailyBudgetRace(t *testing.T) {
	r := newRig(t)
	const limit, each, n = 50, 7, 40 // room for 7 of 40
	_, a := r.agent("racer", policy.Scope{DailyBudget: limit})
	holds := make([]string, n)
	for i := range holds {
		holds[i] = r.hold(each)
	}
	var ok, denied atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rule, err := r.st.Charge(context.Background(), agents.ChargeReq{HoldID: holds[i], AgentID: a.ID, Amount: each, Scope: a.Scope})
			switch {
			case err != nil:
				t.Error(err)
			case rule == "":
				ok.Add(1)
			case rule == policy.RuleBudgetDaily:
				denied.Add(1)
			default:
				t.Errorf("rule %s", rule)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != limit/each || ok.Load()+denied.Load() != n {
		t.Fatalf("admitted %d, denied %d", ok.Load(), denied.Load())
	}
	snap, err := r.st.Snapshot(context.Background(), a.ID, "")
	if err != nil || snap.DailySpent != money.Nanos(ok.Load()*each) || snap.DailySpent > limit {
		t.Fatalf("daily spent %d (%v)", snap.DailySpent, err)
	}
}

func TestChargeTaskBudgetRace(t *testing.T) {
	r := newRig(t)
	const limit, each = 30, 4
	_, a := r.agent("tasker", policy.Scope{TaskBudget: limit, DailyBudget: 1000})
	var ok atomic.Int64
	var wg sync.WaitGroup
	for _, task := range []string{"t1", "t2"} {
		for i := 0; i < 20; i++ {
			h := r.hold(each)
			wg.Add(1)
			go func() {
				defer wg.Done()
				rule, err := r.st.Charge(context.Background(), agents.ChargeReq{HoldID: h, AgentID: a.ID, TaskID: task, Amount: each, Scope: a.Scope})
				if err != nil {
					t.Error(err)
				}
				if rule == "" {
					ok.Add(1)
				} else if rule != policy.RuleBudgetTask {
					t.Errorf("rule %s", rule)
				}
			}()
		}
	}
	wg.Wait()
	// Each task window admits limit/each = 7; the two tasks are independent.
	if ok.Load() != 2*(limit/each) {
		t.Fatalf("admitted %d", ok.Load())
	}
	for _, task := range []string{"t1", "t2"} {
		snap, _ := r.st.Snapshot(context.Background(), a.ID, task)
		if snap.TaskSpent != money.Nanos(limit/each*each) {
			t.Fatalf("%s spent %d", task, snap.TaskSpent)
		}
	}
	// Both tasks' spend also counted in the daily window.
	if snap, _ := r.st.Snapshot(context.Background(), a.ID, "t1"); snap.DailySpent != money.Nanos(2*(limit/each)*each) {
		t.Fatalf("daily %d", snap.DailySpent)
	}
	// A request with no task header counts as the shared "default" task.
	if rule, _ := r.charge(a, "", each); rule != "" {
		t.Fatalf("default task: %s", rule)
	}
	if snap, _ := r.st.Snapshot(context.Background(), a.ID, ""); snap.TaskSpent != each {
		t.Fatalf("default task spent %d", snap.TaskSpent)
	}
}

// A refused request leaves no trace: all or none across the windows.
func TestChargeIsAllOrNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, a := r.agent("aon", policy.Scope{DailyBudget: 100, TaskBudget: 10})
	if rule, err := r.charge(a, "t", 11); err != nil || rule != policy.RuleBudgetTask {
		t.Fatalf("%s %v", rule, err)
	}
	snap, _ := r.st.Snapshot(ctx, a.ID, "t")
	if snap.DailySpent != 0 || snap.TaskSpent != 0 || snap.VelocitySpent != 0 {
		t.Fatalf("a refused charge left %+v", snap)
	}
	a.Scope.VelocityPerMinute = 5
	if rule, err := r.charge(a, "t", 8); err != nil || rule != policy.RuleVelocity {
		t.Fatalf("%s %v", rule, err)
	}
	snap, _ = r.st.Snapshot(ctx, a.ID, "t")
	if snap.DailySpent != 0 || snap.TaskSpent != 0 || snap.VelocitySpent != 0 {
		t.Fatalf("a velocity refusal left %+v", snap)
	}
}

// Velocity is a sliding window: spend ages out after a minute.
func TestVelocitySlidingWindow(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	now := time.Now()
	r.st.SetClock(func() time.Time { return now })
	_, a := r.agent("fast", policy.Scope{VelocityPerMinute: 10})
	for i, at := range []time.Duration{0, 20 * time.Second, 40 * time.Second} {
		now = time.Now().Add(at)
		_ = i
		if rule, err := r.charge(a, "", 4); (at < 40*time.Second && (rule != "" || err != nil)) || (at == 40*time.Second && rule != policy.RuleVelocity) {
			t.Fatalf("at %v: %s %v", at, rule, err)
		}
	}
	// 30s later the first charge (t0) has aged out of the window (t0+70 > 60).
	now = time.Now().Add(70 * time.Second)
	snap, _ := r.st.Snapshot(ctx, a.ID, "")
	if snap.VelocitySpent != 4 { // only the t+20s charge is still inside
		t.Fatalf("velocity spent %d", snap.VelocitySpent)
	}
	if rule, err := r.charge(a, "", 6); rule != "" || err != nil {
		t.Fatalf("after the window slid: %s %v", rule, err)
	}
}

func TestVelocityRace(t *testing.T) {
	r := newRig(t)
	const limit, each, n = 20, 3, 30
	_, a := r.agent("burst", policy.Scope{VelocityPerMinute: limit})
	holds := make([]string, n)
	for i := range holds {
		holds[i] = r.hold(each)
	}
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rule, err := r.st.Charge(context.Background(), agents.ChargeReq{HoldID: holds[i], AgentID: a.ID, Amount: each, Scope: a.Scope})
			if err != nil {
				t.Error(err)
			}
			if rule == "" {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != limit/each {
		t.Fatalf("admitted %d, want %d", ok.Load(), limit/each)
	}
}

// Closing a charge gives the window the real cost back, once.
func TestCloseChargeCorrectsWindowsOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, a := r.agent("closer", policy.Scope{DailyBudget: 100, TaskBudget: 100, VelocityPerMinute: 100})
	h := r.hold(40)
	if rule, err := r.st.Charge(ctx, agents.ChargeReq{HoldID: h, AgentID: a.ID, TaskID: "t", Amount: 40, Scope: a.Scope}); rule != "" || err != nil {
		t.Fatalf("%s %v", rule, err)
	}
	if err := r.st.CloseCharge(ctx, h, 15); err != nil {
		t.Fatal(err)
	}
	if err := r.st.CloseCharge(ctx, h, 0); err != nil { // second close: nothing happens
		t.Fatal(err)
	}
	snap, _ := r.st.Snapshot(ctx, a.ID, "t")
	if snap.DailySpent != 15 || snap.TaskSpent != 15 || snap.VelocitySpent != 15 {
		t.Fatalf("after settle-for-15: %+v", snap)
	}
	// Overage (I7-style): the real cost may exceed the estimate; the window follows.
	h2 := r.hold(10)
	_, _ = r.st.Charge(ctx, agents.ChargeReq{HoldID: h2, AgentID: a.ID, TaskID: "t", Amount: 10, Scope: a.Scope})
	_ = r.st.CloseCharge(ctx, h2, 12)
	if snap, _ = r.st.Snapshot(ctx, a.ID, "t"); snap.DailySpent != 27 {
		t.Fatalf("after overage: %+v", snap)
	}
	// A hold that was never charged: no-op, no error.
	if err := r.st.CloseCharge(ctx, r.hold(5), 0); err != nil {
		t.Fatal(err)
	}
	// Freed budget is usable again.
	if rule, _ := r.charge(a, "t", 70); rule != "" {
		t.Fatalf("budget not given back: %s", rule)
	}
}

func TestDecisionLogIsAppendOnly(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, a := r.agent("logger", policy.DefaultScope())
	for _, v := range []policy.Verdict{policy.Allow, policy.Deny, policy.RequireApproval} {
		if err := r.st.LogDecision(ctx, agents.Decision{RequestID: "r1", AgentID: a.ID, AccountID: r.acct, Kind: "model", Subject: "m",
			Amount: 5, Verdict: v, Rule: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.st.Decisions(ctx, "r1")
	if err != nil || len(got) != 3 || got[0].Verdict != policy.Allow || got[2].Verdict != policy.RequireApproval {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE policy_decisions SET rule='y'`); err == nil {
		t.Fatal("decision rows are mutable")
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM policy_decisions`); err == nil {
		t.Fatal("decision rows are deletable")
	}
	if _, err := r.pool.Exec(ctx, `TRUNCATE policy_decisions`); err == nil {
		t.Fatal("decision table is truncatable")
	}
}

func TestRegistryCancel(t *testing.T) {
	var reg agents.Registry
	ctx1, c1 := context.WithCancel(context.Background())
	ctx2, c2 := context.WithCancel(context.Background())
	ctx3, c3 := context.WithCancel(context.Background())
	rm1 := reg.Add("a", c1)
	reg.Add("a", c2)
	reg.Add("b", c3)
	if ids := reg.AgentIDs(); len(ids) != 2 {
		t.Fatalf("%v", ids)
	}
	rm1() // finished normally: not cancelled by a later kill
	if n := reg.Cancel("a"); n != 1 {
		t.Fatalf("cancelled %d", n)
	}
	if ctx1.Err() != nil || ctx2.Err() == nil || ctx3.Err() != nil {
		t.Fatal("wrong contexts cancelled")
	}
	if n := reg.Cancel("nobody"); n != 0 {
		t.Fatalf("cancelled %d", n)
	}
}

func TestRename(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	secret, a := r.agent("claude-code-1", policy.Scope{})
	r.agent("other", policy.Scope{})
	got, err := r.st.Rename(ctx, "claude-code-1", "claude-code")
	if err != nil || got.ID != a.ID || got.Name != "claude-code" {
		t.Fatalf("rename: %+v %v", got, err)
	}
	if au, err := r.st.Authenticate(ctx, secret); err != nil || au.Name != "claude-code" {
		t.Fatalf("the key must keep working: %+v %v", au, err)
	}
	if _, err := r.st.Rename(ctx, "claude-code", "other"); !errors.Is(err, agents.ErrNameTaken) {
		t.Fatalf("taken name: %v", err)
	}
	if _, err := r.st.Rename(ctx, "claude-code", "bad name"); !errors.Is(err, agents.ErrBadName) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := r.st.Rename(ctx, "nobody", "x"); !errors.Is(err, agents.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
