package x402

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Config describes one paid resource server.
type Config struct {
	Network      string // CAIP-2, e.g. eip155:84532
	Token        common.Address
	TokenName    string // EIP-712 domain name of the token
	TokenVersion string
	PayTo        common.Address
	Timeout      time.Duration // maxTimeoutSeconds: how long a signed authorization may live
	Chain        Chain
	Now          func() time.Time // injectable for tests
}

func (c *Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Config) requirements(amount *big.Int) Requirements {
	return Requirements{Scheme: SchemeExact, Network: c.Network, Amount: amount.String(), Asset: c.Token.Hex(),
		PayTo: c.PayTo.Hex(), MaxTimeoutSeconds: int(c.Timeout.Seconds()),
		Extra: Extra{AssetTransferMethod: "eip3009", Name: c.TokenName, Version: c.TokenVersion}}
}

// Verifier is the resource server's side of x402 (it plays the facilitator
// too: PLAN says the gateway's paid endpoint verifies and settles itself).
type Verifier struct {
	Config
	mu   sync.Mutex
	seen map[[32]byte]bool // nonces accepted in this process; the token contract is the real guard (I11)
}

func NewVerifier(c Config) *Verifier {
	if c.Timeout == 0 {
		c.Timeout = 60 * time.Second
	}
	return &Verifier{Config: c, seen: map[[32]byte]bool{}}
}

// Verify runs the spec's verification steps (scheme_exact_evm.md, "Phase 2")
// against requirements built from this server's own config, never from the
// payload: the client does not get to pick the price or the payee. Returns an
// x402 error reason and a message; ("", nil) means valid.
func (v *Verifier) Verify(ctx context.Context, p PaymentPayload, want Requirements) (Auth, []byte, string, error) {
	if p.X402Version != Version {
		return Auth{}, nil, ReasonVersion, fmt.Errorf("x402Version %d", p.X402Version)
	}
	if p.Accepted.Scheme != SchemeExact || want.Scheme != SchemeExact {
		return Auth{}, nil, ReasonScheme, errors.New("only the exact scheme is supported")
	}
	// The payload must restate the requirements we issued, field for field.
	if p.Accepted.Network != want.Network {
		return Auth{}, nil, ReasonNetwork, fmt.Errorf("network %q, want %q", p.Accepted.Network, want.Network)
	}
	if p.Accepted != want {
		return Auth{}, nil, ReasonRequirements, errors.New("accepted requirements differ from the ones issued")
	}
	a, err := ParseAuthorization(p.Payload.Authorization)
	if err != nil {
		return Auth{}, nil, ReasonPayload, err
	}
	sig, err := hexBytes(p.Payload.Signature)
	if err != nil || len(sig) != 65 {
		return Auth{}, nil, ReasonBadSignature, errors.New("signature must be 65 bytes")
	}
	// Step 3: authorization parameters meet the requirements.
	if a.To != v.PayTo {
		return a, sig, ReasonRecipient, fmt.Errorf("to %s, want %s", a.To, v.PayTo)
	}
	need, _ := new(big.Int).SetString(want.Amount, 10)
	if a.Value.Cmp(need) != 0 { // exact scheme: not less, not more
		return a, sig, ReasonValueMismatch, fmt.Errorf("value %s, want %s", a.Value, need)
	}
	now := big.NewInt(v.now().Unix())
	if now.Cmp(a.ValidAfter) <= 0 {
		return a, sig, ReasonValidAfter, errors.New("authorization not yet valid")
	}
	// Reject a window that closes too soon to settle, and one that stays open
	// longer than we promised (it would widen the replay window).
	if new(big.Int).Add(now, big.NewInt(2)).Cmp(a.ValidBefore) >= 0 {
		return a, sig, ReasonValidBefore, errors.New("authorization expired or about to")
	}
	if a.ValidBefore.Cmp(new(big.Int).Add(now, big.NewInt(int64(v.Timeout.Seconds())+5))) > 0 {
		return a, sig, ReasonValidBefore, errors.New("authorization window longer than maxTimeoutSeconds")
	}
	// Step 1: the signature recovers to `from`, over this token's domain on this chain.
	chainID, err := v.Chain.ChainID(ctx)
	if err != nil {
		return a, sig, ReasonSettleError, err
	}
	if v.Network != "eip155:"+chainID.String() {
		return a, sig, ReasonNetwork, fmt.Errorf("server network %q does not match chain %s", v.Network, chainID)
	}
	dom := Domain{Name: v.TokenName, Version: v.TokenVersion, ChainID: chainID, Contract: v.Token}
	signer, err := Recover(Digest(dom, a), sig)
	if err != nil || signer != a.From {
		return a, sig, ReasonBadSignature, errors.New("signature does not recover to from")
	}
	// Replay: already spent on chain, or already in flight here.
	used, err := v.Chain.AuthorizationUsed(ctx, v.Token, a.From, a.Nonce)
	if err != nil {
		return a, sig, ReasonSettleError, err
	}
	if used {
		return a, sig, ReasonReplay, errors.New("nonce already used")
	}
	// Step 2: enough balance.
	bal, err := v.Chain.BalanceOf(ctx, v.Token, a.From)
	if err != nil {
		return a, sig, ReasonSettleError, err
	}
	if bal.Cmp(a.Value) < 0 {
		return a, sig, ReasonInsufficientFunds, errors.New("payer balance too low")
	}
	return a, sig, "", nil
}

