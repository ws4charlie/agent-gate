package paidtool

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func firecrawl(t *testing.T, status int, body string, seen *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if seen != nil {
			_ = json.NewDecoder(r.Body).Decode(seen)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const oneResult = `{"success":true,"data":{"web":[{"title":"Go","url":"https://go.dev","description":"The Go language"}]}}`

func call(t *testing.T, h http.Handler, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/search", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// open lets a test reach the tool without paying; refuse stands in for x402.
func open(next http.Handler) http.Handler { return next }

func refuse(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusPaymentRequired) })
}

func TestDiscoveryIsFreeButCallsArePaid(t *testing.T) {
	h := searchMCP(refuse, &Searcher{Key: "test-key"})
	code, out := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if code != http.StatusOK || out["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %v", code, out)
	}
	code, out = call(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if code != http.StatusOK || len(out["result"].(map[string]any)["tools"].([]any)) != 1 {
		t.Fatalf("tools/list: %d %v", code, out)
	}
	if code, _ = call(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	if code, _ = call(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"go"}}}`); code != http.StatusPaymentRequired {
		t.Fatalf("tools/call: %d, want 402", code)
	}
}

func TestBadCallIsRefusedBeforePayment(t *testing.T) {
	h := searchMCP(refuse, &Searcher{Key: "test-key"})
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"  "}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other","arguments":{"query":"go"}}}`,
	} {
		code, out := call(t, h, body)
		if code != http.StatusOK || out["error"] == nil {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
}

func TestSearchReturnsResultsAndCapsLimit(t *testing.T) {
	var seen map[string]any
	up := firecrawl(t, http.StatusOK, oneResult, &seen)
	h := searchMCP(open, &Searcher{Key: "test-key", URL: up.URL})
	_, out := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"go","limit":500}}}`)
	text := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "https://go.dev") || !strings.Contains(text, "The Go language") {
		t.Fatalf("text = %q", text)
	}
	if seen["limit"] != float64(maxResults) {
		t.Fatalf("limit = %v", seen["limit"])
	}
}

// A failed search must answer with an HTTP error: that is what stops x402 from
// settling, so the agent pays nothing for it.
func TestUpstreamFailureIsNotCharged(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusUnauthorized} {
		up := firecrawl(t, status, `{"error":"Bearer test-key is bad"}`, nil)
		h := searchMCP(open, &Searcher{Key: "test-key", URL: up.URL})
		code, out := call(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"go"}}}`)
		if code < 400 || out["error"] == nil {
			t.Fatalf("upstream %d: answered %d %v", status, code, out)
		}
		if b, _ := json.Marshal(out); strings.Contains(string(b), "test-key") {
			t.Fatalf("the upstream reply leaked to the caller: %s", b)
		}
	}
}
