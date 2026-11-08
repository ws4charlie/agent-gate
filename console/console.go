// Package console is the operator's local web console: an overview of every
// agent (spend, calls, refusals, balance), the controls an operator changes
// (budgets, allowlists, kill switch), and a place to run research tasks from
// templates and read their results.
//
// It reads the gateway's database directly, like the CLI does: there is no
// HTTP admin route on the gateway. Tasks it runs are ordinary agent traffic:
// each template has its own agent, and every model call, tool call and payment
// goes through the gateway, so the console never holds a provider key and gets
// no exemption from policy.
//
// It listens on loopback only. Requests whose Host is not a loopback name are
// refused (DNS rebinding), and so are state-changing requests from another
// origin (a page on the internet posting to localhost). The API also needs the
// console key, which the operator gets in the link the console prints: an
// agent on the same machine cannot raise its own budget with a bare curl.
package console

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/agents"
	runs "github.com/ws4charlie/agent-gate/internal/run"
)

//go:embed web
var webFS embed.FS

// Config is how the console is started.
type Config struct {
	Gateway string // base URL of the gateway the tasks call, e.g. http://127.0.0.1:18090
	Account string // ledger account the template agents spend from; "" disables running tasks
	DataDir string // templates, template agent keys, run results

	WalletAddress string // optional: show this address's USDC balance
	RPCURL        string // JSON-RPC endpoint for the wallet balance
	USDC          string // USDC contract on that chain

	KnownTools []string // tool names from the gateway's MCP and payments files, offered in forms

	// DefaultPayees are the addresses the built-in templates may pay: the
	// sellers of the paid tools this deployment trusts. A built-in template
	// ships with no payee, since an address is a deployment's choice.
	DefaultPayees []string
}

type Server struct {
	cfg    Config
	pool   *pgxpool.Pool
	agents *agents.Store
	runs   *runs.Store
	client *http.Client
	key    string // the console key; the API refuses requests without it

	mu   sync.Mutex
	live map[string]*task // tasks running in this process, by run id
}

func New(cfg Config, pool *pgxpool.Pool) (*Server, error) {
	if err := ensureDirs(cfg.DataDir); err != nil {
		return nil, err
	}
	key, err := loadKey(filepath.Join(cfg.DataDir, "console.key"))
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, pool: pool, agents: agents.NewStore(pool), runs: runs.New(pool), key: key,
		client: &http.Client{Timeout: 3 * time.Minute}, live: map[string]*task{}}, nil
}

// Key is the console key, for the link the operator opens.
func (s *Server) Key() string { return s.key }

// loadKey reads the console key, making one on first start. It is kept so a
// bookmarked link keeps working; delete the file to change it.
func loadKey(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		if k := strings.TrimSpace(string(raw)); len(k) >= 32 {
			return k, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	k := base64.RawURLEncoding.EncodeToString(b[:])
	return k, writeFileAtomic(path, []byte(k+"\n"))
}

// Handler serves the page and the JSON API.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	mux := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServerFS(web))
	mux.Handle("/api/", s.requireKey(api))

	api.HandleFunc("GET /api/overview", s.handleOverview)
	api.HandleFunc("GET /api/events", s.handleEvents)
	api.HandleFunc("PUT /api/agents/{ref}/scope", s.handleSetScope)
	api.HandleFunc("POST /api/agents/{ref}/kill", s.handleKill)
	api.HandleFunc("POST /api/agents/{ref}/resume", s.handleResume)

	api.HandleFunc("GET /api/templates", s.handleTemplates)
	api.HandleFunc("POST /api/templates", s.handleSaveTemplate)
	api.HandleFunc("DELETE /api/templates/{id}", s.handleDeleteTemplate)

	api.HandleFunc("GET /api/runs", s.handleRuns)
	api.HandleFunc("POST /api/runs", s.handleStartRun)
	api.HandleFunc("GET /api/runs/{id}", s.handleRun)
	api.HandleFunc("POST /api/runs/{id}/kill", s.handleKillRun)
	api.HandleFunc("GET /api/runs/{id}/files/{name}", s.handleRunFile)
	return guard(mux)
}

// Shutdown cancels the tasks still running.
func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.live {
		t.cancel()
	}
}

// requireKey refuses API requests without the console key.
func (s *Server) requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("X-Console-Key")
		if k == "" || subtle.ConstantTimeCompare([]byte(k), []byte(s.key)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("console key missing or wrong: open the link the console printed"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guard refuses requests that did not come from a page this console served.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "console: host not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			o := r.Header.Get("Origin")
			if o == "" || !sameHost(o, r.Host) {
				http.Error(w, "console: cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == host
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, agents.ErrNotFound), errors.Is(err, runs.ErrNotFound), errors.Is(err, errNoTemplate):
		return http.StatusNotFound
	case errors.Is(err, agents.ErrBadState), errors.Is(err, errInvalid):
		return http.StatusBadRequest
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}
