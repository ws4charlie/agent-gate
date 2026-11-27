package vaultsign_test

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

func bi(s string) *big.Int { n, _ := new(big.Int).SetString(s, 10); return n }

func TestPayloadHashEqualsX402Digest(t *testing.T) {
	cases := []struct {
		name string
		dom  x402.Domain
		auth x402.Auth
	}{
		{"usdc base sepolia", x402.Domain{Name: "USDC", Version: "2", ChainID: big.NewInt(84532), Contract: common.HexToAddress(x402.USDCBaseSepolia)},
			x402.Auth{From: common.HexToAddress("0x1111111111111111111111111111111111111111"), To: common.HexToAddress("0x2222222222222222222222222222222222222222"),
				Value: big.NewInt(1500), ValidAfter: big.NewInt(1700000000), ValidBefore: big.NewInt(1700000600), Nonce: x402.PaymentNonce("hold-1")}},
		{"local chain, zero values", x402.Domain{Name: "Mock Token", Version: "1", ChainID: big.NewInt(1337), Contract: common.HexToAddress("0xAbCdEf0123456789aBcDeF0123456789abCDef01")},
			x402.Auth{From: common.HexToAddress("0xFFfFfFffFFfffFFfFFfFFFFFffFFFffffFfFFFfF"), To: common.HexToAddress("0x0000000000000000000000000000000000000001"),
				Value: big.NewInt(0), ValidAfter: big.NewInt(0), ValidBefore: big.NewInt(1), Nonce: [32]byte{}}},
		{"max uint256 value, unicode name", x402.Domain{Name: "Ünïcode \"Coin\"", Version: "10", ChainID: bi("18446744073709551617"), Contract: common.HexToAddress("0x036cbd53842c5426634e7929541ec2318f3dcf7e")},
			x402.Auth{From: common.HexToAddress("0x3333333333333333333333333333333333333333"), To: common.HexToAddress("0x4444444444444444444444444444444444444444"),
				Value: new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)), ValidAfter: big.NewInt(5), ValidBefore: bi("99999999999"),
				Nonce: [32]byte{0xff, 1, 2, 3, 31: 0xee}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := vaultsign.Payload(c.dom, c.auth)
			var td apitypes.TypedData
			if err := json.Unmarshal(p, &td); err != nil {
				t.Fatal(err)
			}
			h, _, err := apitypes.TypedDataAndHash(td)
			if err != nil {
				t.Fatal(err)
			}
			if want := x402.Digest(c.dom, c.auth); !bytes.Equal(h, want[:]) {
				t.Fatalf("typed data hash %x, x402 digest %x", h, want)
			}
		})
	}
}

// The spec fixes the members, the field order of both types and the JSON
// type of every value (chainId a number, every message value a string).
func TestPayloadGolden(t *testing.T) {
	dom := x402.Domain{Name: "USDC", Version: "2", ChainID: big.NewInt(84532), Contract: common.HexToAddress(x402.USDCBaseSepolia)}
	a := x402.Auth{From: common.HexToAddress("0x1111111111111111111111111111111111111111"), To: common.HexToAddress("0xABCDEFabcdef0000000000000000000000000002"),
		Value: big.NewInt(10000), ValidAfter: big.NewInt(1764100000), ValidBefore: big.NewInt(1764100300), Nonce: [32]byte{0x01, 31: 0x02}}
	want := `{"types":{"EIP712Domain":[{"name":"name","type":"string"},{"name":"version","type":"string"},{"name":"chainId","type":"uint256"},{"name":"verifyingContract","type":"address"}],` +
		`"TransferWithAuthorization":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"value","type":"uint256"},{"name":"validAfter","type":"uint256"},{"name":"validBefore","type":"uint256"},{"name":"nonce","type":"bytes32"}]},` +
		`"primaryType":"TransferWithAuthorization",` +
		`"domain":{"name":"USDC","version":"2","chainId":84532,"verifyingContract":"0x036cbd53842c5426634e7929541ec2318f3dcf7e"},` +
		`"message":{"from":"0x1111111111111111111111111111111111111111","to":"0xabcdefabcdef0000000000000000000000000002","value":"10000","validAfter":"1764100000","validBefore":"1764100300",` +
		`"nonce":"0x0100000000000000000000000000000000000000000000000000000000000002"}}`
	if got := string(vaultsign.Payload(dom, a)); got != want {
		t.Fatalf("payload\n got %s\nwant %s", got, want)
	}
}
