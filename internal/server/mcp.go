package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ws4charlie/agent-gate/internal/approval"
	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/mcp"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/run"
	"github.com/ws4charlie/agent-gate/internal/screen"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// Audit statuses written by the MCP route.
const (
	StatusScreened = "screened" // the tool result was blocked by the screen
)

const maxMCPBody = 8 << 20

// handleMCP is POST /mcp/{server}: a Streamable HTTP proxy to one upstream MCP
// server, with the same gate a model call goes through. See package mcp for
// the protocol version and for what the proxy does with it.
//
//	tools/list  forwarded, then filtered by the agent's tool allowlist
//	tools/call  policy -> reserve (kind tool) -> forward -> screen -> settle -> audit
//	the rest    (resources, prompts, discover, ...) forwarded and logged, not inspected
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.MCP == nil {
		http.NotFound(w, r)
		return
	}
	reqID := uuid.NewString()
	name := r.PathValue("server")
	ctx, span := s.Tracer.Start(r.Context(), "gateway.mcp",
		trace.WithAttributes(attribute.String("request.id", reqID), attribute.String("mcp.server", name)))
	c := &call{s: s, w: w, r: r, f: "mcp", ctx: ctx, span: span, start: time.Now(), rpc: true,
		ev: audit.Event{RequestID: reqID, Kind: "mcp", Provider: name, At: time.Now()}}
	w.Header().Set("X-Request-Id", reqID)
	if r.Method != http.MethodPost {
		// 2026-07-28 removed the GET stream and sessions: GET and DELETE are 405.
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		span.End()
		return
	}
	c.mcp(name)
}

// protoFail answers a protocol-level error: a 4xx whose body is the JSON-RPC
// error, as the spec's transport rules require.
func (c *call) protoFail(status int, e *mcp.Error, auditStatus string) {
	c.ev.Err = e.Message
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_, _ = c.w.Write(mcp.ErrorResponse(c.rpcID, *e))
	c.finish(auditStatus)
}

// writeRPCError turns a gate refusal into a JSON-RPC error. Application-level
// refusals (policy, funds, duplicates, approval) are HTTP 200 with an error
// object: the transport worked and the call was refused, which is what the
// task and JSON-RPC both mean by "not a transport error". Malformed requests
// and internal failures keep their HTTP status.
func (c *call) writeRPCError(httpStatus int, code, msg string, extra map[string]any) {
	rc, status := mcp.CodeInternal, httpStatus
	data := map[string]any{"code": code, "request_id": c.ev.RequestID}
	for k, v := range extra {
		if k != "type" {
			data[k] = v
		}
	}
	switch {
	case extra["type"] == "policy_denied":
		rc, status = mcp.CodePolicyDenied, http.StatusOK
		data["rule"] = code
	case code == "insufficient_funds":
		rc, status = mcp.CodeInsufficient, http.StatusOK
	case code == "duplicate_request":
		rc, status = mcp.CodeDuplicate, http.StatusOK
	case code == "approval_required":
		rc, status = mcp.CodeApproval, http.StatusOK
	case strings.HasPrefix(code, "approval_"), code == "account_inactive":
		rc, status = mcp.CodePolicyDenied, http.StatusOK
	case code == "bad_request":
		rc = mcp.CodeInvalidParams
	case code == "unknown_server":
		rc, status = mcp.CodeInvalidParams, http.StatusNotFound
	case code == "upstream_unavailable", code == "upstream_error":
		rc = mcp.CodeUpstream
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_, _ = c.w.Write(mcp.ErrorResponse(c.rpcID, mcp.Error{Code: rc, Message: msg, Data: data}))
}

func (c *call) mcp(name string) {
	s := c.s
	// Origin: the spec requires validating it against DNS rebinding. An agent
	// process sends none; a browser sends one, and is refused unless listed.
	if o := c.r.Header.Get("Origin"); o != "" && !slices.Contains(s.MCP.AllowedOrigins, o) {
		c.protoFail(http.StatusForbidden, &mcp.Error{Code: mcp.CodeInvalidRequest, Message: "Origin not allowed"}, StatusPolicyDenied)
		return
	}

	// 1. authenticate. A bad credential is a transport error (401): there is
	// no agent to answer as.
	agent, err := s.authenticate(c.ctx, c.r)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			c.fail(http.StatusUnauthorized, audit.StatusUnauthorized, "unauthorized", "missing or invalid API key", nil)
		} else {
			s.Log.Error("auth lookup", "err", err)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		}
		return
	}
	c.agent = agent
	c.ev.AccountID, c.ev.AgentID = agent.AccountID, agent.ID
	c.span.SetAttributes(attribute.String("account.id", agent.AccountID), attribute.String("agent.id", agent.ID))
	if c.refuseInactive(policy.KindTool) { // rule 1, before the body is read (I12)
		return
	}
	srv, ok := s.MCP.Servers[name]
	if !ok {
		c.fail(http.StatusNotFound, audit.StatusBadRequest, "unknown_server", "no such MCP server", nil)
		return
	}

	// 2. read and check the message
	body, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, maxMCPBody))
	if err != nil {
		c.protoFail(http.StatusBadRequest, &mcp.Error{Code: mcp.CodeInvalidRequest, Message: "cannot read body"}, audit.StatusBadRequest)
		return
	}
	req, perr := mcp.Parse(body)
	c.rpcID = req.ID
	if perr != nil {
		c.protoFail(http.StatusBadRequest, perr, audit.StatusBadRequest)
		return
	}
	if status, e := mcp.Check(c.r.Header, req); e != nil {
		c.protoFail(status, e, audit.StatusBadRequest)
		return
	}
	c.ev.Model = req.Method
	if req.IsNotification() {
		c.forwardNotification(srv, req, body)
		return
	}
	kind := run.KindMCP
	if req.Method == "tools/call" {
		kind = run.KindTool
	}
	if !c.beginRun(kind) {
		return
	}
	switch req.Method {
	case "tools/call":
		c.toolCall(name, srv, req, body)
	case "tools/list":
		c.toolsList(name, srv, req, body)
	default:
		c.passthrough(srv, req, body)
	}
}

