package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/server"
)

// newDirectEnv adds a "direct" provider with no usage lookup (like direct OpenAI)
// routed by the "dir/" prefix, next to the lookup-capable fakeoa.
func newDirectEnv(t *testing.T) *env {
	return newEnv(t, 10*usd, func(s *server.Server) {
		s.Providers.Providers["direct"] = provider.ProviderCfg{Kind: provider.KindOpenAICompatible, BaseURL: s.Providers.Providers["fakeoa"].BaseURL, NoUsageLookup: true}
		s.Providers.Routes = append([]provider.Route{{Prefix: "dir/", Provider: "direct", Strip: true}}, s.Providers.Routes...)
	})
}

// activeHold waits for the single active hold and returns provider, upstream id.
func (e *env) activeHold() (prov, up string) {
	e.t.Helper()
	for i := 0; i < 300; i++ {
		err := e.pool.QueryRow(context.Background(), `SELECT provider, upstream_id FROM holds WHERE status='active' AND account_id=$1`, e.acct).Scan(&prov, &up)
		if err == nil && prov != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("no active hold with a provider")
	return
}

// I6: the placeholder is on the hold while the upstream is still working, only
// for a provider with no usage lookup.
func TestPlaceholderUpstreamIDWhileWaiting_I6(t *testing.T) {
	for _, c := range []struct {
		model string
		want  bool
	}{{"dir/m", true}, {"oa/m", false}} {
		e := newDirectEnv(t)
		e.fake.Push(fakeupstream.Script{Latency: 400 * time.Millisecond})
		done := make(chan struct{})
		go func() { defer close(done); e.do("/v1/chat/completions", chatBody(c.model, false), nil) }()
		var up string
		if c.want {
			var prov string
			prov, up = e.activeHold()
			if prov != "direct" || !strings.HasPrefix(up, "pending:") || len(up) <= len("pending:") {
				t.Fatalf("%s: provider %q upstream %q", c.model, prov, up)
			}
		} else {
			time.Sleep(150 * time.Millisecond)
			if err := e.pool.QueryRow(context.Background(), `SELECT upstream_id FROM holds WHERE status='active' AND account_id=$1`, e.acct).Scan(&up); err != nil || up != "" {
				t.Fatalf("%s: upstream %q err %v, want none", c.model, up, err)
			}
		}
		<-done
		e.noActiveHolds()
	}
}

// I6: a hold left active for a no-lookup provider is charged in full by the reconciler.
func TestCrashedNoLookupHoldChargedInFull_I6(t *testing.T) {
	e := newDirectEnv(t)
	ctx := context.Background()
	est := estimate(chatBody("dir/m", false))
	e.setScope(func(s *policy.Scope) { s.DailyBudget = est * 4 })
	snap := e.snapshot("")
	h, err := e.led.Reserve(ctx, ledger.ReserveReq{AccountID: e.acct, Amount: est, IdemKey: "dead-nl", Kind: ledger.KindModel, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if rule, err := e.srv.Agents.Charge(ctx, agents.ChargeReq{HoldID: h.ID, AgentID: e.agentID(), Amount: est, Scope: snap.Scope}); rule != "" || err != nil {
		t.Fatalf("%s %v", rule, err)
	}
	if err := e.led.AttachUpstream(ctx, h.ID, "direct", "pending:req-x"); err != nil {
		t.Fatal(err)
	}
	rep, err := server.Reconcile(ctx, e.pool, e.led, (&provider_lookup{e}).l(), e.audit, e.srv.Agents, time.Now().Add(time.Hour))
	if err != nil || rep.Settled != 1 || rep.Released != 0 || rep.Errors != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if got := 10*usd - e.balance().Balance; got != est {
		t.Fatalf("charged %d, want the full hold %d", got, est)
	}
	rows, _ := e.audit.ByRequest(ctx, "reconcile:"+h.ID)
	if len(rows) != 1 || rows[0].Status != audit.StatusReconciledSettled || rows[0].CostSource != "hold_no_lookup" || rows[0].Cost != est {
		t.Fatalf("%+v", rows)
	}
	if got := e.snapshot("").DailySpent; got != est {
		t.Fatalf("budget window %d, want %d", got, est)
	}
	e.noActiveHolds()
}

// A clean upstream failure on a no-lookup provider still releases: nothing charged.
func TestCleanFailureNoLookupChargesNothing(t *testing.T) {
	e := newDirectEnv(t)
	e.fake.Push(fakeupstream.Script{Status: 500})
	resp, _ := e.do("/v1/chat/completions", chatBody("dir/m", false), nil)
	if resp.StatusCode != 500 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	e.noActiveHolds()
	if b := e.balance(); b.Balance != 10*usd || b.Held != 0 {
		t.Fatalf("%+v", b)
	}
	rep, err := server.Reconcile(context.Background(), e.pool, e.led, (&provider_lookup{e}).l(), e.audit, e.srv.Agents, time.Now().Add(time.Hour))
	if err != nil || rep.Settled+rep.Released != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if b := e.balance(); b.Balance != 10*usd {
		t.Fatalf("%+v", b)
	}
}
