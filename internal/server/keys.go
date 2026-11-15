package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

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
	if errors.Is(err, agents.ErrUnauthorized) {
		return agents.Agent{}, errUnauthorized
	}
	return a, err
}
