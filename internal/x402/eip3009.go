package x402

import (
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The two EIP-712 type strings. Hashing them by hand (no apitypes) keeps the
// signed bytes explainable: digest = keccak(0x1901 || domainSeparator || structHash).
var (
	domainTypeHash = crypto.Keccak256Hash([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	twaTypeHash    = crypto.Keccak256Hash([]byte("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"))
)

// Domain is the token's EIP-712 domain. A signature is valid for one token on
// one chain only, which is what pins a payment to a network.
type Domain struct {
	Name, Version string
	ChainID       *big.Int
	Contract      common.Address
}

func (d Domain) Separator() common.Hash {
	return crypto.Keccak256Hash(domainTypeHash[:],
		crypto.Keccak256([]byte(d.Name)), crypto.Keccak256([]byte(d.Version)),
		common.LeftPadBytes(d.ChainID.Bytes(), 32), common.LeftPadBytes(d.Contract.Bytes(), 32))
}

// Auth is a parsed Authorization.
type Auth struct {
	From, To                       common.Address
	Value, ValidAfter, ValidBefore *big.Int
	Nonce                          [32]byte
}

func parseUint(name, s string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || n.Sign() < 0 || n.BitLen() > 256 {
		return nil, fmt.Errorf("%s: not a uint256: %q", name, s)
	}
	return n, nil
}

func parseAddr(name, s string) (common.Address, error) {
	if !common.IsHexAddress(s) {
		return common.Address{}, fmt.Errorf("%s: not an address: %q", name, s)
	}
	return common.HexToAddress(s), nil
}

// ParseAuthorization validates the wire form.
func ParseAuthorization(a Authorization) (Auth, error) {
	var out Auth
	var err error
	if out.From, err = parseAddr("from", a.From); err != nil {
		return out, err
	}
	if out.To, err = parseAddr("to", a.To); err != nil {
		return out, err
	}
	if out.Value, err = parseUint("value", a.Value); err != nil {
		return out, err
	}
	if out.ValidAfter, err = parseUint("validAfter", a.ValidAfter); err != nil {
		return out, err
	}
	if out.ValidBefore, err = parseUint("validBefore", a.ValidBefore); err != nil {
		return out, err
	}
	nb, err := hexBytes(a.Nonce)
	if err != nil || len(nb) != 32 {
		return out, fmt.Errorf("nonce: need 32 hex bytes")
	}
	copy(out.Nonce[:], nb)
	return out, nil
}

// Wire is the inverse of ParseAuthorization.
func (a Auth) Wire() Authorization {
	return Authorization{From: a.From.Hex(), To: a.To.Hex(), Value: a.Value.String(),
		ValidAfter: a.ValidAfter.String(), ValidBefore: a.ValidBefore.String(), Nonce: "0x" + common.Bytes2Hex(a.Nonce[:])}
}

// Digest is the 32 bytes that get signed.
func Digest(d Domain, a Auth) common.Hash {
	structHash := crypto.Keccak256Hash(twaTypeHash[:],
		common.LeftPadBytes(a.From.Bytes(), 32), common.LeftPadBytes(a.To.Bytes(), 32),
		common.LeftPadBytes(a.Value.Bytes(), 32), common.LeftPadBytes(a.ValidAfter.Bytes(), 32),
		common.LeftPadBytes(a.ValidBefore.Bytes(), 32), a.Nonce[:])
	sep := d.Separator()
	return crypto.Keccak256Hash([]byte{0x19, 0x01}, sep[:], structHash[:])
}

// Sign returns r||s||v with v in {27,28}, the form the contract's ecrecover wants.
func Sign(key *ecdsa.PrivateKey, digest common.Hash) ([]byte, error) {
	sig, err := crypto.Sign(digest[:], key)
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}

// Recover returns the signer of digest.
func Recover(digest common.Hash, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, errors.New("signature must be 65 bytes")
	}
	s := append([]byte(nil), sig...)
	if s[64] >= 27 {
		s[64] -= 27
	}
	pub, err := crypto.SigToPub(digest[:], s)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub), nil
}

// PaymentNonce derives the EIP-3009 nonce from the ledger hold id (I11): one
// hold, one nonce, so a retried payment re-signs the same nonce and the token
// contract rejects the second transfer. Domain-separated so it cannot collide
// with a nonce from another use of the same hash.
func PaymentNonce(holdID string) [32]byte {
	return crypto.Keccak256Hash([]byte("agentgate/x402/nonce/v1:" + holdID))
}

func hexBytes(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}
