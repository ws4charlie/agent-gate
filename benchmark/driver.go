package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/mcp"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/paidtool"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/pricing"
	"github.com/ws4charlie/agent-gate/internal/provider"
	"github.com/ws4charlie/agent-gate/internal/screen"
	"github.com/ws4charlie/agent-gate/internal/server"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

const (
	payeeHex = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8" // a public test address
	otherHex = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC" // another one
	usd      = money.Nanos(1_000_000_000)
)

// Outcome is what one step came to: allow, deny, approval or (for a payment
// whose result is unknown) pending, plus the rule id the gateway reported.
type Outcome struct{ Verdict, Rule string }

// rig is the assembled gateway and its fakes.
type rig struct {
	pool  *pgxpool.Pool
	led   *ledger.Postgres
	srv   *server.Server
	gw    *httptest.Server
	root  string
	close []func()
}

func (r *rig) Close() {
	for i := len(r.close) - 1; i >= 0; i-- {
		r.close[i]()
	}
}

func newRig(ctx context.Context, pool *pgxpool.Pool, root string) (*rig, error) {
	r := &rig{pool: pool, led: ledger.New(pool), root: root}
	// Model upstream. $1000/$2000 per 1M tokens: one call is real money against small budgets,
	// so a budget of cents is reached in a few calls (same trick as demo/phase2-policy.sh).
	fake := fakeupstream.New()
	r.close = append(r.close, fake.Close)
	const upKey = "bench-upstream-key"
	fake.Key = upKey
	// A call takes 200 ms, so calls fired together overlap and are all "in flight" while
	// the velocity window counts their worst case (the same trick as the policy demo).
	fake.SetDefault(fakeupstream.Script{Latency: 200 * time.Millisecond})
	row := pricing.Price{Input: 1_000 * usd, Output: 2_000 * usd, CacheWrite: 1_000 * usd}

	// MCP upstream; "paid" costs $0.005 a call, everything else is free.
	up := mcp.NewFake()
	r.close = append(r.close, up.Close)
	const mcpCred = "Bearer bench-mcp-credential"
	up.Key = mcpCred

	// Simulated chain, paid tool and wallet. echo costs $0.01, premium $0.50.
	sim, err := x402.NewSim()
	if err != nil {
		return nil, err
	}
	r.close = append(r.close, func() { sim.Close() })
	key, _ := crypto.GenerateKey()
	w, err := wallet.New(key, x402.LocalChainID)
	if err != nil {
		return nil, err
	}
	if err := sim.Mint(ctx, w.Address(), big.NewInt(500_000_000)); err != nil {
		return nil, err
	}
	ver := x402.NewVerifier(x402.Config{Network: x402.NetworkLocal, Token: sim.Token, TokenName: "USDC", TokenVersion: "2",
		PayTo: common.HexToAddress(payeeHex), Chain: sim.Chain})
	tool := httptest.NewServer(paidtool.Handler(ver))
	r.close = append(r.close, tool.Close)

	r.srv = &server.Server{
		Pool: pool, Ledger: r.led, Audit: audit.NewStore(pool),
		Providers: provider.Config{
			Providers: map[string]provider.ProviderCfg{"fake": {Kind: provider.KindOpenAICompatible, BaseURL: fake.URL + "/v1"}},
			Routes:    []provider.Route{{Prefix: "fake/", Provider: "fake", Strip: true}},
		},
		Prices:  &pricing.Table{Models: map[string]pricing.Price{"chat": row, "chat-big": row}},
		Keys:    func(string) string { return upKey },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxHold: 5 * usd, DefaultMaxTokens: 100,
		MCP:    &mcp.Config{Servers: map[string]mcp.Server{"up": {URL: up.URL, AuthEnv: "BENCH_MCP", Prices: map[string]money.Nanos{"paid": 5_000_000}}}},
		Screen: &screen.Screener{},
		Getenv: func(k string) string {
			if k == "BENCH_MCP" {
				return mcpCred
			}
			return ""
		},
		Payments: &server.Payments{Wallet: w, Tools: map[string]string{"echo": tool.URL + "/echo", "premium": tool.URL + "/premium"}},
	}
	r.gw = httptest.NewServer(r.srv.Handler())
	r.close = append(r.close, r.gw.Close)
	return r, nil
}

// agent is one scenario's agent with its own funded account.
type agent struct {
	name, key string
}

func (r *rig) newAgent(ctx context.Context, s Setup) (agent, error) {
	acct, err := r.led.CreateAccount(ctx, "bench-"+uuid.NewString()[:8])
	if err != nil {
		return agent{}, err
	}
	if err := r.led.Deposit(ctx, acct, 100*usd, "dep", ""); err != nil {
		return agent{}, err
	}
	sub := func(in []string) []string {
		out := []string{}
		for _, v := range in {
			switch v {
			case "@payee":
				v = payeeHex
			case "@other":
				v = otherHex
			}
			out = append(out, v)
		}
		return out
	}
	n := func(f float64) money.Nanos { return money.Nanos(f*float64(usd) + 0.5) }
	name := "bench-" + uuid.NewString()[:8]
	secret, _, err := r.srv.Agents.Create(ctx, acct, name, policy.Scope{Models: sub(s.Models), Tools: sub(s.Tools), Payees: sub(s.Payees),
		DailyBudget: n(s.Daily), TaskBudget: n(s.Task), VelocityPerMinute: n(s.Velocity), ApprovalThreshold: n(s.ApprovalOver)})
	return agent{name: name, key: secret}, err
}

