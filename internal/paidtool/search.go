package paidtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// SearchPrice is one search in USDC atomic units ($0.01).
var SearchPrice = big.NewInt(10_000)

const (
	defaultSearchURL = "https://api.firecrawl.dev/v2/search"
	defaultResults   = 5
	maxResults       = 10
	maxSearchBody    = 1 << 16
	maxUpstreamBody  = 4 << 20
)

// Searcher runs a web search against Firecrawl. It is the real service behind
// the paid search tool; x402 is the layer this repository adds in front of it.
type Searcher struct {
	Key    string
	URL    string // default: Firecrawl's search endpoint
	Client *http.Client
}

// Search returns the top results as plain text, one block per result.
func (s *Searcher) Search(ctx context.Context, query string, limit int) (string, error) {
	if limit <= 0 {
		limit = defaultResults
	}
	limit = min(limit, maxResults)
	body, err := json.Marshal(map[string]any{"query": query, "limit": limit})
	if err != nil {
		return "", err
	}
	url := s.URL
	if url == "" {
		url = defaultSearchURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.Key)
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("search upstream unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search upstream returned %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Web []struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				URL         string `json:"url"`
			} `json:"web"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&out); err != nil {
		return "", errors.New("search upstream sent unreadable JSON")
	}
	blocks := make([]string, 0, len(out.Data.Web))
	for _, r := range out.Data.Web {
		blocks = append(blocks, r.Title+"\n"+r.URL+"\n"+r.Description)
	}
	if len(blocks) == 0 {
		return "no results", nil
	}
	return strings.Join(blocks, "\n\n"), nil
}

// SearchMCP is a small MCP server with one paid tool. Discovery is free, so a
// client can start up and list tools; only tools/call goes through x402.
func SearchMCP(v *x402.Verifier, s *Searcher) http.Handler {
	return searchMCP(func(next http.Handler) http.Handler {
		return v.Protect(SearchPrice, "Web search", next)
	}, s)
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		ProtocolVersion string `json:"protocolVersion"`
		Name            string `json:"name"`
		Arguments       struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		} `json:"arguments"`
	} `json:"params"`
}

func searchMCP(protect func(http.Handler) http.Handler, s *Searcher) http.Handler {
	paid := protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		text, err := s.Search(r.Context(), in.Params.Arguments.Query, in.Params.Arguments.Limit)
		if err != nil {
			reply(w, in.ID, map[string]any{"isError": true, "content": textContent(err.Error())})
			return
		}
		reply(w, in.ID, map[string]any{"content": textContent(text)})
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSearchBody))
		if err != nil {
			http.Error(w, "unreadable request", http.StatusBadRequest)
			return
		}
		var in rpcRequest
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, "not JSON", http.StatusBadRequest)
			return
		}
		switch in.Method {
		case "initialize":
			reply(w, in.ID, map[string]any{
				"protocolVersion": in.Params.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "agentgate-search", "version": "0.1.0"},
			})
		case "ping":
			reply(w, in.ID, map[string]any{})
		case "tools/list":
			reply(w, in.ID, map[string]any{"tools": []any{searchTool()}})
		case "tools/call":
			if in.Params.Name != "search" || strings.TrimSpace(in.Params.Arguments.Query) == "" {
				replyError(w, in.ID, -32602, "search needs a query")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			paid.ServeHTTP(w, r)
		default:
			if len(in.ID) == 0 { // a notification needs no answer
				w.WriteHeader(http.StatusAccepted)
				return
			}
			replyError(w, in.ID, -32601, "method not found")
		}
	})
}

func searchTool() map[string]any {
	return map[string]any{
		"name":        "search",
		"description": "Search the web. Returns the title, URL and a summary of each result.",
		"inputSchema": map[string]any{
			"type":     "object",
			"required": []string{"query"},
			"properties": map[string]any{
				"query": map[string]string{"type": "string", "description": "What to search for"},
				"limit": map[string]any{"type": "integer", "description": "How many results", "minimum": 1, "maximum": maxResults},
			},
		},
	}
}

func textContent(s string) []map[string]string {
	return []map[string]string{{"type": "text", "text": s}}
}

func reply(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func replyError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}