// ---- talking to the upstream ----

// upstream sends one POST to the upstream MCP server. The client's own
// Authorization is never forwarded; the server's credential comes from the
// environment (I9). The mirrored protocol headers and any Mcp-Param-* pass on.
func (c *call) upstream(ctx context.Context, srv mcp.Server, body []byte, extra ...[2]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.r.Header {
		ck := http.CanonicalHeaderKey(k)
		if ck == "Mcp-Protocol-Version" || ck == "Mcp-Method" || ck == "Mcp-Name" || strings.HasPrefix(ck, "Mcp-Param-") {
			req.Header[ck] = append([]string(nil), v...)
		}
	}
	// Mcp-Session-Id is dropped on purpose: 2026-07-28 has no sessions, and a
	// client-chosen id must not steer another client's upstream state.
	if name, val := srv.Credential(c.s.Getenv); name != "" && val != "" {
		req.Header.Set(name, val)
	}
	for _, h := range extra { // only the x402 payment header today
		req.Header.Set(h[0], h[1])
	}
	return c.s.Client.Do(req)
}

func (c *call) forwardNotification(srv mcp.Server, req mcp.Request, body []byte) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 10*time.Second)
	defer cancel()
	if resp, err := c.upstream(ctx, srv, body); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}
	c.w.WriteHeader(http.StatusAccepted)
	c.finish(audit.StatusOK)
}

// finalMessage reads an upstream reply that may be JSON or SSE and returns the
// JSON-RPC response (for SSE: the event that carries the request's id).
func finalMessage(resp *http.Response, id json.RawMessage) ([]byte, error) {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return io.ReadAll(io.LimitReader(resp.Body, maxMCPBody))
	}
	var final []byte
	err := mcp.ReadEvents(io.LimitReader(resp.Body, maxMCPBody), func(ev mcp.Event) bool {
		var m struct {
			ID json.RawMessage `json:"id"`
		}
		if len(ev.Data) > 0 && json.Unmarshal(ev.Data, &m) == nil && bytes.Equal(m.ID, id) {
			final = ev.Data
			return false
		}
		return true
	})
	if err == nil && final == nil {
		err = io.ErrUnexpectedEOF
	}
	return final, err
}

