package approval_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

type rig struct {
	t     *testing.T
	q     *approval.Queue
	led   *ledger.Postgres
	acct  string
	agent agents.Agent
	now   time.Time
	seq   int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pool := dbtest.New(t)
	r := &rig{t: t, led: ledger.New(pool), now: time.Now()}
	ctx := context.Background()
	var err error
	if r.acct, err = r.led.CreateAccount(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := r.led.Deposit(ctx, r.acct, 100*money.Nanos(1e9), "dep", ""); err != nil {
		t.Fatal(err)
	}
	if _, r.agent, err = agents.NewStore(pool).Create(ctx, r.acct, "bot", policy.DefaultScope()); err != nil {
		t.Fatal(err)
	}
	st := approval.NewStore(pool)
	st.SetClock(func() time.Time { return r.now })
	r.q = &approval.Queue{Store: st, Ledger: r.led}
	return r
}

// queue creates a hold and a pending approval for it, as the gateway does.
func (r *rig) queue(ttl time.Duration) approval.Approval {
	r.t.Helper()
	r.seq++
	key := fmt.Sprint("k-", r.seq)
	h, err := r.led.Reserve(context.Background(), ledger.ReserveReq{AccountID: r.acct, Amount: 5, IdemKey: key, Kind: ledger.KindModel, TTL: time.Hour})
	if err != nil {
		r.t.Fatal(err)
	}
	a, created, err := r.q.Create(context.Background(), approval.Approval{AgentID: r.agent.ID, AccountID: r.acct, IdemKey: key,
		RequestID: "req", Kind: "model", Subject: "m", Amount: 5, Fingerprint: "fp", HoldID: h.ID}, ttl)
	if err != nil || !created {
		r.t.Fatalf("%v created=%v", err, created)
	}
	return a
}

func (r *rig) held() money.Nanos {
	b, _ := r.led.Balance(context.Background(), r.acct)
	return b.Held
}

func TestFingerprintDistinguishesFieldBoundaries(t *testing.T) {
	if approval.Fingerprint("ab", "c") == approval.Fingerprint("a", "bc") {
		t.Fatal("fields run together")
	}
	if approval.Fingerprint("a", "b") != approval.Fingerprint("a", "b") {
		t.Fatal("not deterministic")
	}
	if approval.Fingerprint("a") == approval.Fingerprint("a", "") {
		t.Fatal("trailing empty field ignored")
	}
}

func TestCreateIsIdempotentPerKey(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.queue(time.Hour)
	again, created, err := r.q.Create(ctx, approval.Approval{AgentID: r.agent.ID, AccountID: r.acct, IdemKey: a.IdemKey, RequestID: "other",
		Kind: "model", Fingerprint: "different", HoldID: a.HoldID}, time.Hour)
	if err != nil || created || again.ID != a.ID || again.Fingerprint != "fp" {
		t.Fatalf("%+v created=%v %v", again, created, err)
	}
	if got, found, _ := r.q.Find(ctx, r.agent.ID, a.IdemKey); !found || got.ID != a.ID || got.Status != approval.Pending {
		t.Fatalf("%+v %v", got, found)
	}
	if _, found, _ := r.q.Find(ctx, r.agent.ID, "nope"); found {
		t.Fatal("found a ghost")
	}
}

func TestAllowRecordsApproverAndNeedsAName(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.queue(time.Hour)
	if _, err := r.q.Allow(ctx, a.ID, ""); err == nil {
		t.Fatal("anonymous approval accepted")
	}
	got, err := r.q.Allow(ctx, a.ID, "alice")
	if err != nil || got.Status != approval.Approved || got.DecidedBy != "alice" || got.DecidedAt == nil {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := r.q.Allow(ctx, a.ID, "bob"); err == nil { // already decided
		t.Fatal("decided twice")
	}
	if _, err := r.q.Allow(ctx, "nope", "alice"); err != approval.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if r.held() != 5 {
		t.Fatalf("held %d: approval must keep the hold", r.held())
	}
}

// Allow and Deny pressed at once: one wins, the other fails, and the hold is
// released only if Deny won.
func TestAllowDenyRace(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		a := r.queue(time.Hour)
		var wg sync.WaitGroup
		var wins atomic.Int64
		var allowWon atomic.Bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := r.q.Allow(ctx, a.ID, "alice"); err == nil {
				wins.Add(1)
				allowWon.Store(true)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := r.q.Deny(ctx, a.ID, "bob"); err == nil {
				wins.Add(1)
			}
		}()
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("round %d: %d decisions won", i, wins.Load())
		}
		got, _ := r.q.Get(ctx, a.ID)
		if allowWon.Load() != (got.Status == approval.Approved) {
			t.Fatalf("status %s but allowWon=%v", got.Status, allowWon.Load())
		}
		if err := r.led.Audit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConsumeOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.queue(time.Hour)
	if ok, _ := r.q.Consume(ctx, a.ID); ok {
		t.Fatal("consumed a pending approval")
	}
	if _, err := r.q.Allow(ctx, a.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := r.q.Consume(ctx, a.ID); err != nil {
				t.Error(err)
			} else if ok {
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("consumed %d times", n.Load())
	}
}

func TestDenyReleasesHold(t *testing.T) {
	r := newRig(t)
	a := r.queue(time.Hour)
	if r.held() != 5 {
		t.Fatalf("held %d", r.held())
	}
	if got, err := r.q.Deny(context.Background(), a.ID, "bob"); err != nil || got.Status != approval.Denied {
		t.Fatalf("%+v %v", got, err)
	}
	if r.held() != 0 {
		t.Fatalf("held %d after deny", r.held())
	}
	if ok, _ := r.q.Consume(context.Background(), a.ID); ok {
		t.Fatal("consumed a denied approval")
	}
}

func TestExpiry(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	pending := r.queue(time.Minute)
	approved := r.queue(time.Minute)
	live := r.queue(time.Hour)
	if _, err := r.q.Allow(ctx, approved.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Minute)

	if _, err := r.q.Allow(ctx, pending.ID, "alice"); err != approval.ErrExpired {
		t.Fatalf("allow after expiry: %v", err)
	}
	// An approval that was granted but not used in time cannot be used.
	if ok, _ := r.q.Consume(ctx, approved.ID); ok {
		t.Fatal("consumed an expired approval")
	}
	// The failed Allow already swept everything that was due, approved-but-unused included.
	if n, err := r.q.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
	for _, c := range []struct {
		id   string
		want approval.Status
	}{{pending.ID, approval.Expired}, {approved.ID, approval.Expired}, {live.ID, approval.Pending}} {
		if got, _ := r.q.Get(ctx, c.id); got.Status != c.want {
			t.Fatalf("%s: %s, want %s", c.id, got.Status, c.want)
		}
	}
	if r.held() != 5 { // only the live one keeps its hold
		t.Fatalf("held %d", r.held())
	}
	if err := r.led.Audit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestListByStatus(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a, b := r.queue(time.Hour), r.queue(time.Hour)
	_, _ = r.q.Allow(ctx, b.ID, "alice")
	if l, _ := r.q.List(ctx, approval.Pending); len(l) != 1 || l[0].ID != a.ID {
		t.Fatalf("%+v", l)
	}
	if l, _ := r.q.List(ctx, ""); len(l) != 2 {
		t.Fatalf("%+v", l)
	}
}