// play runs one step and returns its outcome.
func (r *rig) play(ctx context.Context, a agent, st Step) (Outcome, error) {
	switch st.Op {
	case "kill":
		_, err := r.srv.Agents.Kill(ctx, a.name)
		return Outcome{}, err
	case "revoke":
		_, err := r.srv.Agents.Revoke(ctx, a.name)
		return Outcome{}, err
	case "kill_run":
		_, err := r.srv.Runs.Kill(ctx, st.Run)
		return Outcome{}, err
	case "approve":
		pending, err := r.srv.Approvals.Store.List(ctx, "pending")
		if err != nil {
			return Outcome{}, err
		}
		for _, p := range pending {
			if p.AgentID != "" && r.owns(ctx, a, p.AgentID) {
				_, err = r.srv.Approvals.Allow(ctx, p.ID, "benchmark")
				return Outcome{}, err
			}
		}
		return Outcome{}, fmt.Errorf("approve: nothing pending for %s", a.name)
	case "screen":
		b, err := os.ReadFile(r.root + "/internal/screen/testdata/" + st.File)
		if err != nil {
			return Outcome{}, err
		}
		v := (&screen.Screener{}).Screen(ctx, screen.ToolResult{Server: "up", Tool: "t", Texts: []string{string(b)}})
		if v.Blocked {
			return Outcome{"deny", v.Rule}, nil
		}
		return Outcome{"allow", "allow"}, nil
	case "chat", "tool", "pay":
		n := max(st.Repeat, 1)
		outs := make([]Outcome, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			do := func(i int) { outs[i], errs[i] = r.request(ctx, a, st) }
			if st.Parallel {
				wg.Add(1)
				go func(i int) { defer wg.Done(); do(i) }(i)
			} else {
				do(i)
			}
		}
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				return Outcome{}, e
			}
		}
		if st.Parallel { // the first refusal, if any: "did the burst get past the limit"
			for _, o := range outs {
				if o.Verdict != "allow" {
					return o, nil
				}
			}
		}
		return outs[n-1], nil
	}
	return Outcome{}, fmt.Errorf("unknown op %q", st.Op)
}

func (r *rig) owns(ctx context.Context, a agent, agentID string) bool {
	got, err := r.srv.Agents.Get(ctx, a.name)
	return err == nil && got.ID == agentID
}

func (r *rig) request(ctx context.Context, a agent, st Step) (Outcome, error) {
	idem := st.Idem
	if idem == "" || st.Repeat > 1 {
		idem = uuid.NewString()
	}
	var path string
	var body any
	h := http.Header{"Content-Type": {"application/json"}, "Idempotency-Key": {idem}}
	key := a.key
	if st.BadKey {
		key = "ag_" + strings.Repeat("0", 40)
	}
	h.Set("Authorization", "Bearer "+key)
	if st.Task != "" {
		h.Set("X-AgentGate-Task", st.Task)
	}
	if st.Run != "" {
		h.Set("X-AgentGate-Run", st.Run)
	}
	switch st.Op {
	case "chat":
		path = "/v1/chat/completions"
		body = map[string]any{"model": st.Model, "max_tokens": 100, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	case "pay":
		path = "/v1/payments"
		body = map[string]any{"tool": st.Tool, "input": map[string]any{"text": "hello"}}
	case "tool":
		path = "/mcp/up"
		meta := map[string]any{"io.modelcontextprotocol/protocolVersion": mcp.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
		args := st.Args
		if args == nil {
			args = map[string]any{"text": "hello"}
		}
		body = map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": st.Tool, "arguments": args, "_meta": meta}}
		h.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
		h.Set("Mcp-Method", "tools/call")
		h.Set("Mcp-Name", st.Tool)
		h.Set("Accept", "application/json, text/event-stream")
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", r.gw.URL+path, bytes.NewReader(b))
	req.Header = h
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Outcome{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return classify(resp.StatusCode, raw), nil
}

// classify turns a gateway answer into an Outcome. The rule comes from the
// body (rule, else code), never from the HTTP status; the status only tells a
// success from a refusal. 202 is not a success (see SettlementPending in the SDK).
// An MCP refusal is HTTP 200 with a JSON-RPC error object, so "error present" decides, not the status.
func classify(status int, raw []byte) Outcome {
	var v struct {
		Status string          `json:"status"`
		Error  json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(raw, &v)
	rule := ""
	if len(v.Error) > 0 && string(v.Error) != "null" {
		var e struct {
			Rule string `json:"rule"`
			Code any    `json:"code"` // a string in HTTP errors, a number in JSON-RPC errors
			Data struct {
				Code string `json:"code"`
				Rule string `json:"rule"`
			} `json:"data"`
		}
		_ = json.Unmarshal(v.Error, &e)
		code, _ := e.Code.(string)
		rule = firstNonEmpty(e.Rule, e.Data.Rule, e.Data.Code, code, "error")
	}
	switch {
	case rule == "approval_required", status == http.StatusAccepted && v.Status == "approval_required":
		return Outcome{"approval", policy.RuleApprovalThreshold}
	case status == http.StatusAccepted:
		return Outcome{"pending", v.Status}
	case rule == "" && status == http.StatusOK:
		return Outcome{"allow", "allow"}
	case rule == "" || rule == "error":
		rule = fmt.Sprintf("http_%d", status)
	}
	return Outcome{"deny", rule}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
