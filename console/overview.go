package console

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

// Audit statuses grouped the way an operator reads them. A refusal is the
// gateway saying no on purpose; an error is something that failed.
var (
	refusalStatuses = []string{"policy_denied", "approval_denied", "approval_expired", "approval_mismatch",
		"approval_required", "killed", "insufficient_funds", "account_inactive", "unauthorized"}
	quietStatuses = []string{"ok", "reconciled_settled", "reconciled_unmetered", "duplicate_request", "screened"}
)

// toolFilter keeps audit rows that are tool calls or payments, not MCP
// protocol traffic such as tools/list (whose model field is the method).
const toolFilter = `(kind = 'payment' OR (kind = 'mcp' AND model <> '' AND model <> 'initialize' AND model <> 'ping'
	AND split_part(model, '/', 1) NOT IN ('tools', 'resources', 'prompts', 'notifications', 'server', 'completion',
		'logging', 'sampling', 'roots', 'elicitation', 'tasks')))`

type usd struct {
	Nanos int64  `json:"nanos"`
	USD   string `json:"usd"`
}

func dollars(n int64) usd { return usd{Nanos: n, USD: money.Nanos(n).String()} }

type scopeView struct {
	Models            []string `json:"models"`
	Tools             []string `json:"tools"`
	Payees            []string `json:"payees"`
	DailyBudget       usd      `json:"daily_budget"`
	TaskBudget        usd      `json:"task_budget"`
	VelocityPerMinute usd      `json:"velocity_per_minute"`
	ApprovalThreshold usd      `json:"approval_threshold"`
}

func viewScope(s policy.Scope) scopeView {
	return scopeView{Models: nonNil(s.Models), Tools: nonNil(s.Tools), Payees: nonNil(s.Payees),
		DailyBudget: dollars(int64(s.DailyBudget)), TaskBudget: dollars(int64(s.TaskBudget)),
		VelocityPerMinute: dollars(int64(s.VelocityPerMinute)), ApprovalThreshold: dollars(int64(s.ApprovalThreshold))}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type agentView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Console   bool      `json:"console"` // made by the console for a template
	AccountID string    `json:"account_id"`
	Balance   usd       `json:"balance"`
	Available usd       `json:"available"`
	Scope     scopeView `json:"scope"`

	Today     usd        `json:"today"` // counted against the daily budget (held plus settled)
	Spend7d   spendKinds `json:"spend_7d"`
	Calls24h  int64      `json:"calls_24h"`
	Refused   int64      `json:"refused_24h"`
	Errors    int64      `json:"errors_24h"`
	Blocked   int64      `json:"blocked_24h"`
	LastSeen  *time.Time `json:"last_seen"`
	Hourly    []float64  `json:"hourly"` // USD per hour, last 24 hours, oldest first
	CallsHour []int64    `json:"calls_hourly"`
	Models    []lineItem `json:"models"` // this agent's 7-day spend by model
	Tools     []lineItem `json:"tools"`  // and by tool or paid endpoint
}

type spendKinds struct {
	Model   usd `json:"model"`
	Tool    usd `json:"tool"`
	Payment usd `json:"payment"`
	Total   usd `json:"total"`
}

type lineItem struct {
	Name  string     `json:"name"`
	Kind  string     `json:"kind"`
	Calls int64      `json:"calls"`
	Spend usd        `json:"spend"`
	Last  *time.Time `json:"last"`
}

type overview struct {
	At       time.Time    `json:"at"`
	Totals   totals       `json:"totals"`
	Wallet   *walletView  `json:"wallet,omitempty"`
	Explorer string       `json:"explorer"` // block explorer base URL, or ""
	Agents   []*agentView `json:"agents"`
	Models   []lineItem   `json:"models"`
	Tools    []lineItem   `json:"tools"`
	Run      runReady     `json:"run"`
}

type totals struct {
	Today    usd   `json:"today"`
	Spend7d  usd   `json:"spend_7d"`
	Calls24h int64 `json:"calls_24h"`
	Refused  int64 `json:"refused_24h"`
	Errors   int64 `json:"errors_24h"`
	Blocked  int64 `json:"blocked_24h"`
	Active   int   `json:"active_agents"`
}

