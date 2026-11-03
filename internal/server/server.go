// Package server is the HTTP gateway: auth -> route/price -> reserve ->
// upstream -> settle -> audit (PLAN "Request flow").
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/pricing"
	"github.com/ws4charlie/agent-gate/internal/provider"
)

// Hold lease. The initial TTL and the extend tick are chosen together:
//
//   - Tick = TTL/10, so a stream survives nine missed ticks (a slow DB, a GC
//     pause) before its hold can expire and be reconciled mid-stream.
//   - TTL = 5 min is far longer than the lag before a finished call appears in
//     OpenRouter's /generation endpoint (seconds). The reconciler only looks at
//     a hold after expires_at, so a "not found" lookup then means the call
//     really produced no generation, not that it is not indexed yet (I6).
const (
	DefaultHoldTTL     = 5 * time.Minute
	DefaultExtendEvery = DefaultHoldTTL / 10
	maxRequestBytes    = 32 << 20
	maxCapture         = 256 << 10
)

type Server struct {
	Pool      *pgxpool.Pool
	Ledger    ledger.Ledger
	Audit     *audit.Store
	Providers provider.Config
	Prices    *pricing.Table
	Keys      func(provider string) string // provider API key from the environment
	Client    *http.Client
	Tracer    trace.Tracer
	Log       *slog.Logger

	MaxHold          money.Nanos
	DefaultMaxTokens int64
	HoldTTL          time.Duration
	ExtendEvery      time.Duration
	StoreBodies      bool // opt-in; bodies are redacted before storage (I9)
}

func (s *Server) defaults() {
	if s.Client == nil {
		// No overall timeout: streams may run long. Header timeout bounds a dead upstream.
		s.Client = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 120 * time.Second, MaxIdleConnsPerHost: 64}}
	}
	if s.Tracer == nil {
		s.Tracer = noop.NewTracerProvider().Tracer("agentgate")
	}
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.HoldTTL == 0 {
		s.HoldTTL = DefaultHoldTTL
	}
	if s.ExtendEvery == 0 {
		s.ExtendEvery = s.HoldTTL / 10
	}
	if s.DefaultMaxTokens == 0 {
		s.DefaultMaxTokens = 1024
	}
	if s.MaxHold == 0 {
		s.MaxHold = 5_000_000_000
	}
}

func (s *Server) Handler() http.Handler {
	s.defaults()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handle(provider.FormatOpenAI))
	mux.HandleFunc("POST /v1/messages", s.handle(provider.FormatAnthropic))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

// ---- errors ----

func writeErr(w http.ResponseWriter, f provider.Format, status int, code, msg string, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	e := map[string]any{"type": code, "code": code, "message": msg}
	for k, v := range extra {
		e[k] = v
	}
	var body any = map[string]any{"error": e}
	if f == provider.FormatAnthropic {
		body = map[string]any{"type": "error", "error": e}
	}
	_ = json.NewEncoder(w).Encode(body)
}

// ---- request state ----

type call struct {
	s     *Server
	w     http.ResponseWriter
	r     *http.Request
	f     provider.Format
	ctx   context.Context
	span  trace.Span
	start time.Time
	ev    audit.Event
	ext   audit.Extra
	done  bool
}

// finish writes the audit row and closes the span; it runs once. It uses a
// context detached from the client: a disconnect must not lose the audit row.
func (c *call) finish(status string) {
	if c.done {
		return
	}
	c.done = true
	c.ev.Status = status
	c.ev.LatencyMs = time.Since(c.start).Milliseconds()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if err := c.s.Audit.Write(ctx, c.ev, c.ext); err != nil {
		c.s.Log.Error("audit write failed", "request_id", c.ev.RequestID, "err", err)
	}
	// Span attributes are ids, counts and status only: never prompts or keys (I9).
	c.span.SetAttributes(attribute.String("request.status", status), attribute.String("model", c.ev.Model),
		attribute.String("provider", c.ev.Provider), attribute.String("cost.source", c.ev.CostSource),
		attribute.Int64("tokens.input", c.ev.Usage.InputTokens), attribute.Int64("tokens.output", c.ev.Usage.OutputTokens),
		attribute.Int64("cost.nanos", int64(c.ev.Cost)))
	if status != audit.StatusOK {
		c.span.SetStatus(codes.Error, status)
	}
	c.span.End()
	c.s.Log.Info("request", "request_id", c.ev.RequestID, "account", c.ev.AccountID, "model", c.ev.Model,
		"status", status, "cost_nanos", int64(c.ev.Cost), "latency_ms", c.ev.LatencyMs)
}

