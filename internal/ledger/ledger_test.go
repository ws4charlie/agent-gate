package ledger_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
)

var ctx = context.Background()

func setup(t testing.TB) (*ledger.Postgres, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.New(t)
	return ledger.New(pool), pool
}

func funded(t testing.TB, l *ledger.Postgres, amount money.Nanos) string {
	t.Helper()
	id, err := l.CreateAccount(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Deposit(ctx, id, amount, "dep-0", "seed"); err != nil {
		t.Fatal(err)
	}
	return id
}

func reserve(t testing.TB, l *ledger.Postgres, acct, key string, amt money.Nanos) ledger.Hold {
	t.Helper()
	h, err := l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: amt, IdemKey: key, Kind: ledger.KindModel, TTL: time.Minute})
	if err != nil {
		t.Fatalf("reserve %s: %v", key, err)
	}
	return h
}

func mustAudit(t testing.TB, l *ledger.Postgres) {
	t.Helper()
	if err := l.Audit(ctx); err != nil {
		t.Fatalf("audit: %v", err)
	}
}

func bal(t testing.TB, l *ledger.Postgres, acct string) ledger.Balance {
	t.Helper()
	b, err := l.Balance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func entryCount(t testing.TB, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// I3, I4: every mutation replayed 100 times changes nothing and returns the
// first result.
func TestReplay_I3_I4(t *testing.T) {
	l, pool := setup(t)
	acct := funded(t, l, 1000)
	mustAudit(t, l)

	// Deposit
	for i := 0; i < 100; i++ {
		if err := l.Deposit(ctx, acct, 1000, "dep-0", "seed"); err != nil {
			t.Fatalf("deposit replay %d: %v", i, err)
		}
	}
	// Reserve
	h := reserve(t, l, acct, "r1", 300)
	for i := 0; i < 100; i++ {
		got := reserve(t, l, acct, "r1", 300)
		if got.ID != h.ID || !got.ExpiresAt.Equal(h.ExpiresAt) {
			t.Fatalf("reserve replay %d returned %+v want %+v", i, got, h)
		}
	}
	// AttachUpstream is naturally idempotent.
	for i := 0; i < 100; i++ {
		if err := l.AttachUpstream(ctx, h.ID, "fake", "up-1"); err != nil {
			t.Fatal(err)
		}
	}
	// Settle
	first, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 120, IdemKey: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		got, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 120, IdemKey: "s1"})
		if err != nil || got != first {
			t.Fatalf("settle replay %d: %+v %v want %+v", i, got, err, first)
		}
	}
	// Release
	h2 := reserve(t, l, acct, "r2", 200)
	rel, err := l.Release(ctx, h2.ID, "rel1", "client cancelled")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		got, err := l.Release(ctx, h2.ID, "rel1", "client cancelled")
		if err != nil || got != rel {
			t.Fatalf("release replay %d: %+v %v want %+v", i, got, err, rel)
		}
	}
	// Reconcile: a settle and a release, then 100 more passes find nothing.
	hs := reserve(t, l, acct, "r3", 100)
	if err := l.AttachUpstream(ctx, hs.ID, "fake", "up-3"); err != nil {
		t.Fatal(err)
	}
	hr := reserve(t, l, acct, "r4", 100)
	late := time.Now().Add(time.Hour)
	lk := fakeLookup{"up-3": 40}
	rep, err := l.Reconcile(ctx, lk, late)
	if err != nil || rep != (ledger.Report{Settled: 1, Released: 1}) {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	for i := 0; i < 100; i++ {
		rep, err := l.Reconcile(ctx, lk, late)
		if err != nil || rep != (ledger.Report{}) {
			t.Fatalf("reconcile replay %d: %+v %v", i, rep, err)
		}
	}
	// The reconciler's own keys replay too.
	if got, err := l.Settle(ctx, ledger.SettleReq{HoldID: hs.ID, Actual: 40, IdemKey: "reconcile:" + hs.ID}); err != nil || got.Charged != 40 {
		t.Fatalf("settle replay with reconcile key: %+v %v", got, err)
	}
	if got, err := l.Release(ctx, hr.ID, "reconcile:"+hr.ID, "x"); err != nil || got.Released != 100 {
		t.Fatalf("release replay with reconcile key: %+v %v", got, err)
	}

	// 1 deposit + 4 holds + (settle,release) + release + (settle,release) + release.
	if n := entryCount(t, pool); n != 1+4+2+1+2+1 {
		t.Fatalf("replays wrote extra entries: %d", n)
	}
	b := bal(t, l, acct)
	if b.Balance != 1000-120-40 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	mustAudit(t, l)
}

