// Package x402 is a hand-written implementation of the x402 v2 "exact" scheme
// on EVM with EIP-3009 (transferWithAuthorization), HTTP transport. No SDK:
// every byte is visible. Spec: github.com/coinbase/x402, specs/
// x402-specification-v2.md, specs/schemes/exact/scheme_exact_evm.md,
// specs/transports-v2/http.md.
package x402

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// HTTP transport header names. Each carries base64(JSON).
const (
	HeaderRequired  = "PAYMENT-REQUIRED"  // server -> client, on the 402
	HeaderSignature = "PAYMENT-SIGNATURE" // client -> server, on the retry
	HeaderResponse  = "PAYMENT-RESPONSE"  // server -> client, on the 200
)

const (
	Version     = 2
	SchemeExact = "exact"

	// LocalChainID is go-ethereum's simulated backend; the only chain besides
	// Base Sepolia that the wallet will sign for (I10 hardening).
	LocalChainID       = 1337
	BaseSepoliaChainID = 84532
	NetworkLocal       = "eip155:1337"
	NetworkBaseSepolia = "eip155:84532"

	// USDC on Base Sepolia. Source: Circle, "USDC Contract Addresses",
	// https://developers.circle.com/stablecoins/usdc-contract-addresses
	USDCBaseSepolia = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
)

// Resource describes what is being paid for.
type Resource struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// Extra carries the EIP-712 domain name/version of the token (required by the
// eip3009 transfer method) so the client can rebuild the signed message.
type Extra struct {
	AssetTransferMethod string `json:"assetTransferMethod,omitempty"`
	Name                string `json:"name"`
	Version             string `json:"version"`
}

// Requirements is one entry of PaymentRequired.accepts.
type Requirements struct {
	Scheme            string `json:"scheme"`
	Network           string `json:"network"`
	Amount            string `json:"amount"` // atomic units; USDC has 6 decimals
	Asset             string `json:"asset"`  // token contract
	PayTo             string `json:"payTo"`
	MaxTimeoutSeconds int    `json:"maxTimeoutSeconds"`
	Extra             Extra  `json:"extra"`
}

// PaymentRequired is the body of the 402 (also base64 in PAYMENT-REQUIRED).
type PaymentRequired struct {
	X402Version int            `json:"x402Version"`
	Error       string         `json:"error,omitempty"`
	Resource    Resource       `json:"resource"`
	Accepts     []Requirements `json:"accepts"`
}

// Authorization is the EIP-3009 message, all numbers as decimal strings.
type Authorization struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
	ValidAfter  string `json:"validAfter"`
	ValidBefore string `json:"validBefore"`
	Nonce       string `json:"nonce"` // 0x + 32 bytes
}

type Payload struct {
	Signature     string        `json:"signature"` // 0x + 65 bytes (r, s, v)
	Authorization Authorization `json:"authorization"`
}

// PaymentPayload is what the client sends in PAYMENT-SIGNATURE.
type PaymentPayload struct {
	X402Version int          `json:"x402Version"`
	Resource    *Resource    `json:"resource,omitempty"`
	Accepted    Requirements `json:"accepted"`
	Payload     Payload      `json:"payload"`
}

// SettleResponse is sent in PAYMENT-RESPONSE.
type SettleResponse struct {
	Success     bool   `json:"success"`
	ErrorReason string `json:"errorReason,omitempty"`
	Payer       string `json:"payer,omitempty"`
	Transaction string `json:"transaction"`
	Network     string `json:"network"`
}

// Error reasons from spec section 9 that this implementation returns.
const (
	ReasonInsufficientFunds = "insufficient_funds"
	ReasonValidAfter        = "invalid_exact_evm_payload_authorization_valid_after"
	ReasonValidBefore       = "invalid_exact_evm_payload_authorization_valid_before"
	ReasonValueMismatch     = "invalid_exact_evm_payload_authorization_value_mismatch"
	ReasonBadSignature      = "invalid_exact_evm_payload_signature"
	ReasonRecipient         = "invalid_exact_evm_payload_recipient_mismatch"
	ReasonNetwork           = "invalid_network"
	ReasonPayload           = "invalid_payload"
	ReasonRequirements      = "invalid_payment_requirements"
	ReasonScheme            = "invalid_scheme"
	ReasonVersion           = "invalid_x402_version"
	ReasonTxState           = "invalid_transaction_state"
	ReasonSettleError       = "unexpected_settle_error"
	ReasonPending           = "settlement_pending"
	ReasonReplay            = "invalid_exact_evm_payload_nonce_used" // not in the spec list: nonce already spent
)

// EncodeHeader is base64(JSON(v)).
func EncodeHeader(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// DecodeHeader reverses EncodeHeader into v.
func DecodeHeader(s string, v any) error {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("x402: header is not base64: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("x402: header is not JSON: %w", err)
	}
	return nil
}
