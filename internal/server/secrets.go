package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ws4charlie/agent-gate/internal/audit"
	"github.com/ws4charlie/agent-gate/internal/mcp"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/screen"
)

// Statuses of a tool call that carried a key or password (see secretGate).
const (
	StatusSecretHeld    = "secret_held"    // confirm mode: not sent; the agent was told how to confirm
	StatusSecretBlocked = "secret_blocked" // block mode: not sent
)

// confirmArg is the argument an agent adds to send a held call anyway. The
// gateway removes it before the call goes upstream.
const confirmArg = "_agentgate_confirm"

// confirmKey signs confirm codes. It lives only in this process: a restart
// makes held codes stale, and the agent is simply told again.
var confirmKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}()

// confirmCode binds a confirmation to one agent, one tool and the exact
// arguments: change anything and the code no longer fits.
func confirmCode(agentID, tool string, args json.RawMessage) string {
	m := hmac.New(sha256.New, confirmKey)
	m.Write([]byte(agentID + "\x00" + tool + "\x00"))
	m.Write(args)
	return "c_" + hex.EncodeToString(m.Sum(nil))[:12]
}

// secretGate looks for a key or password in a tool call's arguments before
// anything is paid or sent. It reports whether it answered the call. A call
// it lets through has had any confirm argument removed (p and body updated).
func (c *call) secretGate(tool, task string, req mcp.Request, p *mcp.CallParams, body *[]byte) bool {
	args, code, hadCode := screen.StripKey(p.Arguments, confirmArg)
	if hadCode {
		params, _ := json.Marshal(mcp.CallParams{Name: p.Name, Arguments: args})
		req.Params = params
		b, err := json.Marshal(req)
		if err != nil {
			c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
			return true
		}
		p.Arguments, *body = args, b
	}
	sec, hit := screen.FindSecret(args)
	if !hit {
		return false
	}
	snap, err := c.s.Agents.Snapshot(c.ctx, c.agent.ID, task)
	if err != nil {
		c.s.Log.Error("policy snapshot", "request_id", c.ev.RequestID, "err", err)
		c.fail(http.StatusInternalServerError, audit.StatusInternal, "internal_error", "internal error", nil)
		return true
	}
	mode := snap.Scope.SecretMode()
	want := confirmCode(c.agent.ID, tool, args)
	if mode == policy.SecretsConfirm && hadCode && hmac.Equal([]byte(code), []byte(want)) {
		c.s.Log.Warn("secret sent on confirmation", "request_id", c.ev.RequestID, "tool", tool, "what", sec.What, "where", sec.Where)
		c.ev.Err = "secret_confirmed: " + sec.What
		return false
	}

	risk := map[string]any{"rule": "secret_in_request", "what": sec.What, "where": sec.Where, "seen": sec.Seen, "mode": mode}
	c.ev.Err = "secret_in_request: " + sec.What + " in " + sec.Where
	c.s.Log.Warn("secret held", "request_id", c.ev.RequestID, "run", c.runID, "tool", tool, "what", sec.What, "where", sec.Where, "mode", mode)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	if mode == policy.SecretsBlock {
		risk["action"] = "blocked"
		_, _ = c.w.Write(mcp.ErrorResponse(c.rpcID, mcp.Error{Code: mcp.CodeScreened,
			Message: fmt.Sprintf("not sent: %s contains what looks like a %s", sec.Where, readable(sec.What)),
			Data:    map[string]any{"agentgate_risk": risk, "request_id": c.ev.RequestID}}))
		c.finish(StatusSecretBlocked)
		return true
	}
	risk["action"] = "held"
	risk["to_send_anyway"] = map[string]string{"add_argument": confirmArg, "value": want}
	text := fmt.Sprintf("AgentGate held this tool call: %s contains what looks like a %s (%s). Nothing was sent. "+
		"Ask the user before sending it. To send it anyway, repeat the same call with the argument %q set to %q; "+
		"otherwise remove the secret and try again.", sec.Where, readable(sec.What), sec.Seen, confirmArg, want)
	res, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.rpcID, "result": map[string]any{
		"content":           []map[string]string{{"type": "text", "text": text}},
		"structuredContent": map[string]any{"agentgate_risk": risk},
		"isError":           true,
	}})
	_, _ = c.w.Write(res)
	c.finish(StatusSecretHeld)
	return true
}

func readable(what string) string { return strings.ReplaceAll(what, "_", " ") }