// I4: same key, different parameters is a conflict, and writes nothing.
func TestIdempotencyConflict_I4(t *testing.T) {
	l, pool := setup(t)
	acct := funded(t, l, 1000)
	h := reserve(t, l, acct, "r1", 300)
	n := entryCount(t, pool)

	if err := l.Deposit(ctx, acct, 999, "dep-0", ""); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("deposit: %v", err)
	}
	_, err := l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: 301, IdemKey: "r1", Kind: ledger.KindModel, TTL: time.Minute})
	if !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("reserve amount: %v", err)
	}
	_, err = l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: 300, IdemKey: "r1", Kind: ledger.KindTool, TTL: time.Minute})
	if !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("reserve kind: %v", err)
	}
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 50, IdemKey: "s1"}); err != nil {
		t.Fatal(err)
	}
	n = entryCount(t, pool)
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 51, IdemKey: "s1"}); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("settle: %v", err)
	}
	// A settle key already used on another hold is also a conflict.
	h2 := reserve(t, l, acct, "r2", 100)
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: h2.ID, Actual: 50, IdemKey: "s1"}); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("settle key reuse across holds: %v", err)
	}
	if got := entryCount(t, pool); got != n+1 { // only reserve r2's hold entry
		t.Fatalf("conflicts wrote entries: %d -> %d", n, got)
	}
	mustAudit(t, l)
}

// I3: a terminal hold stays terminal under any other key.
func TestSingleTerminalState_I3(t *testing.T) {
	l, _ := setup(t)
	acct := funded(t, l, 1000)
	h := reserve(t, l, acct, "r1", 300)
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 100, IdemKey: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 100, IdemKey: "s2"}); !errors.Is(err, ledger.ErrHoldNotActive) {
		t.Fatalf("settle again, other key: %v", err)
	}
	if _, err := l.Release(ctx, h.ID, "rel", "x"); !errors.Is(err, ledger.ErrHoldNotActive) {
		t.Fatalf("release settled hold: %v", err)
	}
	if _, err := l.Release(ctx, h.ID, "s1", "x"); !errors.Is(err, ledger.ErrHoldNotActive) {
		t.Fatalf("release settled hold with the settle key: %v", err)
	}
	if err := l.Extend(ctx, h.ID, time.Minute); !errors.Is(err, ledger.ErrHoldNotActive) {
		t.Fatalf("extend settled hold: %v", err)
	}
	h2 := reserve(t, l, acct, "r2", 100)
	if _, err := l.Release(ctx, h2.ID, "rel2", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: h2.ID, Actual: 1, IdemKey: "s3"}); !errors.Is(err, ledger.ErrHoldNotActive) {
		t.Fatalf("settle released hold: %v", err)
	}
	if _, err := l.Release(ctx, h2.ID, "rel3", "x"); !errors.Is(err, ledger.ErrHoldNotActive) {
		t.Fatalf("release again, other key: %v", err)
	}
	if err := l.Extend(ctx, "no-such-hold", time.Minute); !errors.Is(err, ledger.ErrHoldNotFound) {
		t.Fatalf("extend unknown: %v", err)
	}
	mustAudit(t, l)
}

// I1: the database itself refuses an insolvent state, not just the Go code.
func TestSolvencyEnforcedByDB_I1(t *testing.T) {
	l, pool := setup(t)
	acct := funded(t, l, 100)
	if _, err := pool.Exec(ctx, `UPDATE accounts SET held_nanos = 101 WHERE id=$1`, acct); err == nil {
		t.Fatal("held > balance was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE accounts SET held_nanos = -1 WHERE id=$1`, acct); err == nil {
		t.Fatal("negative held was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET amount_nanos = 1`); err == nil {
		t.Fatal("ledger_entries accepted an UPDATE")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries`); err == nil {
		t.Fatal("ledger_entries accepted a DELETE")
	}
	_, err := l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: 101, IdemKey: "big", Kind: ledger.KindModel, TTL: time.Minute})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("over-reserve: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE accounts SET status='suspended' WHERE id=$1`, acct); err != nil {
		t.Fatal(err)
	}
	_, err = l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: 1, IdemKey: "sus", Kind: ledger.KindModel, TTL: time.Minute})
	if !errors.Is(err, ledger.ErrAccountInactive) {
		t.Fatalf("suspended: %v", err)
	}
	mustAudit(t, l)
}