// ---- tools/list ----

// scopeOnly is a snapshot with every limit removed, so that Evaluate can only
// answer from the allowlist. It lets the gateway ask "is this tool in scope?"
// with the one engine instead of a second matcher (no second gate).
func scopeOnly(s policy.Snapshot) policy.Snapshot {
	s.Scope.DailyBudget, s.Scope.TaskBudget, s.Scope.VelocityPerMinute, s.Scope.ApprovalThreshold = 0, 0, 0, 0
	return s
}

func (c *call) inScope(snap policy.Snapshot, tool string) policy.Decision {
	return policy.Evaluate(policy.Request{AgentID: c.agent.ID, Kind: policy.KindTool, Tool: tool, Amount: 1}, scopeOnly(snap))
}

func (c *call) toolsList(server string, srv mcp.Server, req mcp.Request, body []byte) {
	snap, err := c.s.Agents.Snapshot(c.ctx, c.agent.ID, "")
	if err != nil {
		c.s.Log.Error("policy snapshot", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return
	}
	resp, err := c.upstream(c.ctx, srv, body)
	if err != nil {
		c.fail(http.StatusBadGateway, audit.StatusUpstreamFailed, "upstream_unavailable", "upstream unreachable", nil)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		c.fail(http.StatusBadGateway, audit.StatusUpstreamError, "upstream_error", "upstream answered "+http.StatusText(resp.StatusCode), nil)
		return
	}
	msg, err := finalMessage(resp, req.ID)
	if err != nil {
		c.fail(http.StatusBadGateway, audit.StatusUpstreamFailed, "upstream_unavailable", "upstream reply could not be read", nil)
		return
	}
	out, kept, total := filterTools(msg, func(tool string) bool { return c.inScope(snap, server+"/"+tool).Verdict == policy.Allow })
	c.ev.Err = ""
	c.span.SetAttributes(attribute.Int("tools.total", total), attribute.Int("tools.listed", kept))
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(out)
	c.finish(audit.StatusOK)
}

// filterTools keeps the tools allow() accepts. A reply that is not a
// recognisable list is passed on unchanged: the gateway filters, it does not
// reinterpret.
func filterTools(msg []byte, allow func(string) bool) (out []byte, kept, total int) {
	var m map[string]json.RawMessage
	var res map[string]json.RawMessage
	var tools []json.RawMessage
	if json.Unmarshal(msg, &m) != nil || json.Unmarshal(m["result"], &res) != nil || json.Unmarshal(res["tools"], &tools) != nil {
		return msg, 0, 0
	}
	list := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var n struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t, &n) == nil && allow(n.Name) {
			list = append(list, t)
		}
	}
	res["tools"], _ = json.Marshal(list)
	m["result"], _ = json.Marshal(res)
	out, _ = json.Marshal(m)
	return out, len(list), len(tools)
}

// ---- passthrough ----

// passthrough forwards a request the gateway does not interpret (resources,
// prompts, discover, ping, subscriptions) and streams the answer back
// unchanged. It is authenticated, run-attached and audited, but not screened or
// metered: that content re-enters the agent's context unchecked, a limit
// docs/reference.md states.
func (c *call) passthrough(srv mcp.Server, req mcp.Request, body []byte) {
	upCtx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	defer c.trackInFlight(cancel)()
	resp, err := c.upstream(upCtx, srv, body)
	if err != nil {
		if c.killed.Load() {
			c.ev.Err = c.killRule()
			c.fail(http.StatusForbidden, StatusKilled, c.killRule(), "the agent or its run was killed", map[string]any{"type": "policy_denied"})
			return
		}
		c.fail(http.StatusBadGateway, audit.StatusUpstreamFailed, "upstream_unavailable", "upstream unreachable", nil)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		c.fail(http.StatusBadGateway, audit.StatusUpstreamError, "upstream_error", "upstream answered "+http.StatusText(resp.StatusCode), nil)
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.w.Header().Set("Content-Type", ct)
	}
	c.w.Header().Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(http.StatusOK)
	fl, _ := c.w.(http.Flusher)
	buf := make([]byte, 32<<10)
	status := audit.StatusOK
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := c.w.Write(buf[:n]); werr != nil {
				status = audit.StatusClientDisconnected
				break
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				status = audit.StatusUpstreamFailed
				if c.killed.Load() {
					status = StatusKilled
				}
			}
			break
		}
	}
	c.finish(status)
}