type runReady struct {
	Enabled bool     `json:"enabled"`
	Reason  string   `json:"reason,omitempty"`
	Gateway string   `json:"gateway"`
	Runners []string `json:"runners"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ov, err := s.overview(ctx)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (s *Server) overview(ctx context.Context) (*overview, error) {
	now := time.Now().UTC()
	list, err := s.agents.List(ctx)
	if err != nil {
		return nil, err
	}
	ov := &overview{At: now, Agents: []*agentView{}, Models: []lineItem{}, Tools: []lineItem{}}
	byID := map[string]*agentView{}
	for _, a := range list {
		if a.Status == policy.StatusRevoked {
			continue
		}
		v := &agentView{ID: a.ID, Name: a.Name, Status: string(a.Status), AccountID: a.AccountID,
			Console: strings.HasPrefix(a.Name, agentPrefix), Scope: viewScope(a.Scope),
			Hourly: make([]float64, 24), CallsHour: make([]int64, 24), Models: []lineItem{}, Tools: []lineItem{},
			Today: dollars(0), Balance: dollars(0), Available: dollars(0),
			Spend7d: spendKinds{Model: dollars(0), Tool: dollars(0), Payment: dollars(0), Total: dollars(0)}}
		ov.Agents = append(ov.Agents, v)
		byID[a.ID] = v
		if a.Status == policy.StatusActive {
			ov.Totals.Active++
		}
	}
	if err := s.fillAccounts(ctx, byID); err != nil {
		return nil, err
	}
	if err := s.fillToday(ctx, byID, now); err != nil {
		return nil, err
	}
	if err := s.fillAudit(ctx, byID, now); err != nil {
		return nil, err
	}
	for _, v := range ov.Agents {
		ov.Totals.Today.Nanos += v.Today.Nanos
		ov.Totals.Spend7d.Nanos += v.Spend7d.Total.Nanos
		ov.Totals.Calls24h += v.Calls24h
		ov.Totals.Refused += v.Refused
		ov.Totals.Errors += v.Errors
		ov.Totals.Blocked += v.Blocked
	}
	ov.Totals.Today = dollars(ov.Totals.Today.Nanos)
	ov.Totals.Spend7d = dollars(ov.Totals.Spend7d.Nanos)
	if ov.Models, err = s.lineItems(ctx, `kind = 'model'`, now, byID, func(v *agentView, li lineItem) { v.Models = append(v.Models, li) }); err != nil {
		return nil, err
	}
	if ov.Tools, err = s.lineItems(ctx, toolFilter, now, byID, func(v *agentView, li lineItem) { v.Tools = append(v.Tools, li) }); err != nil {
		return nil, err
	}
	if s.cfg.WalletAddress != "" {
		ov.Wallet = s.wallet(ctx)
	}
	ov.Run = s.runReady()
	ov.Explorer = s.cfg.Explorer
	return ov, nil
}

func (s *Server) fillAccounts(ctx context.Context, byID map[string]*agentView) error {
	rows, err := s.pool.Query(ctx, `SELECT g.id, a.balance_nanos, a.held_nanos FROM agents g JOIN accounts a ON a.id = g.account_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var bal, held int64
		if err := rows.Scan(&id, &bal, &held); err != nil {
			return err
		}
		if v := byID[id]; v != nil {
			v.Balance, v.Available = dollars(bal), dollars(bal-held)
		}
	}
	return rows.Err()
}

func (s *Server) fillToday(ctx context.Context, byID map[string]*agentView, now time.Time) error {
	rows, err := s.pool.Query(ctx, `SELECT agent_id, spent_nanos FROM budget_windows WHERE window_key = $1`,
		"day:"+now.Format("2006-01-02"))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		if v := byID[id]; v != nil {
			v.Today = dollars(n)
		}
	}
	return rows.Err()
}

func (s *Server) fillAudit(ctx context.Context, byID map[string]*agentView, now time.Time) error {
	// Spend by kind over 7 days.
	rows, err := s.pool.Query(ctx, `SELECT agent_id,
		COALESCE(SUM(cost_nanos) FILTER (WHERE kind = 'model'), 0)::bigint,
		COALESCE(SUM(cost_nanos) FILTER (WHERE kind = 'mcp'), 0)::bigint,
		COALESCE(SUM(cost_nanos) FILTER (WHERE kind = 'payment'), 0)::bigint,
		MAX(at)
		FROM audit_events WHERE at > $1 AND agent_id <> '' GROUP BY agent_id`, now.Add(-7*24*time.Hour))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var m, t, p int64
		var last time.Time
		if err := rows.Scan(&id, &m, &t, &p, &last); err != nil {
			rows.Close()
			return err
		}
		if v := byID[id]; v != nil {
			v.Spend7d = spendKinds{Model: dollars(m), Tool: dollars(t), Payment: dollars(p), Total: dollars(m + t + p)}
			l := last
			v.LastSeen = &l
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Hourly spend and outcome counts over 24 hours.
	start := now.Truncate(time.Hour).Add(-23 * time.Hour)
	rows, err = s.pool.Query(ctx, `SELECT agent_id, date_trunc('hour', at AT TIME ZONE 'UTC'),
		COALESCE(SUM(cost_nanos), 0)::bigint, COUNT(*),
		COUNT(*) FILTER (WHERE status = ANY($2)),
		COUNT(*) FILTER (WHERE NOT (status = ANY($2)) AND NOT (status = ANY($3))),
		COUNT(*) FILTER (WHERE status = 'screened')
		FROM audit_events WHERE at >= $1 AND agent_id <> '' GROUP BY 1, 2`, start, refusalStatuses, quietStatuses)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var hour time.Time
		var cost, calls, refused, errs, blocked int64
		if err := rows.Scan(&id, &hour, &cost, &calls, &refused, &errs, &blocked); err != nil {
			return err
		}
		v := byID[id]
		if v == nil {
			continue
		}
		i := int(hour.UTC().Sub(start) / time.Hour)
		if i >= 0 && i < 24 {
			v.Hourly[i] += float64(cost) / 1e9
			v.CallsHour[i] += calls
		}
		v.Calls24h += calls
		v.Refused += refused
		v.Errors += errs
		v.Blocked += blocked
	}
	return rows.Err()
}