// I2: Audit really detects drift (it is not vacuous).
func TestAuditDetectsDrift_I2(t *testing.T) {
	l, pool := setup(t)
	acct := funded(t, l, 100)
	mustAudit(t, l)
	if _, err := pool.Exec(ctx, `UPDATE accounts SET balance_nanos = balance_nanos + 1 WHERE id=$1`, acct); err != nil {
		t.Fatal(err)
	}
	err := l.Audit(ctx)
	if err == nil || !strings.Contains(err.Error(), "I2") {
		t.Fatalf("audit missed drift: %v", err)
	}
}

// I7: overage is charged up to hold + free balance; the rest is recorded.
func TestOverage_I7(t *testing.T) {
	l, pool := setup(t)

	// With free balance: balance 1000, hold 100, actual 250 -> charged 250.
	a1 := funded(t, l, 1000)
	h := reserve(t, l, a1, "r", 100)
	s, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 250, IdemKey: "s"})
	if err != nil || s != (ledger.Settlement{HoldID: h.ID, Charged: 250}) {
		t.Fatalf("with free balance: %+v %v", s, err)
	}
	if b := bal(t, l, a1); b.Balance != 750 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}

	// Without free balance: balance == hold, actual 250 -> charged 100, 150 unrecovered.
	a2 := funded(t, l, 100)
	h = reserve(t, l, a2, "r", 100)
	s, err = l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 250, IdemKey: "s"})
	if err != nil || s != (ledger.Settlement{HoldID: h.ID, Charged: 100, Unrecovered: 150}) {
		t.Fatalf("without free balance: %+v %v", s, err)
	}
	if b := bal(t, l, a2); b.Balance != 0 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	// Replay returns the same capped result, and the flag is on the audit row.
	if again, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 250, IdemKey: "s"}); err != nil || again != s {
		t.Fatalf("replay: %+v %v", again, err)
	}
	var memo string
	var unrec int64
	if err := pool.QueryRow(ctx, `SELECT e.memo, h.unrecovered_nanos FROM ledger_entries e JOIN holds h ON h.id=e.hold_id
		WHERE e.hold_id=$1 AND e.kind='settle'`, h.ID).Scan(&memo, &unrec); err != nil {
		t.Fatal(err)
	}
	if unrec != 150 || !strings.Contains(memo, "unrecovered=150") {
		t.Fatalf("unrecovered not flagged: %q %d", memo, unrec)
	}

	// Partial free balance, other holds' money is not free: balance 300,
	// hold A 100, hold B 150 (free 50), actual A 400 -> may take 100+50.
	a3 := funded(t, l, 300)
	ha := reserve(t, l, a3, "a", 100)
	hb := reserve(t, l, a3, "b", 150)
	s, err = l.Settle(ctx, ledger.SettleReq{HoldID: ha.ID, Actual: 400, IdemKey: "s"})
	if err != nil || s.Charged != 150 || s.Unrecovered != 250 {
		t.Fatalf("partial: %+v %v", s, err)
	}
	if b := bal(t, l, a3); b.Balance != 150 || b.Held != 150 {
		t.Fatalf("balance %+v", b)
	}
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: hb.ID, Actual: 150, IdemKey: "sb"}); err != nil {
		t.Fatal(err)
	}
	mustAudit(t, l)
}

