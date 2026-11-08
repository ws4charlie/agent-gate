package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// The wallet tile shows the USDC the gateway's wallet holds on chain, read
// with one eth_call to balanceOf. Only the address is needed: the console
// never loads the private key.

type walletView struct {
	Address string    `json:"address"`
	USDC    string    `json:"usdc,omitempty"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

var walletCache struct {
	sync.Mutex
	v *walletView
}

const walletTTL = 30 * time.Second

func (s *Server) wallet(ctx context.Context) *walletView {
	walletCache.Lock()
	defer walletCache.Unlock()
	if v := walletCache.v; v != nil && v.Address == s.cfg.WalletAddress && time.Since(v.At) < walletTTL {
		return v
	}
	v := &walletView{Address: s.cfg.WalletAddress, At: time.Now().UTC()}
	if s.cfg.RPCURL == "" || s.cfg.USDC == "" {
		v.Error = "no RPC URL or USDC contract configured"
	} else if n, err := s.usdcBalance(ctx); err != nil {
		v.Error = err.Error()
	} else {
		v.USDC = n
	}
	walletCache.v = v
	return v
}

func (s *Server) usdcBalance(ctx context.Context) (string, error) {
	addr := strings.ToLower(strings.TrimPrefix(s.cfg.WalletAddress, "0x"))
	if !payeeRe.MatchString("0x" + addr) {
		return "", fmt.Errorf("wallet address is not a 0x address")
	}
	data := "0x70a08231" + strings.Repeat("0", 24) + addr // balanceOf(address)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []any{map[string]string{"to": s.cfg.USDC, "data": data}, "latest"}})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.RPCURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("rpc: %s", redactURL(err.Error(), s.cfg.RPCURL))
	}
	defer resp.Body.Close()
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("rpc: bad answer (HTTP %d)", resp.StatusCode)
	}
	if out.Error != nil {
		return "", fmt.Errorf("rpc: %s", out.Error.Message)
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(out.Result, "0x"), 16)
	if !ok {
		return "", fmt.Errorf("rpc: bad balance %q", out.Result)
	}
	// USDC has 6 decimals; a nano is 1e-9 USD.
	nanos := new(big.Int).Mul(n, big.NewInt(1000))
	if !nanos.IsInt64() {
		return "", fmt.Errorf("balance too large")
	}
	return money.Nanos(nanos.Int64()).String(), nil
}

// redactURL keeps an RPC URL, which often carries an API key, out of errors.
func redactURL(msg, u string) string {
	if u == "" {
		return msg
	}
	return strings.ReplaceAll(msg, u, "<rpc url>")
}