// ---- tools/call ----

func (c *call) toolCall(server string, srv mcp.Server, req mcp.Request, body []byte) {
	s := c.s
	var p mcp.CallParams
	if json.Unmarshal(req.Params, &p) != nil || p.Name == "" {
		c.protoFail(http.StatusBadRequest, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "tools/call needs params.name"}, audit.StatusBadRequest)
		return
	}
	tool := server + "/" + p.Name // the name policy scopes match: no '/' in a tool name, so it is unambiguous
	c.ev.Model = tool
	price := srv.Price(p.Name)
	task, ok := c.taskID()
	if !ok {
		return
	}

	var hold ledger.Hold
	var held bool
	if price == 0 {
		// A free tool still gets a decision. Evaluate refuses Amount <= 0, and
		// that check is for models and payments, so it stays. Here the question
		// is only "may this agent use this tool?": the scope and status rules,
		// asked of the same engine with the limits removed. Nothing is
		// reserved, charged or counted against a budget; the decision is logged
		// with amount 0.
		snap, err := s.Agents.Snapshot(c.ctx, c.agent.ID, task)
		if err != nil {
			s.Log.Error("policy snapshot", "request_id", c.ev.RequestID, "err", err)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
			return
		}
		d := c.inScope(snap, tool)
		c.logDecision(policy.Request{AgentID: c.agent.ID, Kind: policy.KindTool, Tool: tool, TaskID: task}, d, "")
		if d.Verdict != policy.Allow {
			c.denyResponse(d)
			return
		}
	} else {
		idem, ok := c.idemKey()
		if !ok {
			return
		}
		fp := approval.Fingerprint("tool", tool, itoa(int64(price)), task, bodyHash(p.Arguments))
		approved, approvalID, stop := c.checkApproval(idem, fp)
		if stop {
			return
		}
		preq := policy.Request{AgentID: c.agent.ID, Kind: policy.KindTool, Tool: tool, Amount: price, TaskID: task, Approved: approved}
		d, ok := c.evaluate(preq, approvalID)
		if !ok {
			c.releaseApproved(approved)
			return
		}
		switch d.Verdict {
		case policy.Deny:
			c.releaseApproved(approved)
			c.denyResponse(d)
			return
		case policy.RequireApproval:
			h, ok := c.reserve(price, idem, true, s.HoldTTL)
			if !ok {
				return
			}
			c.ext.HoldID = h.ID
			c.queueApproval(h, preq, idem, fp, d)
			return
		}
		if hold, ok = c.reserve(price, idem, !approved, s.HoldTTL); !ok {
			return
		}
		held = true
		c.ext.HoldID = hold.ID
		if !c.charge(hold, preq) {
			return
		}
		stopLease := c.lease(hold.ID)
		defer stopLease()
	}
	c.ev.CostSource = "tool_price"

	upCtx, upSpan := c.child("upstream")
	upCtx, cancel := context.WithCancel(upCtx)
	defer cancel()
	defer c.trackInFlight(cancel)() // I12: kill by agent or by run cancels this call
	resp, err := c.upstream(upCtx, srv, body)
	if err != nil {
		upSpan.End()
		c.toolFailed(hold, held, err)
		return
	}
	defer resp.Body.Close()
	upSpan.SetAttributes(attribute.Int("http.status", resp.StatusCode))
	upSpan.End()
	if resp.StatusCode == http.StatusPaymentRequired && !held && c.s.Payments != nil && c.payMCP(server, srv, p.Name, body, resp) {
		return // paid (or refused before signing); the request is answered
	}
	if resp.StatusCode/100 != 2 {
		// The upstream's error body is not relayed: it is text from outside,
		// and the gateway answers with its own error.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		c.releaseTool(hold, held, "upstream status "+http.StatusText(resp.StatusCode))
		c.fail(http.StatusBadGateway, audit.StatusUpstreamError, "upstream_error", "upstream answered "+http.StatusText(resp.StatusCode), nil)
		return
	}

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		c.toolSSE(server, p.Name, req, resp, hold, held, price)
		return
	}
	msg, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPBody))
	if err != nil {
		c.toolFailed(hold, held, err)
		return
	}
	switch verdict, kind := c.screenMessage(server, p.Name, msg); {
	case verdict.Blocked:
		c.blocked(server, p.Name, verdict, hold, held, price)
	case kind == msgError, kind == msgUnknown:
		// A JSON-RPC error (or something that is no JSON-RPC response): the call did
		// not produce a result, so nothing is charged. The message passes through
		// the screen above first.
		c.releaseTool(hold, held, "upstream returned no result")
		c.ev.Err = "upstream_rpc_error"
		c.relayJSON(msg)
		c.finish(audit.StatusUpstreamFailed)
	default:
		c.relayJSON(msg)
		c.settleTool(hold, held, tool, price, audit.StatusOK)
	}
}