// I3, I4: settle and release produce distinct charged and released entries.
func TestSettleWritesDistinctEntries_I3(t *testing.T) {
	l, pool := setup(t)
	acct := funded(t, l, 1000)
	h := reserve(t, l, acct, "r", 300)
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 120, IdemKey: "s"}); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT kind, amount_nanos, balance_after, held_after FROM ledger_entries WHERE hold_id=$1 ORDER BY id`, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var k string
		var a, b, hd int64
		if err := rows.Scan(&k, &a, &b, &hd); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %d bal=%d held=%d", k, a, b, hd))
	}
	want := []string{"hold 300 bal=1000 held=300", "settle 120 bal=880 held=180", "release 180 bal=880 held=0"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries:\n got %q\nwant %q", got, want)
	}
}

// I4: a replayed Reserve returns exactly the first ExpiresAt even when the
// clock has sub-microsecond precision (Postgres stores microseconds).
func TestReserveReplayExpiresAtPrecision_I4(t *testing.T) {
	l, _ := setup(t)
	l = l.WithClock(func() time.Time { return time.Unix(1_800_000_000, 123_456_789) })
	acct := funded(t, l, 100)
	first := reserve(t, l, acct, "r", 10)
	again := reserve(t, l, acct, "r", 10)
	if !first.ExpiresAt.Equal(again.ExpiresAt) {
		t.Fatalf("first %v != replay %v", first.ExpiresAt, again.ExpiresAt)
	}
}

func TestExtend(t *testing.T) {
	var clock = time.Now()
	l, pool := setup(t)
	l = l.WithClock(func() time.Time { return clock })
	acct := funded(t, l, 100)
	h := reserve(t, l, acct, "r", 10) // expires clock+1m
	clock = clock.Add(30 * time.Second)
	if err := l.Extend(ctx, h.ID, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	var exp time.Time
	_ = pool.QueryRow(ctx, `SELECT expires_at FROM holds WHERE id=$1`, h.ID).Scan(&exp)
	if want := clock.Add(10 * time.Minute); exp.Sub(want).Abs() > time.Millisecond {
		t.Fatalf("expires_at %v want %v", exp, want)
	}
	// A shorter extend never shortens the lease.
	if err := l.Extend(ctx, h.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	var exp2 time.Time
	_ = pool.QueryRow(ctx, `SELECT expires_at FROM holds WHERE id=$1`, h.ID).Scan(&exp2)
	if !exp2.Equal(exp) {
		t.Fatalf("shortened: %v -> %v", exp, exp2)
	}
}

type fakeLookup map[string]money.Nanos

func (f fakeLookup) Lookup(_ context.Context, _, upstreamID string) (ledger.Usage, bool, error) {
	c, ok := f[upstreamID]
	return ledger.Usage{Cost: c, Source: "fake"}, ok, nil
}

// I5: 50 goroutines race for a balance that fits only 20 of them.
func TestConcurrentReserve_I5(t *testing.T) {
	l, _ := setup(t)
	acct := funded(t, l, 200)
	const n = 50
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: 10, IdemKey: fmt.Sprint("k", i), Kind: ledger.KindModel, TTL: time.Minute})
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ledger.ErrInsufficientFunds):
		default:
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	b := bal(t, l, acct)
	if ok != 20 || b.Held != 200 || b.Held > b.Balance {
		t.Fatalf("ok=%d balance=%+v", ok, b)
	}
	mustAudit(t, l)
}

// I5: random balance, amounts and fan-out; held never exceeds balance and
// equals exactly what was granted.
func TestConcurrentReserveProperty_I5(t *testing.T) {
	l, _ := setup(t)
	rapid.Check(t, func(rt *rapid.T) {
		balance := money.Nanos(rapid.Int64Range(1, 1000).Draw(rt, "balance"))
		amounts := rapid.SliceOfN(rapid.Int64Range(1, 200), 2, 30).Draw(rt, "amounts")
		acct, err := l.CreateAccount(ctx, "p")
		if err != nil {
			rt.Fatal(err)
		}
		if err := l.Deposit(ctx, acct, balance, "d", ""); err != nil {
			rt.Fatal(err)
		}
		granted := make([]bool, len(amounts))
		fails := make([]error, len(amounts))
		var wg sync.WaitGroup
		for i, a := range amounts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: money.Nanos(a), IdemKey: fmt.Sprint("k", i), Kind: ledger.KindModel, TTL: time.Minute})
				granted[i] = err == nil
				if err != nil && !errors.Is(err, ledger.ErrInsufficientFunds) {
					fails[i] = err
				}
			}()
		}
		wg.Wait()
		var sum money.Nanos
		for i, a := range amounts {
			if fails[i] != nil {
				rt.Fatalf("reserve %d: %v", i, fails[i])
			}
			if granted[i] {
				sum += money.Nanos(a)
			}
		}
		b, err := l.Balance(ctx, acct)
		if err != nil {
			rt.Fatal(err)
		}
		if b.Held > b.Balance || b.Held != sum {
			rt.Fatalf("balance=%+v granted sum=%d", b, sum)
		}
		if err := l.Audit(ctx); err != nil {
			rt.Fatal(err)
		}
	})
}

// I4: concurrent duplicates of every mutation: one wins, the rest replay it.
func TestConcurrentDuplicates_I4(t *testing.T) {
	l, pool := setup(t)
	acct := funded(t, l, 1000)
	run := func(name string, f func() error) {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := f(); err != nil {
					t.Errorf("%s: %v", name, err)
				}
			}()
		}
		wg.Wait()
	}
	run("deposit", func() error { return l.Deposit(ctx, acct, 500, "dep-x", "") })
	var mu sync.Mutex
	ids := map[string]bool{}
	run("reserve", func() error {
		h, err := l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: 400, IdemKey: "rx", Kind: ledger.KindModel, TTL: time.Minute})
		mu.Lock()
		ids[h.ID] = true
		mu.Unlock()
		return err
	})
	if len(ids) != 1 {
		t.Fatalf("duplicate reserves produced %d holds", len(ids))
	}
	var hid string
	for id := range ids {
		hid = id
	}
	results := map[ledger.Settlement]bool{}
	run("settle", func() error {
		s, err := l.Settle(ctx, ledger.SettleReq{HoldID: hid, Actual: 150, IdemKey: "sx"})
		mu.Lock()
		results[s] = true
		mu.Unlock()
		return err
	})
	if len(results) != 1 {
		t.Fatalf("settle results differ: %v", results)
	}
	if n := entryCount(t, pool); n != 1+1+1+1+1 { // dep0, depx, hold, settle, release
		t.Fatalf("entries %d", n)
	}
	if b := bal(t, l, acct); b.Balance != 1350 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	mustAudit(t, l)
}

// I3: a reconciler and a live settle race for the same expired hold; exactly
// one terminal state results, and the loser skips without counting.
func TestReconcileRacingSettle_I3(t *testing.T) {
	l, pool := setup(t)
	acct := funded(t, l, 10_000)
	late := time.Now().Add(time.Hour)

	// The reconciler is parked inside Lookup. It holds no lock then, so a live
	// settle must finish without waiting for it, and the reconciler loses.
	h := reserve(t, l, acct, "r1", 100)
	if err := l.AttachUpstream(ctx, h.ID, "fake", "up1"); err != nil {
		t.Fatal(err)
	}
	inLookup, proceed := make(chan struct{}), make(chan struct{})
	recDone := make(chan ledger.Report, 1)
	go func() {
		rep, err := l.Reconcile(ctx, blockingLookup{inLookup, proceed, 60}, late)
		if err != nil {
			t.Error(err)
		}
		recDone <- rep
	}()
	<-inLookup
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: 70, IdemKey: "live"}); err != nil {
		t.Fatalf("live settle while reconciler is in Lookup: %v", err)
	}
	select {
	case <-recDone:
		t.Fatal("settle test is vacuous: reconciler returned before being unparked")
	default:
	}
	close(proceed)
	if rep := <-recDone; rep != (ledger.Report{}) {
		t.Fatalf("reconciler must count nothing after losing, got %+v", rep)
	}
	if b := bal(t, l, acct); b.Balance != 10_000-70 || b.Held != 0 {
		t.Fatalf("balance %+v", b)
	}
	mustAudit(t, l)

	// The other order: live settle wins, the reconciler skips and counts nothing.
	h2 := reserve(t, l, acct, "r2", 100)
	if _, err := l.Settle(ctx, ledger.SettleReq{HoldID: h2.ID, Actual: 70, IdemKey: "live2"}); err != nil {
		t.Fatal(err)
	}
	if rep, err := l.Reconcile(ctx, fakeLookup{}, late); err != nil || rep != (ledger.Report{}) {
		t.Fatalf("reconcile after live settle: %+v %v", rep, err)
	}

	// Free-for-all: many holds, a settle and two reconcilers at once.
	const n = 30
	holds := make([]ledger.Hold, n)
	for i := range holds {
		holds[i] = reserve(t, l, acct, fmt.Sprint("m", i), 10)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var total int
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := l.Reconcile(ctx, fakeLookup{}, late)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += rep.Settled + rep.Released + rep.Errors
			mu.Unlock()
		}()
	}
	live := 0
	for i := range holds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.Settle(ctx, ledger.SettleReq{HoldID: holds[i].ID, Actual: 5, IdemKey: fmt.Sprint("ls", i)})
			if err == nil {
				mu.Lock()
				live++
				mu.Unlock()
			} else if !errors.Is(err, ledger.ErrHoldNotActive) {
				t.Errorf("settle %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if total+live != n {
		t.Fatalf("terminal transitions %d (reconciled) + %d (live) != %d holds", total, live, n)
	}
	var multi int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT hold_id FROM ledger_entries WHERE kind='settle'
		GROUP BY hold_id HAVING count(*) > 1) x`).Scan(&multi); err != nil || multi != 0 {
		t.Fatalf("holds settled twice: %d %v", multi, err)
	}
	mustAudit(t, l)
}

