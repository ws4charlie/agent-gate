package x402

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ws4charlie/agent-gate/internal/ledger"
)

// Provider is the name the gateway stores in holds.provider for a payment hold.
const Provider = "x402"

// AuthRef is what the gateway records on a payment hold BEFORE the wallet signs
// (as holds.upstream_id, opaque to the ledger). Once a signature exists the
// chain decides whether money moved, and this is the handle for asking it: the
// payer, the nonce and the token identify the authorization; validBefore says
// when it can no longer execute.
type AuthRef struct {
	From        common.Address `json:"from"`
	Nonce       common.Hash    `json:"nonce"`
	Value       string         `json:"value"` // atomic units
	ValidBefore int64          `json:"validBefore"`
	Token       common.Address `json:"token"`
}

func NewAuthRef(a Auth, token common.Address) AuthRef {
	return AuthRef{From: a.From, Nonce: a.Nonce, Value: a.Value.String(), ValidBefore: a.ValidBefore.Int64(), Token: token}
}

func (r AuthRef) Encode() string { b, _ := json.Marshal(r); return string(b) }

func DecodeAuthRef(s string) (AuthRef, error) {
	var r AuthRef
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return r, fmt.Errorf("x402: bad authorization ref: %w", err)
	}
	return r, nil
}

// AuthReader is the one chain question the reconciler asks.
type AuthReader interface {
	AuthorizationUsed(ctx context.Context, token, who common.Address, nonce [32]byte) (bool, error)
}

// ErrWindowOpen: the authorization has not executed, but it still could.
var ErrWindowOpen = errors.New("x402: authorization unused but its window is still open")

// AuthLookup is the ledger.UsageLookup for payment holds (I6 for money). After
// signing, the chain is the source of truth:
//
//	used on chain            -> found: charge the authorized value
//	unused, window still open -> error: the hold stays active, the next pass asks again
//	unused, window closed     -> not found: it can never execute, so release
type AuthLookup struct {
	Chain AuthReader
	Now   func() time.Time
	// Margin is added to validBefore before declaring the window closed, to
	// cover a transaction already in flight and clock skew between us and the chain.
	Margin time.Duration
}

var _ ledger.UsageLookup = (*AuthLookup)(nil)

// DefaultAuthMargin is how long after validBefore the chain is still asked to
// show a late settlement. It is a cautious rule of thumb: a release is safe only
// once a transaction sent at the last moment could have landed.
const DefaultAuthMargin = 2 * time.Minute

// HoldTTL is how long a payment's hold must last: the outcome is known once the
// signature has expired and the margin has passed, and not before.
func HoldTTL(r Requirements) time.Duration {
	return time.Duration(r.MaxTimeoutSeconds)*time.Second + DefaultAuthMargin
}

func (l *AuthLookup) Lookup(ctx context.Context, _, ref string) (ledger.Usage, bool, error) {
	r, err := DecodeAuthRef(ref)
	if err != nil {
		return ledger.Usage{}, false, err // never guess: an unreadable ref stays active and is counted as an error
	}
	used, err := l.Chain.AuthorizationUsed(ctx, r.Token, r.From, r.Nonce)
	if err != nil {
		return ledger.Usage{}, false, err
	}
	if used {
		v, ok := new(big.Int).SetString(r.Value, 10)
		if !ok {
			return ledger.Usage{}, false, errors.New("x402: bad value in ref")
		}
		n, ok := NanosFromAtomic(v)
		if !ok {
			return ledger.Usage{}, false, errors.New("x402: value out of range")
		}
		return ledger.Usage{Cost: n, Source: "chain"}, true, nil
	}
	now, margin := time.Now(), l.Margin
	if l.Now != nil {
		now = l.Now()
	}
	if margin == 0 {
		margin = DefaultAuthMargin
	}
	if now.Unix() < r.ValidBefore+int64(margin.Seconds()) {
		return ledger.Usage{}, false, ErrWindowOpen
	}
	return ledger.Usage{}, false, nil
}

// RemoteChain answers AuthorizationUsed over HTTP from a paid-tool process.
// Used with the simulated chain, which lives inside that process; against
// Base Sepolia the gateway talks to the RPC node directly instead.
type RemoteChain struct {
	BaseURL string // e.g. http://127.0.0.1:9402
	HTTP    *http.Client
}

func (c *RemoteChain) AuthorizationUsed(ctx context.Context, token, who common.Address, nonce [32]byte) (bool, error) {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	q := url.Values{"token": {token.Hex()}, "from": {who.Hex()}, "nonce": {common.Hash(nonce).Hex()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+AuthStatePath+"?"+q.Encode(), nil)
	if err != nil {
		return false, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("x402: authorization state: status %d", resp.StatusCode)
	}
	var out struct {
		Used bool `json:"used"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Used, nil
}

// AuthStatePath is served by the paid tool (read-only chain data).
const AuthStatePath = "/chain/authorization-state"

// AuthStateHandler serves AuthStatePath from a Chain.
func AuthStateHandler(c Chain) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if !common.IsHexAddress(q.Get("token")) || !common.IsHexAddress(q.Get("from")) || len(q.Get("nonce")) != 66 {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		used, err := c.AuthorizationUsed(r.Context(), common.HexToAddress(q.Get("token")), common.HexToAddress(q.Get("from")),
			common.HexToHash(q.Get("nonce")))
		if err != nil {
			http.Error(w, "chain error", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"used": used})
	})
}