// claim marks a nonce in flight; false means another request already holds it.
func (v *Verifier) claim(n [32]byte) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen[n] {
		return false
	}
	v.seen[n] = true
	return true
}

func (v *Verifier) unclaim(n [32]byte) { v.mu.Lock(); delete(v.seen, n); v.mu.Unlock() }

// Protect wraps next so it only runs for a verified payment of `amount`.
// Spec "authorization" flow: verify -> resource -> settle -> respond. The
// resource's output is buffered and only released once settlement succeeded,
// so a failed settlement never hands out an unpaid result.
func (v *Verifier) Protect(amount *big.Int, desc string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := v.requirements(amount)
		res := Resource{URL: r.URL.Path, Description: desc, MimeType: "application/json"}
		required := func(code int, reason string, err error) {
			pr := PaymentRequired{X402Version: Version, Resource: res, Accepts: []Requirements{want}}
			if reason != "" {
				pr.Error = reason
				if err != nil {
					pr.Error = reason + ": " + err.Error()
				}
			} else {
				pr.Error = "PAYMENT-SIGNATURE header is required"
			}
			h, _ := EncodeHeader(pr)
			w.Header().Set(HeaderRequired, h)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"error":%q}`, pr.Error)
		}
		raw := r.Header.Get(HeaderSignature)
		if raw == "" {
			required(http.StatusPaymentRequired, "", nil)
			return
		}
		var p PaymentPayload
		if err := DecodeHeader(raw, &p); err != nil {
			required(http.StatusPaymentRequired, ReasonPayload, err)
			return
		}
		a, sig, reason, err := v.Verify(r.Context(), p, want)
		if reason != "" {
			required(http.StatusPaymentRequired, reason, err)
			return
		}
		if !v.claim(a.Nonce) {
			required(http.StatusPaymentRequired, ReasonReplay, errors.New("nonce already in flight"))
			return
		}
		// Keep the claim on success (a replay of a settled payment must fail
		// fast); drop it on failure so a corrected retry is possible.
		ok := false
		defer func() {
			if !ok {
				v.unclaim(a.Nonce)
			}
		}()

		rec := &bufResponse{h: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status >= 400 {
			// The resource itself failed: do not charge.
			copyResp(w, rec)
			return
		}
		tx, err := v.Chain.Settle(r.Context(), v.Token, a, sig)
		sr := SettleResponse{Network: v.Network, Payer: a.From.Hex(), Transaction: tx.Hex()}
		var pend *PendingError
		switch {
		case err == nil:
			sr.Success = true
			ok = true
		case errors.As(err, &pend):
			sr.ErrorReason = ReasonPending
			ok = true // may have moved money: never allow a second submission of this nonce
		case errors.Is(err, ErrReverted):
			sr.ErrorReason, sr.Transaction = ReasonTxState, ""
		default:
			sr.ErrorReason, sr.Transaction = ReasonSettleError, ""
		}
		hdr, _ := EncodeHeader(sr)
		if !sr.Success {
			w.Header().Set(HeaderResponse, hdr)
			w.Header().Set("Content-Type", "application/json")
			code := http.StatusPaymentRequired
			if sr.ErrorReason == ReasonPending {
				code = http.StatusAccepted
			}
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"error":%q}`, sr.ErrorReason)
			return
		}
		w.Header().Set(HeaderResponse, hdr)
		copyResp(w, rec)
	})
}

type bufResponse struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (b *bufResponse) Header() http.Header         { return b.h }
func (b *bufResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufResponse) WriteHeader(s int)           { b.status = s }

func copyResp(w http.ResponseWriter, b *bufResponse) {
	for k, v := range b.h {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Length", strconv.Itoa(b.body.Len()))
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}

// ResetSeenForTest forgets the in-memory nonce set, to prove the token
// contract (not this process) is what stops a replay (I11).
func (v *Verifier) ResetSeenForTest() { v.mu.Lock(); v.seen = map[[32]byte]bool{}; v.mu.Unlock() }
