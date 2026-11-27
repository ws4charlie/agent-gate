package vaultsign

import (
	"errors"
	"fmt"
)

// Codes from Vault's signing API (docs/gate-credential.md, "The signing API").
const (
	CodeRequestInvalid        = "request_invalid"
	CodeCredentialMissing     = "credential_missing"
	CodeCredentialMalformed   = "credential_malformed"
	CodeClientNotAuthorized   = "client_not_authorized"
	CodeWalletNotGateGoverned = "wallet_not_gate_governed"
	CodeKidUnknown            = "kid_unknown"
	CodeSignatureInvalid      = "signature_invalid"
	CodeAudienceMismatch      = "audience_mismatch"
	CodeWalletMismatch        = "wallet_mismatch"
	CodeTTLTooLong            = "ttl_too_long"
	CodeNotYetValid           = "not_yet_valid"
	CodeExpired               = "expired"
	CodePaymentMismatch       = "payment_mismatch"
	CodeAuthorizationWindow   = "authorization_window"
	CodeFastLaneCap           = "fast_lane_cap"
	CodeReplayed              = "replayed"
	CodeNonceReused           = "nonce_reused"
	CodePayloadInvalid        = "payload_invalid"
	CodeRetryLater            = "retry_later"
	CodeSigningFailed         = "signing_failed"
)

// KnownCodes maps every code of the spec's answer table to the statuses it
// comes with. client_not_authorized has two: 401 (no valid token) and 403
// (the certificate or the role is wrong).
var KnownCodes = map[string][]int{
	CodeRequestInvalid: {400}, CodeCredentialMissing: {400}, CodeCredentialMalformed: {400},
	CodeClientNotAuthorized:   {401, 403},
	CodeWalletNotGateGoverned: {403}, CodeKidUnknown: {403}, CodeSignatureInvalid: {403},
	CodeAudienceMismatch: {403}, CodeWalletMismatch: {403}, CodeTTLTooLong: {403},
	CodeNotYetValid: {403}, CodeExpired: {403}, CodePaymentMismatch: {403},
	CodeAuthorizationWindow: {403}, CodeFastLaneCap: {403},
	CodeReplayed: {409}, CodeNonceReused: {409},
	CodePayloadInvalid: {422},
	CodeRetryLater:     {503}, CodeSigningFailed: {503},
}

// configCodes point at configuration (the two sides' settings differ, or
// Gate's identity is wrong), not at this payment. The operator log says so.
var configCodes = map[string]bool{
	CodeWalletNotGateGoverned: true, CodeAudienceMismatch: true,
	CodeKidUnknown: true, CodeClientNotAuthorized: true,
}

// Refusal is Vault's answer when it did not sign. It is never a signature.
type Refusal struct {
	Status  int    // HTTP status
	Code    string // the spec's code; "" when the body had none
	Field   string // the field the code names, when there is one
	Message string // Vault's text, for people; never parsed
}

func (r *Refusal) Error() string {
	s := fmt.Sprintf("vaultsign: vault refused (%d %s)", r.Status, r.Code)
	if r.Field != "" {
		s += " field " + r.Field
	}
	if r.Message != "" {
		s += ": " + r.Message
	}
	return s
}

// Final reports that sending the same request again cannot change the answer.
// It is true for every Refusal the caller sees: Vault stores every 4xx and
// signing_failed and returns them unchanged to a repeat; retry_later, the one
// answer that is not stored, is retried inside Sign and never reaches the
// caller as a Refusal (it becomes ErrRetryExhausted). An unknown code is
// treated as final too: the safe reading of an answer we do not understand is
// "this credential is spent". The only way forward is a new credential.
func (r *Refusal) Final() bool { return true }

// NeedsNewNonce reports that the next attempt must use a new EIP-3009 nonce,
// not only a new credential. Vault's spec: "A refused authorization keeps its
// EIP-3009 nonce at Vault". Sign cannot tell from outside which refusals were
// decided before the payload was recorded, and a new nonce costs nothing
// while a reused one is refused as nonce_reused, so it is true for every
// refusal.
func (r *Refusal) NeedsNewNonce() bool { return true }

// Config reports that the code points at configuration rather than at this
// payment: wallet_not_gate_governed, audience_mismatch, kid_unknown or
// client_not_authorized. These go to the operator log; retrying with a new
// credential will fail the same way until someone fixes the settings.
func (r *Refusal) Config() bool { return configCodes[r.Code] }

// Known reports whether the code is in the spec's table with this status.
func (r *Refusal) Known() bool {
	for _, s := range KnownCodes[r.Code] {
		if s == r.Status {
			return true
		}
	}
	return false
}

var (
	// ErrBadSignature: Vault answered 200 but the answer does not verify (echo
	// mismatch, wrong length, high s, bad v, or it does not recover to the
	// authorization's from). Gate must never forward it. Vault has signed
	// something, so the nonce is spent: the next attempt needs a new nonce and
	// a new credential, and the operator must look.
	ErrBadSignature = errors.New("vaultsign: vault's signature does not verify")
	// ErrRetryExhausted: Vault kept answering retry_later (or the connection
	// kept failing) until RetryFor ran out. Nothing is known to be decided; the
	// same credential may still be sent again before it expires.
	ErrRetryExhausted = errors.New("vaultsign: vault did not decide before the retry time ran out")
	// ErrExpired: the credential expired before it could be sent, or the
	// signature came back after the authorization's validBefore. Nothing usable.
	ErrExpired = errors.New("vaultsign: expired")
	// ErrTLS: the TLS handshake failed on a certificate (ours refused, or the
	// server's not trusted). This is configuration and is not retried.
	ErrTLS = errors.New("vaultsign: TLS certificate failure (configuration)")
	// ErrToken: the token endpoint refused our client (wrong id or secret).
	ErrToken = errors.New("vaultsign: token endpoint refused the client (configuration)")
)