func (c *call) relayJSON(msg []byte) {
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(msg)
}

type msgKind int

const (
	msgResult msgKind = iota
	msgError
	msgNotification
	msgUnknown
)

// screenMessage screens every string a JSON-RPC message carries (result,
// error, notification params). A message that cannot be parsed is screened as
// raw text, so malformed output is not a way around the screen.
func (c *call) screenMessage(server, tool string, msg []byte) (screen.Verdict, msgKind) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
		Params json.RawMessage `json:"params"`
	}
	var texts []string
	kind := msgUnknown
	if json.Unmarshal(msg, &m) != nil {
		texts = []string{string(msg)}
	} else {
		switch {
		case len(m.Result) > 0:
			kind, texts = msgResult, screen.ExtractTexts(m.Result)
		case len(m.Error) > 0:
			kind, texts = msgError, screen.ExtractTexts(m.Error)
		case m.Method != "":
			kind, texts = msgNotification, screen.ExtractTexts(m.Params)
		default:
			texts = []string{string(msg)}
		}
	}
	return c.s.Screen.Screen(c.ctx, screen.ToolResult{Server: server, Tool: tool, Texts: texts}), kind
}

// blocked refuses a result the screen stopped. The client gets a JSON-RPC
// error with the rule id and nothing of the content. The call did run, so it is
// settled at the tool's price. The log keeps the rule and a hash of the content,
// never the content (I9).
func (c *call) blocked(server, tool string, v screen.Verdict, hold ledger.Hold, held bool, price money.Nanos) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if _, err := c.s.Pool.Exec(ctx, `INSERT INTO screen_events (request_id, run_id, agent_id, server, tool, rule, content_sha256, at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, c.ev.RequestID, c.runID, c.agent.ID, server, tool, v.Rule, v.Hash, time.Now().UTC().Truncate(time.Microsecond)); err != nil {
		c.s.Log.Error("screen event", "request_id", c.ev.RequestID, "err", err)
	}
	c.s.Log.Warn("tool output blocked", "request_id", c.ev.RequestID, "run", c.runID, "server", server, "tool", tool, "rule", v.Rule, "sha256", v.Hash)
	c.span.AddEvent("screen " + v.Rule)
	c.ev.Err = v.Rule
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(mcp.ErrorResponse(c.rpcID, mcp.Error{Code: mcp.CodeScreened, Message: "tool output blocked by the gateway's screen",
		Data: map[string]any{"rule": v.Rule, "request_id": c.ev.RequestID}}))
	c.settleTool(hold, held, server+"/"+tool, price, StatusScreened)
}

// toolSSE streams an SSE reply event by event, screening each before it is
// forwarded. Progress notifications that pass are forwarded at once; a hit
// ends the stream with an error event; what was already forwarded stays
// forwarded (it was screened clean).
func (c *call) toolSSE(server, tool string, req mcp.Request, resp *http.Response, hold ledger.Hold, held bool, price money.Nanos) {
	fl, _ := c.w.(http.Flusher)
	c.w.Header().Set("Content-Type", "text/event-stream")
	c.w.Header().Set("Cache-Control", "no-cache")
	c.w.Header().Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(http.StatusOK)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	flush()
	var final, gone, hit bool
	var verdict screen.Verdict
	var finalKind msgKind
	err := mcp.ReadEvents(resp.Body, func(ev mcp.Event) bool {
		if len(ev.Data) > 0 {
			v, kind := c.screenMessage(server, tool, ev.Data)
			if v.Blocked {
				hit, verdict = true, v
				return false
			}
			if kind == msgResult || kind == msgError {
				final, finalKind = true, kind
			}
		}
		if _, werr := c.w.Write(ev.Raw); werr != nil {
			gone = true
			return false
		}
		flush()
		return !final // the response ends the stream
	})
	switch {
	case hit:
		c.s.Log.Warn("tool output blocked", "request_id", c.ev.RequestID, "run", c.runID, "server", server, "tool", tool, "rule", verdict.Rule, "sha256", verdict.Hash)
		c.blockedStream(server, tool, verdict, hold, held, price)
	case gone:
		c.releaseTool(hold, held, "client disconnected")
		c.finish(audit.StatusClientDisconnected)
	case err != nil || !final:
		c.releaseTool(hold, held, "stream ended without a result")
		c.ev.Err = "upstream_stream_failed"
		if c.killed.Load() {
			c.ev.Err = c.killRule()
			_ = mcp.WriteEvent(c.w, mcp.ErrorResponse(c.rpcID, mcp.Error{Code: mcp.CodePolicyDenied, Message: "the agent or its run was killed",
				Data: map[string]any{"rule": c.killRule(), "request_id": c.ev.RequestID}}))
			flush()
			c.finish(StatusKilled)
			return
		}
		_ = mcp.WriteEvent(c.w, mcp.ErrorResponse(c.rpcID, mcp.Error{Code: mcp.CodeUpstream, Message: "upstream stream failed", Data: map[string]any{"request_id": c.ev.RequestID}}))
		flush()
		c.finish(audit.StatusUpstreamFailed)
	case finalKind == msgError:
		c.releaseTool(hold, held, "upstream returned no result")
		c.ev.Err = "upstream_rpc_error"
		c.finish(audit.StatusUpstreamFailed)
	default:
		c.settleTool(hold, held, server+"/"+tool, price, audit.StatusOK)
	}
}

// blockedStream is blocked for a response that has already started: the error
// goes out as the last event.
func (c *call) blockedStream(server, tool string, v screen.Verdict, hold ledger.Hold, held bool, price money.Nanos) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if _, err := c.s.Pool.Exec(ctx, `INSERT INTO screen_events (request_id, run_id, agent_id, server, tool, rule, content_sha256, at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, c.ev.RequestID, c.runID, c.agent.ID, server, tool, v.Rule, v.Hash, time.Now().UTC().Truncate(time.Microsecond)); err != nil {
		c.s.Log.Error("screen event", "request_id", c.ev.RequestID, "err", err)
	}
	c.ev.Err = v.Rule
	_ = mcp.WriteEvent(c.w, mcp.ErrorResponse(c.rpcID, mcp.Error{Code: mcp.CodeScreened, Message: "tool output blocked by the gateway's screen",
		Data: map[string]any{"rule": v.Rule, "request_id": c.ev.RequestID}}))
	if fl, ok := c.w.(http.Flusher); ok {
		fl.Flush()
	}
	c.settleTool(hold, held, server+"/"+tool, price, StatusScreened)
}

