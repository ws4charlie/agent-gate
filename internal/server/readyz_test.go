package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/db/dbtest"
	"github.com/ws4charlie/agent-gate/internal/server"
)

func readyz(t *testing.T, s *server.Server) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body is not JSON: %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestReadyzDatabaseUp(t *testing.T) {
	code, body := readyz(t, &server.Server{Pool: dbtest.New(t)})
	if code != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("got %d %v, want 200 ready", code, body)
	}
}

func TestReadyzDatabaseDown(t *testing.T) {
	// A pool whose server refuses connections: port 1 on loopback.
	pool, err := pgxpool.New(t.Context(), "postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	code, body := readyz(t, &server.Server{Pool: pool})
	if code != http.StatusServiceUnavailable || body["reason"] != "database unreachable" {
		t.Fatalf("got %d %v, want 503 database unreachable", code, body)
	}
}

func TestReadyzNoPool(t *testing.T) {
	code, body := readyz(t, &server.Server{})
	if code != http.StatusServiceUnavailable || body["reason"] != "no database" {
		t.Fatalf("got %d %v, want 503 no database", code, body)
	}
}

func TestHealthzDoesNotNeedDatabase(t *testing.T) {
	rec := httptest.NewRecorder()
	(&server.Server{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}
