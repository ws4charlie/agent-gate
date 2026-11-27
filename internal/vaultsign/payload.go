package vaultsign

import (
	"encoding/json"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// The payload of kind evm_eip3009 is EIP-712 typed data in the form of
// eth_signTypedData_v4. Vault's spec (docs/gate-credential.md, "Payload of
// evm_eip3009") fixes every member, the field order of both types and the JSON
// type of every value, so the payload is built from structs (fixed member
// order), never from maps.

type typeField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type payloadTypes struct {
	EIP712Domain              []typeField `json:"EIP712Domain"`
	TransferWithAuthorization []typeField `json:"TransferWithAuthorization"`
}

type payloadDomain struct {
	Name              string      `json:"name"`
	Version           string      `json:"version"`
	ChainID           json.Number `json:"chainId"` // a JSON number, never a string
	VerifyingContract string      `json:"verifyingContract"`
}

type payloadMessage struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
	ValidAfter  string `json:"validAfter"`
	ValidBefore string `json:"validBefore"`
	Nonce       string `json:"nonce"`
}

type payload struct {
	Types       payloadTypes   `json:"types"`
	PrimaryType string         `json:"primaryType"`
	Domain      payloadDomain  `json:"domain"`
	Message     payloadMessage `json:"message"`
}

// The two type lists, in the order of the spec and of x402's type strings
// (eip3009.go). The test checks the EIP-712 hash of the result against
// x402.Digest, so a reordered field fails there too.
var (
	domainFields = []typeField{
		{"name", "string"}, {"version", "string"}, {"chainId", "uint256"}, {"verifyingContract", "address"},
	}
	twaFields = []typeField{
		{"from", "address"}, {"to", "address"}, {"value", "uint256"},
		{"validAfter", "uint256"}, {"validBefore", "uint256"}, {"nonce", "bytes32"},
	}
)

// lowerAddr writes an address as 0x and 40 lower-case hex digits: one spelling,
// which the spec accepts and which is what the credential's payee uses.
func lowerAddr(a common.Address) string { return strings.ToLower(a.Hex()) }

// Payload builds the typed data Vault signs for one authorization. Vault
// rebuilds the typed data from these values and signs that, so the digest it
// signs is x402.Digest(dom, a) exactly when this is right.
func Payload(dom x402.Domain, a x402.Auth) json.RawMessage {
	p := payload{
		Types:       payloadTypes{EIP712Domain: domainFields, TransferWithAuthorization: twaFields},
		PrimaryType: "TransferWithAuthorization",
		Domain: payloadDomain{
			Name: dom.Name, Version: dom.Version,
			ChainID:           json.Number(dom.ChainID.String()),
			VerifyingContract: lowerAddr(dom.Contract),
		},
		Message: payloadMessage{
			From: lowerAddr(a.From), To: lowerAddr(a.To),
			Value: a.Value.String(), ValidAfter: a.ValidAfter.String(), ValidBefore: a.ValidBefore.String(),
			Nonce: "0x" + common.Bytes2Hex(a.Nonce[:]),
		},
	}
	b, err := json.Marshal(p)
	if err != nil { // only strings and a decimal json.Number: cannot fail
		panic("vaultsign: payload: " + err.Error())
	}
	return b
}
