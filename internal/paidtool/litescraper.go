package paidtool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// LiteCrawlerPrice is one search in USDC atomic units ($0.01).
var LiteCrawlerPrice = big.NewInt(10_000)

const (
	defaultSearchURL = "https://api.firecrawl.dev/v2/search"
	defaultResults   = 5
	maxResults       = 10
	maxSearchBody    = 1 << 16
	maxUpstreamBody  = 4 << 20
)

// LiteCrawler is this repository's demo paid tool: a reseller. It charges an
// agent through x402 and runs the search on Firecrawl, which it pays separately
// with an API key. Firecrawl never sees the x402 payment or a signature.
//
// With Cache set, a result already on disk is returned without calling
// Firecrawl, and a new one is saved. With Replay as well, Firecrawl is never
// called and a query that was not recorded fails. Local testing then spends no
// credits and cannot hit the rate limit. Failed searches are not recorded.
type LiteCrawler struct {
	Key    string
	URL    string // default: Firecrawl's search endpoint
	Client *http.Client
	Cache  string // directory of recorded results; empty: off
	Replay bool
}

type recording struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
	Text  string `json:"text"`
}

func (s *LiteCrawler) recordingPath(query string, limit int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", limit, strings.TrimSpace(query))))
	return filepath.Join(s.Cache, hex.EncodeToString(sum[:])+".json")
}

func (s *LiteCrawler) recorded(query string, limit int) (string, bool) {
	b, err := os.ReadFile(s.recordingPath(query, limit))
	if err != nil {
		return "", false
	}
	var r recording
	if json.Unmarshal(b, &r) != nil {
		return "", false
	}
	return r.Text, true
}

func (s *LiteCrawler) record(query string, limit int, text string) error {
	if err := os.MkdirAll(s.Cache, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(recording{Query: query, Limit: limit, Text: text})
	if err != nil {
		return err
	}
	return os.WriteFile(s.recordingPath(query, limit), b, 0o600)
}

// Search returns the top results as plain text, one block per result.
func (s *LiteCrawler) Search(ctx context.Context, query string, limit int) (string, error) {
	if limit <= 0 {
		limit = defaultResults
	}
	limit = min(limit, maxResults)
	if s.Cache != "" {
		if text, ok := s.recorded(query, limit); ok {
			return text, nil
		}
		if s.Replay {
			return "", errors.New("no recorded result for this search")
		}
	}
	text, err := s.fetch(ctx, query, limit)
	if err != nil {
		return "", err
	}
	if s.Cache != "" {
		if err := s.record(query, limit, text); err != nil {
			return "", errors.New("could not record the search result")
		}
	}
	return text, nil
}

func (s *LiteCrawler) fetch(ctx context.Context, query string, limit int) (string, error) {
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

// LiteCrawlerMCP is a small MCP server with one paid tool. Discovery is free, so a
// client can start up and list tools; only tools/call goes through x402.
func LiteCrawlerMCP(v *x402.Verifier, s *LiteCrawler) http.Handler {
	return liteCrawlerMCP(func(next http.Handler) http.Handler {
		return v.Protect(LiteCrawlerPrice, "Web search", next)
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

func liteCrawlerMCP(protect func(http.Handler) http.Handler, s *LiteCrawler) http.Handler {
	paid := protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		text, err := s.Search(r.Context(), in.Params.Arguments.Query, in.Params.Arguments.Limit)
		if err != nil {
			// An HTTP failure tells x402 not to settle: a failed search costs nothing.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "error": map[string]any{"code": -32000, "message": err.Error()}})
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
				"serverInfo":      map[string]string{"name": "litecrawler", "version": "0.1.0"},
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
