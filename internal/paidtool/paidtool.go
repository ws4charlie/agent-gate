// Package paidtool is a tiny paid HTTP tool behind the x402 middleware. It
// exists so the payments slice has a real counterparty to pay.
package paidtool

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// Prices in USDC atomic units (6 decimals).
var (
	EchoPrice    = big.NewInt(10_000)  // $0.01
	PremiumPrice = big.NewInt(500_000) // $0.50
)

// Handler serves POST /echo ($0.01) and POST /premium ($0.50), both paid.
func Handler(v *x402.Verifier) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /echo", v.Protect(EchoPrice, "Echo the input back", tool("echo", strings.ToUpper)))
	mux.Handle("POST /premium", v.Protect(PremiumPrice, "Premium analysis", tool("premium", func(s string) string { return "analysis of: " + s })))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

func tool(name string, f func(string) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"tool": name, "output": f(in.Text)})
	})
}
