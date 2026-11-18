package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/policy"
)

// CreateKey makes a gateway credential for an account: an agent with the
// default scope (any model, any tool, no limits, no payees), which is what a
// T2 API key always was. It returns the secret, shown once; only its SHA-256 is
// stored (I9). `agentgate agent create` is the way to make a scoped one.
func CreateKey(ctx context.Context, pool *pgxpool.Pool, accountID, label string) (secret, id string, err error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	name := "key-" + hex.EncodeToString(b[:])
	if label != "" {
		name = label + "-" + hex.EncodeToString(b[:])
	}
	secret, a, err := agents.NewStore(pool).Create(ctx, accountID, name, policy.DefaultScope())
	if err != nil && label != "" { // a label that is not a valid name: fall back to the plain one
		secret, a, err = agents.NewStore(pool).Create(ctx, accountID, "key-"+hex.EncodeToString(b[:]), policy.DefaultScope())
	}
	return secret, a.ID, err
}

var errUnauthorized = errors.New("unauthorized")

// errKeyExpired is an errUnauthorized, so every caller that refuses with 401
// keeps doing so; authFailure gives it its own code.
var errKeyExpired = fmt.Errorf("%w: key expired", errUnauthorized)

// authFailure is the error code and message for a refused credential. An
// expired key says so; nothing else tells whether a key ever existed.
func authFailure(err error) (code, msg string) {
	if errors.Is(err, errKeyExpired) {
		return "key_expired", "API key expired"
	}
	return "unauthorized", "missing or invalid API key"
}

// keyWarnWindow is how long before expiry responses start carrying the
// X-AgentGate-Key-Expires header.
const keyWarnWindow = 7 * 24 * time.Hour

// keyWarned remembers when each agent was last warned about, so the log gets
// one line per agent per hour.
var keyWarned sync.Map // agent id -> time.Time

// noteKeyExpiry tells the operator that the key just used is about to stop
// working: a header on the response, and one WARN per agent per hour. It does
// nothing for a key that does not expire soon. The key is never logged.
func (s *Server) noteKeyExpiry(w http.ResponseWriter, a agents.Agent) {
	exp := a.UsedKeyExpiresAt
	if exp == nil || (!a.UsedPrevKey && time.Until(*exp) > keyWarnWindow) {
		return
	}
	w.Header().Set("X-AgentGate-Key-Expires", exp.UTC().Format(time.RFC3339))
	now := time.Now()
	if last, ok := keyWarned.Load(a.ID); ok && now.Sub(last.(time.Time)) < time.Hour {
		return
	}
	keyWarned.Store(a.ID, now)
	s.Log.Warn("api key expires soon", "agent", a.ID, "expires_at", exp.UTC().Format(time.RFC3339), "previous_key", a.UsedPrevKey)
}

// bearer pulls the credential out of the request. Claude Code sends
// Authorization: Bearer or x-api-key depending on setup; both are accepted.
func bearer(r *http.Request) string {
	tok := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	if tok == "" {
		if a, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			tok = strings.TrimSpace(a)
		}
	}
	return tok
}

// authenticate resolves the agent behind a request. A killed or revoked agent
// authenticates and is then refused by policy rule 1, so the refusal is
// audited with its rule id.
//
// A request that came in over the mutual-TLS listener carries a verified
// SPIFFE ID; that identity decides, and any API key on the request is ignored.
// Every other request uses the API key.
func (s *Server) authenticate(ctx context.Context, r *http.Request) (agents.Agent, error) {
	var a agents.Agent
	var err error
	if id, ok := SPIFFEIDFromContext(r.Context()); ok {
		a, err = s.Agents.AuthenticateSPIFFE(ctx, id.String())
	} else {
		a, err = s.Agents.Authenticate(ctx, bearer(r))
	}
	if errors.Is(err, agents.ErrKeyExpired) {
		return agents.Agent{}, errKeyExpired
	}
	if errors.Is(err, agents.ErrUnauthorized) {
		return agents.Agent{}, errUnauthorized
	}
	return a, err
}
