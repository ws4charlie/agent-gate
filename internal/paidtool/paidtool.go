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
	// The same two tools as MCP servers (JSON-RPC over POST): an agent calls
	// tools/call through the gateway, the tool answers 402, the gateway pays.
	mux.Handle("POST /mcp/echo", v.Protect(EchoPrice, "Echo the input back (MCP)", mcpTool(strings.ToUpper)))
	mux.Handle("POST /mcp/premium", v.Protect(PremiumPrice, "Premium analysis (MCP)", mcpTool(func(s string) string { return "analysis of: " + s })))
	mux.Handle("GET "+x402.AuthStatePath, x402.AuthStateHandler(v.Chain)) // read-only: lets the gateway's reconciler ask the chain
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

// mcpTool answers a tools/call with one text result built from arguments.text.
// Anything else gets a JSON-RPC "method not found". It is a demo counterparty,
// not an MCP server: no tools/list, no sessions.
func mcpTool(f func(string) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		if in.Method != "tools/call" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": map[string]any{
			"content": []map[string]string{{"type": "text", "text": f(in.Params.Arguments.Text)}}}})
	})
}