// I6: a lease extended while the reconciler looks up usage is live again, so
// the reconciler must leave the hold active.
func TestReconcileSkipsExtendedHold_I6(t *testing.T) {
	l, _ := setup(t)
	acct := funded(t, l, 1000)
	h := reserve(t, l, acct, "r1", 100)
	if err := l.AttachUpstream(ctx, h.ID, "fake", "up1"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(2 * time.Minute) // past the 1m TTL
	inLookup, proceed := make(chan struct{}), make(chan struct{})
	recDone := make(chan ledger.Report, 1)
	go func() {
		rep, err := l.Reconcile(ctx, blockingLookup{inLookup, proceed, 60}, now)
		if err != nil {
			t.Error(err)
		}
		recDone <- rep
	}()
	<-inLookup
	if err := l.Extend(ctx, h.ID, time.Hour); err != nil { // real clock: expires well past `now`
		t.Fatal(err)
	}
	close(proceed)
	if rep := <-recDone; rep != (ledger.Report{}) {
		t.Fatalf("report %+v", rep)
	}
	if b := bal(t, l, acct); b.Held != 100 {
		t.Fatalf("hold was closed despite the extend: %+v", b)
	}
	mustAudit(t, l)
}

type ctxLookup struct{}

func (ctxLookup) Lookup(c context.Context, _, _ string) (ledger.Usage, bool, error) {
	<-c.Done()
	return ledger.Usage{}, false, c.Err()
}

// I6: a lookup that never answers is cut off by the timeout; the hold stays
// active for the next pass.
func TestReconcileLookupTimeout_I6(t *testing.T) {
	l, _ := setup(t)
	l = l.WithLookupTimeout(20 * time.Millisecond)
	acct := funded(t, l, 1000)
	h := reserve(t, l, acct, "r1", 100)
	if err := l.AttachUpstream(ctx, h.ID, "fake", "up1"); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Reconcile(ctx, ctxLookup{}, time.Now().Add(time.Hour))
	if err != nil || rep != (ledger.Report{Errors: 1}) {
		t.Fatalf("report %+v %v", rep, err)
	}
	if b := bal(t, l, acct); b.Held != 100 {
		t.Fatalf("hold should stay active: %+v", b)
	}
	mustAudit(t, l)
}

type blockingLookup struct {
	in      chan struct{}
	proceed chan struct{}
	cost    money.Nanos
}

func (b blockingLookup) Lookup(context.Context, string, string) (ledger.Usage, bool, error) {
	close(b.in)
	<-b.proceed
	return ledger.Usage{Cost: b.cost, Source: "fake"}, true, nil
}

// I6: a real process reserves and is SIGKILLed before settling. After
// expires_at the reconciler settles from usage if found, else releases.
func TestCrashRecovery_I6(t *testing.T) {
	l, pool := setup(t)
	acct := funded(t, l, 10_000)

	bin := filepath.Join(t.TempDir(), "testcrash")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/ws4charlie/agent-gate/internal/ledger/testcrash").CombinedOutput(); err != nil {
		t.Fatalf("build testcrash: %v\n%s", err, out)
	}
	cc := pool.Config().ConnConfig
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s", cc.Host, cc.Port, cc.User, cc.Database)

	spawnAndKill := func(key, upstream string, amount money.Nanos) string {
		cmd := exec.Command(bin, acct, key, fmt.Sprint(int64(amount)), upstream)
		cmd.Env = append(os.Environ(), "AG_CRASH_DSN="+dsn)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "ready ") {
			cmd.Process.Kill()
			t.Fatalf("helper not ready: %q %v", line, err)
		}
		if err := cmd.Process.Kill(); err != nil { // SIGKILL: no deferred cleanup runs
			t.Fatal(err)
		}
		_ = cmd.Wait()
		return strings.TrimSpace(strings.TrimPrefix(line, "ready "))
	}
	foundID := spawnAndKill("crash-found", "up-found", 500)
	lostID := spawnAndKill("crash-lost", "up-lost", 300)

	if b := bal(t, l, acct); b.Held != 800 {
		t.Fatalf("holds not visible after crash: %+v", b)
	}
	// Not expired yet: the reconciler must leave them alone.
	if rep, err := l.Reconcile(ctx, fakeLookup{"up-found": 420}, time.Now()); err != nil || rep != (ledger.Report{}) {
		t.Fatalf("early reconcile: %+v %v", rep, err)
	}

	rep, err := l.Reconcile(ctx, fakeLookup{"up-found": 420}, time.Now().Add(time.Hour))
	if err != nil || rep != (ledger.Report{Settled: 1, Released: 1}) {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	if b := bal(t, l, acct); b != (ledger.Balance{AccountID: acct, Balance: 10_000 - 420, Held: 0}) {
		t.Fatalf("balance %+v", b)
	}
	status := func(id string) (st string, settled int64) {
		if err := pool.QueryRow(ctx, `SELECT status, settled_nanos FROM holds WHERE id=$1`, id).Scan(&st, &settled); err != nil {
			t.Fatal(err)
		}
		return
	}
	if st, n := status(foundID); st != "settled" || n != 420 {
		t.Fatalf("found hold: %s %d", st, n)
	}
	var memo string
	if st, _ := status(lostID); st != "released" {
		t.Fatalf("lost hold: %s", st)
	}
	if err := pool.QueryRow(ctx, `SELECT memo FROM ledger_entries WHERE hold_id=$1 AND kind='release'`, lostID).Scan(&memo); err != nil || !strings.Contains(memo, "unmetered") {
		t.Fatalf("release not marked unmetered: %q %v", memo, err)
	}
	mustAudit(t, l)
}