// lineItems sums 7 days of audit rows per model or tool name: the totals it
// returns, and each agent's own share handed to add.
func (s *Server) lineItems(ctx context.Context, where string, now time.Time,
	byID map[string]*agentView, add func(*agentView, lineItem)) ([]lineItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT agent_id, model, kind, COUNT(*), COALESCE(SUM(cost_nanos), 0)::bigint, MAX(at)
		FROM audit_events WHERE at > $1 AND model <> '' AND `+where+`
		GROUP BY agent_id, model, kind`, now.Add(-7*24*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ name, kind string }
	total := map[key]*lineItem{}
	var order []key
	for rows.Next() {
		var id string
		var li lineItem
		var cost int64
		var last time.Time
		if err := rows.Scan(&id, &li.Name, &li.Kind, &li.Calls, &cost, &last); err != nil {
			return nil, err
		}
		li.Spend, li.Last = dollars(cost), &last
		if v := byID[id]; v != nil {
			add(v, li)
		}
		k := key{li.Name, li.Kind}
		t := total[k]
		if t == nil {
			t = &lineItem{Name: li.Name, Kind: li.Kind, Spend: dollars(0)}
			total[k] = t
			order = append(order, k)
		}
		t.Calls += li.Calls
		t.Spend = dollars(t.Spend.Nanos + cost)
		if t.Last == nil || last.After(*t.Last) {
			l := last
			t.Last = &l
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]lineItem, 0, len(order))
	for _, k := range order {
		out = append(out, *total[k])
	}
	sortItems(out)
	for _, v := range byID {
		sortItems(v.Models)
		sortItems(v.Tools)
	}
	if len(out) > 30 {
		out = out[:30]
	}
	return out, nil
}

// sortItems puts the biggest spend first, then the most calls.
func sortItems(items []lineItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Spend.Nanos != items[j].Spend.Nanos {
			return items[i].Spend.Nanos > items[j].Spend.Nanos
		}
		return items[i].Calls > items[j].Calls
	})
}

// ---- event feed: refusals, errors, screen blocks ----

type event struct {
	At     time.Time `json:"at"`
	Agent  string    `json:"agent"`
	Class  string    `json:"class"` // refused | error | blocked
	Kind   string    `json:"kind"`
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Detail string    `json:"detail"`
	RunID  string    `json:"run_id"`
}

// eventFilter narrows the event feed. Class is "" for every problem
// (refused, error, blocked), one of those or "notice", or "any" for every row
// that is not a plain success (a run's own page uses that).
type eventFilter struct {
	RunID string
	Agent string // agent name or id
	Class string
	Since time.Duration
	Limit int
}

var eventClasses = map[string]bool{"": true, "any": true, "refused": true, "error": true, "blocked": true, "notice": true}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	q := r.URL.Query()
	f := eventFilter{Agent: q.Get("agent"), Class: q.Get("class"), Limit: 200}
	if !eventClasses[f.Class] {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: class must be refused, error, blocked, notice or any", errInvalid))
		return
	}
	if v := q.Get("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: since must be a duration such as 24h", errInvalid))
			return
		}
		f.Since = d
	}
	ev, err := s.events(ctx, f)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ev})
}

// events returns the newest rows that are not plain success, newest first.
// The classes match the overview's counts, so a count links to its rows.
func (s *Server) events(ctx context.Context, f eventFilter) ([]event, error) {
	q := `SELECT e.at, COALESCE(g.name, e.agent_id), e.kind, e.model, e.status, e.err, e.run_id,
		COALESCE((SELECT d.rule || ': ' || d.reason FROM policy_decisions d WHERE d.request_id = e.request_id
			AND d.decision <> 'allow' ORDER BY d.id DESC LIMIT 1), '')
		FROM audit_events e LEFT JOIN agents g ON g.id = e.agent_id
		WHERE NOT (e.status = ANY($1))`
	args := []any{[]string{"ok", "reconciled_settled", "duplicate_request"}}
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	switch f.Class {
	case "":
		q += ` AND e.status <> 'reconciled_unmetered'`
	case "refused":
		q += ` AND e.status = ANY(` + arg(refusalStatuses) + `)`
	case "blocked":
		q += ` AND e.status = 'screened'`
	case "notice":
		q += ` AND e.status = 'reconciled_unmetered'`
	case "error":
		q += ` AND NOT (e.status = ANY(` + arg(refusalStatuses) + `)) AND NOT (e.status = ANY(` + arg(quietStatuses) + `))`
	}
	if f.RunID != "" {
		q += ` AND e.run_id = ` + arg(f.RunID)
	}
	if f.Agent != "" {
		p := arg(f.Agent)
		q += ` AND (g.name = ` + p + ` OR e.agent_id = ` + p + `)`
	}
	if f.Since > 0 {
		q += ` AND e.at > ` + arg(time.Now().Add(-f.Since))
	}
	q += ` ORDER BY e.at DESC LIMIT ` + arg(f.Limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []event{}
	for rows.Next() {
		var e event
		var errText, why string
		if err := rows.Scan(&e.At, &e.Agent, &e.Kind, &e.Name, &e.Status, &errText, &e.RunID, &why); err != nil {
			return nil, err
		}
		e.Class = classify(e.Status)
		e.Detail = errText
		if why != "" {
			e.Detail = why
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func classify(status string) string {
	switch {
	case status == "screened":
		return "blocked"
	case contains(refusalStatuses, status):
		return "refused"
	case status == "reconciled_unmetered":
		return "notice"
	}
	return "error"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---- controls ----

type scopeEdit struct {
	Models            []string `json:"models"`
	Tools             []string `json:"tools"`
	Payees            []string `json:"payees"`
	DailyBudget       string   `json:"daily_budget_usd"`
	TaskBudget        string   `json:"task_budget_usd"`
	VelocityPerMinute string   `json:"velocity_per_minute_usd"`
	ApprovalThreshold string   `json:"approval_threshold_usd"`
}

func (e scopeEdit) scope() (policy.Scope, error) {
	sc := policy.Scope{Models: clean(e.Models), Tools: clean(e.Tools), Payees: clean(e.Payees)}
	for _, f := range []struct {
		name string
		v    string
		dst  *money.Nanos
	}{{"daily budget", e.DailyBudget, &sc.DailyBudget}, {"task budget", e.TaskBudget, &sc.TaskBudget},
		{"velocity", e.VelocityPerMinute, &sc.VelocityPerMinute}, {"approval threshold", e.ApprovalThreshold, &sc.ApprovalThreshold}} {
		n, err := parseUSD(f.v)
		if err != nil {
			return sc, fmt.Errorf("%w: %s: %v", errInvalid, f.name, err)
		}
		*f.dst = n
	}
	for _, p := range sc.Payees {
		if !payeeRe.MatchString(p) {
			return sc, fmt.Errorf("%w: payee %q is not a 0x address", errInvalid, p)
		}
	}
	return sc, nil
}

// parseUSD reads a USD amount; empty or 0 is "no limit".
func parseUSD(v string) (money.Nanos, error) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "$"))
	if v == "" {
		return 0, nil
	}
	return money.FromUSD(v)
}

func clean(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (s *Server) handleSetScope(w http.ResponseWriter, r *http.Request) {
	var e scopeEdit
	if err := readJSON(r, &e); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sc, err := e.scope()
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	a, err := s.agents.SetScope(r.Context(), r.PathValue("ref"), sc)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "name": a.Name, "scope": viewScope(a.Scope)})
}

// handleRename renames an agent. A task's agent keeps the name the task gives
// it (the runner finds it by that name), and no other agent may take that form.
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ref, name := r.PathValue("ref"), strings.TrimSpace(body.Name)
	cur, err := s.agents.Get(r.Context(), ref)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if strings.HasPrefix(cur.Name, agentPrefix) || strings.HasPrefix(name, agentPrefix) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%w: names starting with %q belong to tasks", errInvalid, agentPrefix))
		return
	}
	a, err := s.agents.Rename(r.Context(), cur.ID, name)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "name": a.Name})
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	s.setStatus(w, r, s.agents.Kill)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.setStatus(w, r, s.agents.Resume)
}

func (s *Server) setStatus(w http.ResponseWriter, r *http.Request, f func(context.Context, string) (agents.Agent, error)) {
	a, err := f(r.Context(), r.PathValue("ref"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "name": a.Name, "status": a.Status})
}