// toolFailed handles a transport failure to the upstream: killed, client gone,
// or unreachable. Nothing arrived, so nothing is charged.
func (c *call) toolFailed(hold ledger.Hold, held bool, err error) {
	switch {
	case c.killed.Load():
		c.releaseTool(hold, held, "agent or run killed")
		c.ev.Err = c.killRule()
		c.fail(http.StatusForbidden, StatusKilled, c.killRule(), "the agent or its run was killed", map[string]any{"type": "policy_denied"})
	case c.r.Context().Err() != nil:
		c.releaseTool(hold, held, "client disconnected")
		c.finish(audit.StatusClientDisconnected)
	default:
		c.s.Log.Warn("mcp upstream", "request_id", c.ev.RequestID, "err", err)
		c.releaseTool(hold, held, "upstream unreachable")
		c.fail(http.StatusBadGateway, audit.StatusUpstreamFailed, "upstream_unavailable", "upstream unreachable", nil)
	}
}

func (c *call) releaseTool(hold ledger.Hold, held bool, reason string) {
	if held {
		c.release(hold, reason)
	}
}

// settleTool charges the tool's price. The audit status says how the call ended.
func (c *call) settleTool(hold ledger.Hold, held bool, tool string, price money.Nanos, status string) {
	if !held { // a free tool: nothing was reserved
		c.finish(status)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 10*time.Second)
	defer cancel()
	st, err := c.s.Ledger.Settle(ctx, ledger.SettleReq{HoldID: hold.ID, Actual: price, IdemKey: "settle:" + hold.ID, Memo: "tool " + tool})
	switch {
	case errors.Is(err, ledger.ErrHoldNotActive):
		c.ev.Cost, c.ev.Err = 0, "settle_lost_to_reconciler" // the reconciler closed the hold first (I3)
		c.finish(audit.StatusSettleLost)
		return
	case err != nil:
		c.s.Log.Error("settle", "request_id", c.ev.RequestID, "err", err) // the hold stays; the reconciler closes it
		c.ev.Err = "settle_failed"
		c.finish(audit.StatusInternal)
		return
	}
	c.closeCharge(hold.ID, st.Charged)
	c.ev.Cost = st.Charged
	c.ext.Unrecovered = st.Unrecovered // I7
	if st.Unrecovered > 0 && c.ev.Err == "" {
		c.ev.Err = "unrecovered_overage"
	}
	c.finish(status)
}