// I1, I2, I3, I4: random sequences of operations with injected replays; the
// audit passes after every step and the ledger agrees with a simple model.
func TestStateful_I1_I2(t *testing.T) {
	l, _ := setup(t)
	rapid.Check(t, func(rt *rapid.T) {
		acct, err := l.CreateAccount(ctx, "sm")
		if err != nil {
			rt.Fatal(err)
		}
		type mh struct {
			id       string
			key      string
			amount   money.Nanos
			status   string // active | settled | released
			upstream string
			settleK  string
			actual   money.Nanos
			result   ledger.Settlement
		}
		var (
			balance, held money.Nanos
			holds         []*mh
			depositKeys   = map[string]money.Nanos{}
			seq           int
			clock         = time.Now()
		)
		next := func(p string) string { seq++; return fmt.Sprintf("%s-%d", p, seq) }
		check := func() {
			b, err := l.Balance(ctx, acct)
			if err != nil {
				rt.Fatal(err)
			}
			if b.Balance != balance || b.Held != held {
				rt.Fatalf("model balance=%d held=%d, ledger %+v", balance, held, b)
			}
			if err := l.Audit(ctx); err != nil {
				rt.Fatalf("audit: %v", err)
			}
		}
		pickHold := func(label string, want string) *mh {
			var c []*mh
			for _, h := range holds {
				if want == "" || h.status == want {
					c = append(c, h)
				}
			}
			if len(c) == 0 {
				return nil
			}
			return c[rapid.IntRange(0, len(c)-1).Draw(rt, label)]
		}
		settleModel := func(h *mh, actual money.Nanos) ledger.Settlement {
			free := balance - held
			limit := h.amount + free
			charged, unrec := actual, money.Nanos(0)
			if actual > limit {
				charged, unrec = limit, actual-limit
			}
			part := min(charged, h.amount)
			balance -= charged
			held -= h.amount
			return ledger.Settlement{HoldID: h.id, Charged: charged, Released: h.amount - part, Unrecovered: unrec}
		}

		// The database is shared across iterations: drain this iteration's
		// leftover holds so the next one's reconcile counts are its own.
		defer func() { _, _ = l.Reconcile(ctx, fakeLookup{}, time.Now().Add(10_000*time.Hour)) }()

		rt.Repeat(map[string]func(*rapid.T){
			"deposit": func(rt *rapid.T) {
				amt := money.Nanos(rapid.Int64Range(1, 500).Draw(rt, "amt"))
				key := next("dep")
				if err := l.Deposit(ctx, acct, amt, key, ""); err != nil {
					rt.Fatal(err)
				}
				balance += amt
				depositKeys[key] = amt
			},
			"replay deposit": func(rt *rapid.T) {
				for k, a := range depositKeys {
					if err := l.Deposit(ctx, acct, a, k, ""); err != nil {
						rt.Fatalf("replay: %v", err)
					}
					break
				}
			},
			"reserve": func(rt *rapid.T) {
				amt := money.Nanos(rapid.Int64Range(1, 300).Draw(rt, "amt"))
				key := next("res")
				h, err := l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: amt, IdemKey: key, Kind: ledger.KindModel, TTL: time.Minute})
				if balance-held < amt {
					if !errors.Is(err, ledger.ErrInsufficientFunds) {
						rt.Fatalf("want insufficient funds, got %v", err)
					}
					return
				}
				if err != nil {
					rt.Fatal(err)
				}
				held += amt
				m := &mh{id: h.ID, key: key, amount: amt, status: "active"}
				if rapid.Bool().Draw(rt, "attach") {
					m.upstream = next("up")
					if err := l.AttachUpstream(ctx, h.ID, "fake", m.upstream); err != nil {
						rt.Fatal(err)
					}
				}
				holds = append(holds, m)
			},
			"replay reserve": func(rt *rapid.T) {
				h := pickHold("hold", "")
				if h == nil {
					return
				}
				got, err := l.Reserve(ctx, ledger.ReserveReq{AccountID: acct, Amount: h.amount, IdemKey: h.key, Kind: ledger.KindModel, TTL: time.Minute})
				if err != nil || got.ID != h.id {
					rt.Fatalf("replay: %+v %v", got, err)
				}
			},
			"settle": func(rt *rapid.T) {
				h := pickHold("hold", "active")
				if h == nil {
					return
				}
				actual := money.Nanos(rapid.Int64Range(0, 2*int64(h.amount)+50).Draw(rt, "actual"))
				h.settleK, h.actual = next("set"), actual
				want := settleModel(h, actual)
				got, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.id, Actual: actual, IdemKey: h.settleK})
				if err != nil || got != want {
					rt.Fatalf("settle: got %+v %v want %+v", got, err, want)
				}
				h.status, h.result = "settled", got
			},
			"replay settle": func(rt *rapid.T) {
				h := pickHold("hold", "settled")
				if h == nil || h.settleK == "" {
					return
				}
				got, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.id, Actual: h.actual, IdemKey: h.settleK})
				if err != nil || got != h.result {
					rt.Fatalf("replay: %+v %v want %+v", got, err, h.result)
				}
			},
			"release": func(rt *rapid.T) {
				h := pickHold("hold", "active")
				if h == nil {
					return
				}
				got, err := l.Release(ctx, h.id, next("rel"), "test")
				if err != nil || got.Released != h.amount {
					rt.Fatalf("release: %+v %v", got, err)
				}
				held -= h.amount
				h.status = "released"
			},
			"settle terminal hold": func(rt *rapid.T) {
				h := pickHold("hold", "")
				if h == nil || h.status == "active" {
					return
				}
				_, err := l.Settle(ctx, ledger.SettleReq{HoldID: h.id, Actual: 1, IdemKey: next("late")})
				if !errors.Is(err, ledger.ErrHoldNotActive) {
					rt.Fatalf("want ErrHoldNotActive, got %v", err)
				}
			},
			"reconcile": func(rt *rapid.T) {
				clock = clock.Add(2 * time.Hour) // past every TTL
				lk := fakeLookup{}
				for _, h := range holds {
					if h.status == "active" && h.upstream != "" && rapid.Bool().Draw(rt, "found") {
						// At most the hold: overage would make the result depend on
						// settle order, which the model does not pin down (I7 has its own test).
						lk[h.upstream] = money.Nanos(rapid.Int64Range(0, int64(h.amount)).Draw(rt, "cost"))
					}
				}
				var wantS, wantR int
				for _, h := range holds {
					if h.status != "active" {
						continue
					}
					if c, ok := lk[h.upstream]; ok && h.upstream != "" {
						h.result = settleModel(h, c)
						h.status, h.settleK, h.actual = "settled", "reconcile:"+h.id, c
						wantS++
					} else {
						held -= h.amount
						h.status = "released"
						wantR++
					}
				}
				rep, err := l.Reconcile(ctx, lk, clock)
				if err != nil || rep.Settled != wantS || rep.Released != wantR || rep.Errors != 0 {
					rt.Fatalf("reconcile: %+v %v want settled=%d released=%d", rep, err, wantS, wantR)
				}
			},
		})
		check()
	})
}
