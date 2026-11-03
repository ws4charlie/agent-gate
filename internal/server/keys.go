package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const keyPrefix = "ag_"

// CreateKey makes a gateway credential for an account and returns the secret.
// The secret is 256 random bits; only its SHA-256 is stored (I9), so a database
// leak does not yield usable credentials and the secret cannot be shown again.
func CreateKey(ctx context.Context, pool *pgxpool.Pool, accountID, label string) (secret, id string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	secret = keyPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	h := sha256.Sum256([]byte(secret))
	err = pool.QueryRow(ctx, `INSERT INTO api_keys (account_id, key_hash, label) VALUES ($1,$2,$3) RETURNING id`,
		accountID, h[:], label).Scan(&id)
	if err != nil {
		return "", "", fmt.Errorf("create key: %w", err)
	}
	return secret, id, nil
}

var errUnauthorized = errors.New("unauthorized")

// authenticate resolves the account behind a request. Claude Code sends
// Authorization: Bearer or x-api-key depending on setup; both are accepted.
//
// The lookup is a plain indexed equality on SHA-256(secret), not a
// constant-time compare: the secret has 256 bits of entropy and the database
// only ever sees its hash, so an attacker cannot use timing to walk toward a
// valid hash without already knowing a preimage. (A constant-time compare would
// matter for comparing the secrets themselves, which we never do.)
func authenticate(ctx context.Context, pool *pgxpool.Pool, r *http.Request) (accountID string, err error) {
	tok := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	if tok == "" {
		if a, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			tok = strings.TrimSpace(a)
		}
	}
	if tok == "" {
		return "", errUnauthorized
	}
	h := sha256.Sum256([]byte(tok))
	err = pool.QueryRow(ctx, `SELECT account_id FROM api_keys WHERE key_hash=$1 AND revoked_at IS NULL`, h[:]).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errUnauthorized
	}
	return accountID, err
}
