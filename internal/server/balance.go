package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ws4charlie/agent-gate/internal/provider"
)

// handleBalance: GET /v1/balance. Read-only. It tells the authenticated agent
// what its account holds and what it has spent today, so an agent can plan
// inside its budget instead of finding the limit by being refused. It reveals
// nothing about other agents or accounts: the credential names both.
//
// Amounts are given as USD strings (like every other response) and as Nanos.
// daily_budget is omitted when the agent has none; remaining_today is then
// omitted too.
func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	agent, err := s.authenticate(r.Context(), r)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			writeErr(w, provider.FormatOpenAI, http.StatusUnauthorized, "unauthorized", "missing or invalid API key", nil)
		} else {
			s.Log.Error("auth lookup", "err", err)
			writeErr(w, provider.FormatOpenAI, http.StatusInternalServerError, "internal_error", "internal error", nil)
		}
		return
	}
	bal, err := s.Ledger.Balance(r.Context(), agent.AccountID)
	if err != nil {
		s.Log.Error("balance", "err", err)
		writeErr(w, provider.FormatOpenAI, http.StatusInternalServerError, "internal_error", "internal error", nil)
		return
	}
	sn, err := s.Agents.Snapshot(r.Context(), agent.ID, "")
	if err != nil {
		s.Log.Error("balance snapshot", "err", err)
		writeErr(w, provider.FormatOpenAI, http.StatusInternalServerError, "internal_error", "internal error", nil)
		return
	}
	out := map[string]any{
		"account_id": agent.AccountID, "agent_id": agent.ID, "agent": agent.Name,
		"balance": bal.Balance.String(), "balance_nanos": int64(bal.Balance),
		"held": bal.Held.String(), "held_nanos": int64(bal.Held),
		"available": (bal.Balance - bal.Held).String(), "available_nanos": int64(bal.Balance - bal.Held),
		"daily_spent": sn.DailySpent.String(), "daily_spent_nanos": int64(sn.DailySpent),
	}
	if b := sn.Scope.DailyBudget; b > 0 {
		rem := b - sn.DailySpent
		if rem < 0 {
			rem = 0
		}
		out["daily_budget"], out["daily_budget_nanos"] = b.String(), int64(b)
		out["remaining_today"], out["remaining_today_nanos"] = rem.String(), int64(rem)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