func (c *call) fail(httpStatus int, status, code, msg string, extra map[string]any) {
	c.ev.Err = code
	writeErr(c.w, c.f, httpStatus, code, msg, extra)
	c.finish(status)
}

func (c *call) child(name string) (context.Context, trace.Span) {
	return c.s.Tracer.Start(c.ctx, name)
}

// ---- the flow ----

func (s *Server) handle(f provider.Format) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := uuid.NewString()
		ctx, span := s.Tracer.Start(r.Context(), "gateway.request",
			trace.WithAttributes(attribute.String("request.id", reqID), attribute.String("api.format", string(f))))
		c := &call{s: s, w: w, r: r, f: f, ctx: ctx, span: span, start: time.Now(),
			ev: audit.Event{RequestID: reqID, Kind: "model", At: time.Now()}}
		w.Header().Set("X-Request-Id", reqID)
		c.run()
	}
}

func (c *call) run() {
	s := c.s

	// 1. authenticate
	acct, err := authenticate(c.ctx, s.Pool, c.r)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			c.fail(http.StatusUnauthorized, audit.StatusUnauthorized, "unauthorized", "missing or invalid API key", nil)
		} else {
			s.Log.Error("auth lookup", "err", err)
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		}
		return
	}
	c.ev.AccountID = acct
	c.span.SetAttributes(attribute.String("account.id", acct))

	body, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, maxRequestBytes))
	if err != nil {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "cannot read body", nil)
		return
	}
	var hdr struct {
		Model               string `json:"model"`
		Stream              bool   `json:"stream"`
		MaxTokens           int64  `json:"max_tokens"`
		MaxCompletionTokens int64  `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &hdr); err != nil || hdr.Model == "" {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "body must be JSON with a model", nil)
		return
	}
	c.ev.Model = hdr.Model
	if s.StoreBodies {
		red := audit.Redact(string(body))
		c.ext.RequestBody = &red
	}

	// 2. route and price
	target, ok := s.Providers.Resolve(hdr.Model)
	if !ok {
		c.fail(http.StatusBadRequest, audit.StatusUnroutable, "unknown_model", "no route for model", nil)
		return
	}
	c.ev.Provider = target.Name
	price, err := s.Prices.Lookup(target.Model)
	if err != nil {
		c.fail(http.StatusBadRequest, audit.StatusUnknownModelPrice, "unknown_model_price", "no price for model "+target.Model, nil)
		return
	}
	key := s.Keys(target.Name)
	if key == "" {
		c.fail(http.StatusBadGateway, audit.StatusInternal, "provider_not_configured", "provider key not configured", nil)
		return
	}

	// policy: T4 puts per-agent rules here; the span exists now so traces keep their shape.
	_, pspan := c.child("policy")
	pspan.SetAttributes(attribute.String("decision", "allow"))
	pspan.End()

	// 3. estimate the worst case and reserve it
	maxOut := hdr.MaxTokens
	if hdr.MaxCompletionTokens > 0 {
		maxOut = hdr.MaxCompletionTokens
	}
	if maxOut <= 0 {
		maxOut = s.DefaultMaxTokens
	}
	hold, ok := c.reserve(body, maxOut, price)
	if !ok {
		return
	}
	c.ext.HoldID = hold.ID

	// release on any early exit before money is consumed
	up, err := provider.Build(target, key, c.f, c.r.Header, body, hdr.Stream, s.DefaultMaxTokens)
	if err != nil {
		c.release(hold, "bad request")
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}

	// 4. upstream
	stopLease := c.lease(hold.ID)
	defer stopLease()
	upCtx, upSpan := c.child("upstream")
	upStart := time.Now()
	req, _ := http.NewRequestWithContext(upCtx, http.MethodPost, up.URL, bytes.NewReader(up.Body))
	req.Header = up.Header
	resp, err := s.Client.Do(req)
	if err != nil {
		upSpan.End()
		c.release(hold, "upstream unreachable")
		if c.r.Context().Err() != nil {
			c.finish(audit.StatusClientDisconnected)
			return
		}
		c.fail(http.StatusBadGateway, audit.StatusUpstreamFailed, "upstream_unavailable", "upstream unreachable", nil)
		return
	}
	defer resp.Body.Close()
	upSpan.SetAttributes(attribute.Int("http.status", resp.StatusCode))

	if resp.StatusCode/100 != 2 {
		// 429, 500, ...: pass the error through; nothing was generated, so nothing is charged.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		upSpan.End()
		provider.CopyResponseHeaders(c.w.Header(), resp.Header)
		c.w.WriteHeader(resp.StatusCode)
		_, _ = c.w.Write(errBody)
		c.release(hold, "upstream status "+http.StatusText(resp.StatusCode))
		c.ev.Err = "upstream_status_" + http.StatusText(resp.StatusCode)
		c.ev.OverheadMs = time.Since(c.start).Milliseconds() - time.Since(upStart).Milliseconds()
		c.finish(audit.StatusUpstreamError)
		return
	}

	// OpenRouter puts the id that /generation understands in a header on every
	// response, also on the Anthropic surface whose body id (msg_...) cannot be
	// looked up. Attach it before reading the body: it wins over any body id and
	// narrows the window in which a crash leaves an unattached hold (I6).
	genID := resp.Header.Get("X-Generation-Id")
	c.attach(hold, target.Name, genID)

	var res outcome
	if hdr.Stream {
		res = c.stream(hold, resp, up, target.Name, genID)
	} else {
		res = c.unary(hold, resp, up, target.Name, genID)
	}
	upSpan.End()
	c.ev.OverheadMs = time.Since(c.start).Milliseconds() - time.Since(upStart).Milliseconds()

	// 5. settle from what was actually received (I8)
	c.settle(hold, price, len(body), res)
}

// reserve prices the worst case and holds it. A repeated Idempotency-Key is
// refused here, before any upstream call (PLAN flow step 7).
func (c *call) reserve(body []byte, maxOut int64, price pricing.Price) (ledger.Hold, bool) {
	s := c.s
	// Input is estimated high (bytes/3); priced at the dearer of plain input and
	// cache write so a cache-writing request cannot exceed its hold.
	inPrice := max(price.Input, price.CacheWrite)
	inCost, e1 := money.Cost(pricing.EstimateTokens(len(body)), inPrice)
	outCost, e2 := money.Cost(maxOut, price.Output)
	if e1 != nil || e2 != nil {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "request too large to price", nil)
		return ledger.Hold{}, false
	}
	amount := min(inCost+outCost, s.MaxHold)
	amount = max(amount, 1)

	idem := strings.TrimSpace(c.r.Header.Get("Idempotency-Key"))
	if len(idem) > 255 {
		c.fail(http.StatusBadRequest, audit.StatusBadRequest, "bad_request", "Idempotency-Key too long", nil)
		return ledger.Hold{}, false
	}
	generated := idem == ""
	if generated {
		idem = "gen-" + c.ev.RequestID
	}

	rctx, rspan := c.child("reserve")
	defer rspan.End()

	// Claim the key. ledger.Reserve replays the first hold for a used key and
	// cannot say "this is a duplicate", so the gateway keeps its own claim row,
	// which also remembers the original request id for the 409 body.
	tag, err := s.Pool.Exec(rctx, `INSERT INTO gateway_requests (account_id, idem_key, request_id) VALUES ($1,$2,$3)
		ON CONFLICT DO NOTHING`, c.ev.AccountID, idem, c.ev.RequestID)
	if err != nil {
		s.Log.Error("claim", "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return ledger.Hold{}, false
	}
	if tag.RowsAffected() == 0 {
		var orig string
		_ = s.Pool.QueryRow(rctx, `SELECT request_id FROM gateway_requests WHERE account_id=$1 AND idem_key=$2`,
			c.ev.AccountID, idem).Scan(&orig)
		c.fail(http.StatusConflict, audit.StatusDuplicate, "duplicate_request", "this Idempotency-Key was already used",
			map[string]any{"original_request_id": orig})
		return ledger.Hold{}, false
	}
	unclaim := func() {
		_, _ = s.Pool.Exec(context.WithoutCancel(rctx), `DELETE FROM gateway_requests WHERE account_id=$1 AND idem_key=$2 AND request_id=$3`,
			c.ev.AccountID, idem, c.ev.RequestID)
	}

	hold, err := s.Ledger.Reserve(rctx, ledger.ReserveReq{AccountID: c.ev.AccountID, Amount: amount, IdemKey: idem,
		Kind: ledger.KindModel, TTL: s.HoldTTL, Memo: "model " + c.ev.Model})
	switch {
	case err == nil && hold.Status != "active":
		// Used key whose hold is already terminal: same answer as a duplicate (never call upstream).
		c.fail(http.StatusConflict, audit.StatusDuplicate, "duplicate_request", "this Idempotency-Key was already used", nil)
		return ledger.Hold{}, false
	case err == nil:
		c.ext.HoldID = hold.ID
		return hold, true
	}
	unclaim() // nothing was held, so the client may retry the same key after fixing the cause
	switch {
	case errors.Is(err, ledger.ErrInsufficientFunds):
		c.fail(http.StatusPaymentRequired, audit.StatusInsufficientFunds, "insufficient_funds", "insufficient funds", nil)
	case errors.Is(err, ledger.ErrAccountInactive):
		c.fail(http.StatusForbidden, audit.StatusAccountInactive, "account_inactive", "account inactive", nil)
	case errors.Is(err, ledger.ErrIdempotencyConflict):
		c.fail(http.StatusUnprocessableEntity, audit.StatusIdempotencyConflict, "idempotency_conflict", "Idempotency-Key reused with a different request", nil)
	default:
		s.Log.Error("reserve", "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
	}
	return ledger.Hold{}, false
}

// lease keeps the hold alive while the upstream call runs, so the reconciler
// cannot close a live request (I6). Returns a stop func.
func (c *call) lease(holdID string) func() {
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(c.s.ExtendEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
				if err := c.s.Ledger.Extend(ctx, holdID, c.s.HoldTTL); err != nil {
					c.s.Log.Warn("extend hold", "request_id", c.ev.RequestID, "err", err)
				}
				cancel()
			}
		}
	}()
	return func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		<-finished
	}
}

func (c *call) release(h ledger.Hold, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if _, err := c.s.Ledger.Release(ctx, h.ID, "release:"+h.ID, reason); err != nil && !errors.Is(err, ledger.ErrHoldNotActive) {
		c.s.Log.Error("release", "request_id", c.ev.RequestID, "err", err) // reconciler will close it
	}
}

var errStreamIncomplete = errors.New("stream ended without a terminal event or carried an error")

// outcome is what the transfer phase learned.
type outcome struct {
	usage       provider.Usage
	seen        bool // the upstream sent a usage object
	outChars    int
	got         bool // any response bytes reached us
	clientGone  bool
	upstreamErr error  // stream died / read failed
	response    []byte // captured only when StoreBodies
}

type capture struct {
	on  bool
	buf bytes.Buffer
}

func (c *capture) Write(b []byte) (int, error) {
	if c.on && c.buf.Len() < maxCapture {
		c.buf.Write(b[:min(len(b), maxCapture-c.buf.Len())])
	}
	return len(b), nil
}

func (c *call) unary(h ledger.Hold, resp *http.Response, up provider.Upstream, name, genID string) outcome {
	var res outcome
	// Nothing has been written to the client yet, so a failure here can still
	// become an honest 502 instead of an empty 200. Settling rules are unchanged.
	failed := func(err error) outcome {
		res.upstreamErr = err
		res.clientGone = c.r.Context().Err() != nil
		if !res.clientGone {
			writeErr(c.w, c.f, http.StatusBadGateway, "upstream_failed", "upstream response could not be read", nil)
		}
		return res
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBytes))
	res.got = len(data) > 0
	if err != nil {
		return failed(err)
	}
	res.usage, res.outChars, res.seen = provider.ParseBody(up.Wire, data)
	if genID != "" {
		res.usage.UpstreamID = genID
	}
	out := data
	if up.Translate {
		t, err := provider.MessagesToChat(data, c.ev.Model)
		if err != nil {
			return failed(err)
		}
		out = t
	}
	provider.CopyResponseHeaders(c.w.Header(), resp.Header)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	if _, err := c.w.Write(out); err != nil {
		res.clientGone = true
	}
	if c.s.StoreBodies {
		res.response = out
	}
	if genID == "" {
		c.attach(h, name, res.usage.UpstreamID) // provider without the header: body id
	}
	return res
}

// stream copies SSE bytes to the client as they arrive and tees them to the
// usage parser. Nothing is buffered beyond one read (PLAN flow step 5).
func (c *call) stream(h ledger.Hold, resp *http.Response, up provider.Upstream, name, genID string) outcome {
	var res outcome
	parser := provider.NewParser(up.Wire)
	parser.SetUpstreamID(genID)
	cap := &capture{on: c.s.StoreBodies}
	fl, _ := c.w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	provider.CopyResponseHeaders(c.w.Header(), resp.Header)
	c.w.Header().Set("Content-Type", "text/event-stream")
	c.w.Header().Set("Cache-Control", "no-cache")
	c.w.WriteHeader(http.StatusOK)
	flush()

	attached := genID != "" // already attached from the header
	after := func() {
		if u, _ := parser.Usage(); !attached && u.UpstreamID != "" {
			attached = true
			c.attach(h, name, u.UpstreamID)
		}
	}
	write := func(b []byte) bool {
		if _, err := c.w.Write(b); err != nil {
			res.clientGone = true
			return false
		}
		flush()
		cap.Write(b)
		return true
	}

	if up.Translate {
		tr := provider.NewSSETranslator(c.ev.Model)
		br := bufio.NewReaderSize(resp.Body, 64<<10)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				res.got = true
				parser.Write(line)
				if d, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
					for _, frame := range tr.Event(bytes.TrimSpace(d)) {
						if !write(frame) {
							break
						}
					}
				}
				after()
			}
			if res.clientGone {
				break
			}
			if err != nil {
				if err != io.EOF {
					res.upstreamErr = err
				}
				break
			}
		}
	} else {
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				res.got = true
				if !write(buf[:n]) {
					break
				}
				parser.Write(buf[:n])
				after()
			}
			if err != nil {
				if err != io.EOF {
					res.upstreamErr = err
				}
				break
			}
		}
	}
	if res.clientGone || c.r.Context().Err() != nil {
		res.clientGone = true
		res.upstreamErr = nil
	}
	res.usage, res.seen = parser.Usage()
	res.outChars = parser.OutChars
	if !res.clientGone && res.upstreamErr == nil && parser.Failed() {
		// A clean close without the terminal event, or an in-band error, is a
		// failed stream, not a success (I8).
		res.upstreamErr = errStreamIncomplete
	}
	res.response = cap.buf.Bytes()
	return res
}

func (c *call) attach(h ledger.Hold, name, upstreamID string) {
	if upstreamID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if err := c.s.Ledger.AttachUpstream(ctx, h.ID, name, upstreamID); err != nil {
		c.s.Log.Warn("attach upstream", "request_id", c.ev.RequestID, "err", err)
	}
}

// settle turns the outcome into a charge. Cost source priority (PLAN step 6):
// provider-reported cost, then the price table on provider token counts, then
// a token estimate. A stream that died still pays for what arrived (I8).
func (c *call) settle(h ledger.Hold, price pricing.Price, reqBytes int, res outcome) {
	s := c.s
	ctx, span := c.child("settle")
	defer span.End()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	status := audit.StatusOK
	switch {
	case res.clientGone:
		status = audit.StatusClientDisconnected
	case res.upstreamErr != nil:
		status = audit.StatusUpstreamFailed
		c.ev.Err = "upstream_stream_failed"
	}

	// Nothing came back at all: nothing to charge, release the whole hold.
	if !res.got && !res.seen {
		c.release(h, "no response received")
		if status == audit.StatusOK {
			status = audit.StatusUpstreamFailed
		}
		c.finish(status)
		return
	}

	u := res.usage
	var cost money.Nanos
	var err error
	switch {
	case res.seen && u.CostNanos != nil:
		cost, c.ev.CostSource = *u.CostNanos, audit.SourceProvider
	case res.seen:
		cost, err = price.Cost(u)
		c.ev.CostSource = audit.SourcePriceTable
	default:
		u.InputTokens = pricing.EstimateTokens(reqBytes)
		u.OutputTokens = pricing.EstimateTokens(res.outChars)
		cost, err = price.Cost(u)
		c.ev.CostSource = audit.SourceEstimated
	}
	if err != nil {
		// The response is already on its way to the client: never write to it.
		// The hold stays active and the reconciler closes it.
		s.Log.Error("price", "request_id", c.ev.RequestID, "err", err)
		c.ev.Err = "price_failed"
		c.finish(audit.StatusInternal)
		return
	}
	c.ev.Usage = u
	c.ev.Cost = cost
	if s.StoreBodies && len(res.response) > 0 {
		red := audit.Redact(string(res.response))
		c.ext.ResponseBody = &red
	}

	st, err := s.Ledger.Settle(ctx, ledger.SettleReq{HoldID: h.ID, Actual: cost, IdemKey: "settle:" + h.ID,
		Memo: "model " + c.ev.Model + " (" + c.ev.CostSource + ")"})
	switch {
	case errors.Is(err, ledger.ErrHoldNotActive):
		// The reconciler closed the hold first. Record the real usage; never
		// retry, the money was already returned or charged by the reconciler (I3).
		c.ev.Cost = 0
		c.ev.Err = "settle_lost_to_reconciler"
		c.ext.Unmetered = false
		c.finish(audit.StatusSettleLost)
		return
	case err != nil:
		s.Log.Error("settle", "request_id", c.ev.RequestID, "err", err)
		c.ev.Err = "settle_failed" // hold stays active; the reconciler will close it
		c.finish(audit.StatusInternal)
		return
	}
	c.ev.Cost = st.Charged
	c.ext.Unrecovered = st.Unrecovered // I7: flagged on the audit row
	if st.Unrecovered > 0 {
		c.ev.Err = "unrecovered_overage"
	}
	c.finish(status)
}