// payMCP handles a free-priced tool that answered HTTP 402 with an x402
// challenge: the same payment path as POST /v1/payments (agent identity,
// policy, approval queue, kill switch, wallet cap), then one retry of the very
// same tools/call with the payment header. It is still the one run step that
// beginRun opened, and the one audit row, now of kind payment.
//
// It returns false when the 402 is not a challenge this gateway can answer
// (no usable PAYMENT-REQUIRED header); the caller then treats it as an
// upstream error. Challenge text from the tool is data: only its payee,
// amount and network are used, and policy decides on exactly those.
func (c *call) payMCP(server string, srv mcp.Server, name string, body []byte, resp *http.Response) bool {
	ch, err := x402.ChallengeFrom(resp)
	if err != nil {
		return false
	}
	req, err := ch.Pick()
	atomic, ok := wallet.Nanos(parseBig(req.Amount))
	if err != nil || !ok || atomic <= 0 {
		return false
	}
	tool := server + "/" + name
	c.ev.Kind = "payment"
	var params struct {
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &params)
	c.payChallenge(payTarget{
		Tool: tool, PolicyTool: tool, Input: params.Params, Ch: ch, Req: req, Amount: atomic,
		Send: func(ctx context.Context, pl x402.PaymentPayload) (*http.Response, error) {
			h, err := x402.EncodeHeader(pl)
			if err != nil {
				return nil, err
			}
			return c.upstream(ctx, srv, body, [2]string{x402.HeaderSignature, h})
		},
		OK: func(out []byte, ctype string, _ x402.SettleResponse, _ money.Nanos) {
			msg := out
			if strings.HasPrefix(ctype, "text/event-stream") {
				fake := &http.Response{Header: http.Header{"Content-Type": {ctype}}, Body: io.NopCloser(bytes.NewReader(out))}
				if m, err := finalMessage(fake, c.rpcID); err == nil {
					msg = m
				}
			}
			// Paid already; the result is still untrusted text and is screened like any other.
			if v, _ := c.screenMessage(server, name, msg); v.Blocked {
				c.blocked(server, name, v, ledger.Hold{}, false, 0)
				return
			}
			c.relayJSON(msg)
			c.finish(audit.StatusOK)
		},
	})
	return true
}
