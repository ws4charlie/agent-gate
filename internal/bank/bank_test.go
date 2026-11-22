package bank

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every status Mercury documents maps to one state; anything else keeps the state.
func TestTargetMapping(t *testing.T) {
	cases := []struct {
		name string
		cur  State
		req  Request
		txn  *Txn
		to   State
		ok   bool
	}{
		{"request pending", AwaitingBankApproval, Request{Status: ReqPending}, nil, AwaitingBankApproval, true},
		{"request approved", AwaitingBankApproval, Request{Status: ReqApproved}, nil, BankApproved, true},
		{"request rejected", AwaitingBankApproval, Request{Status: ReqRejected}, nil, Failed, true},
		{"request cancelled", AwaitingBankApproval, Request{Status: ReqCancelled}, nil, Cancelled, true},
		{"request unknown", AwaitingBankApproval, Request{Status: "x"}, nil, AwaitingBankApproval, false},
		{"txn pending", BankApproved, Request{Status: ReqApproved}, &Txn{Status: TxnPending}, BankApproved, true},
		{"txn sent", BankApproved, Request{Status: ReqApproved}, &Txn{Status: TxnSent}, Sent, true},
		{"txn sent and posted", Sent, Request{Status: ReqApproved}, &Txn{Status: TxnSent, Posted: true}, Settled, true},
		{"txn cancelled", BankApproved, Request{Status: ReqApproved}, &Txn{Status: TxnCancelled}, Cancelled, true},
		{"txn failed before sent", BankApproved, Request{Status: ReqApproved}, &Txn{Status: TxnFailed}, Failed, true},
		{"txn blocked before sent", BankApproved, Request{Status: ReqApproved}, &Txn{Status: TxnBlocked}, Failed, true},
		{"txn reversed before sent", BankApproved, Request{Status: ReqApproved}, &Txn{Status: TxnReversed}, Failed, true},
		{"txn failed after sent is a return", Sent, Request{Status: ReqApproved}, &Txn{Status: TxnFailed}, Returned, true},
		{"txn reversed after sent is a return", Sent, Request{Status: ReqApproved}, &Txn{Status: TxnReversed}, Returned, true},
		{"txn reversed after settled is a return", Settled, Request{Status: ReqApproved}, &Txn{Status: TxnReversed}, Returned, true},
		{"txn unknown", BankApproved, Request{Status: ReqApproved}, &Txn{Status: "x"}, BankApproved, false},
	}
	for _, c := range cases {
		to, ok := Target(c.cur, c.req, c.txn)
		if to != c.to || ok != c.ok {
			t.Errorf("%s: got %s/%v, want %s/%v", c.name, to, ok, c.to, c.ok)
		}
	}
}

func TestStateMachine(t *testing.T) {
	for _, s := range []State{Refused, Cancelled, Failed, Returned} {
		if !s.Final() {
			t.Errorf("%s should be final", s)
		}
		for _, to := range []State{AwaitingBankApproval, BankApproved, Sent, Settled, Returned, Failed, Cancelled, Refused} {
			if allowed(s, to) {
				t.Errorf("%s -> %s must not be allowed", s, to)
			}
		}
	}
	if Settled.Final() || Sent.Final() {
		t.Error("sent and settled can still be returned")
	}
	if !allowed(Settled, Returned) || allowed(Settled, Sent) || allowed(Sent, BankApproved) {
		t.Error("forward path wrong")
	}
}

func TestIsSandbox(t *testing.T) {
	for u, want := range map[string]bool{
		SandboxBaseURL:                                 true,
		"https://api-sandbox.mercury.com/":             true,
		"https://api.mercury.com/api/v1/":              false,
		"http://api-sandbox.mercury.com/api":           false, // plain http is not it
		"https://api-sandbox.mercury.com.evil":         false,
		"https://evil.example/api-sandbox.mercury.com": false,
		"": false,
	} {
		if got := IsSandbox(u); got != want {
			t.Errorf("IsSandbox(%q) = %v, want %v", u, got, want)
		}
	}
}

// The Mercury rail talks to a local stand-in: the right paths, the idempotency
// key passed through, ACH only, request-send-money (never direct send money).
func TestMercuryRailAgainstStandIn(t *testing.T) {
	var paths []string
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer dummy-test-token" {
			t.Errorf("bad auth header")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/request-send-money"):
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &sent)
			io.WriteString(w, `{"requestId":"11111111-1111-1111-1111-111111111111","status":"pendingApproval","reviews":[]}`)
		case strings.HasSuffix(r.URL.Path, "/request-send-money/r1"):
			io.WriteString(w, `{"requestId":"r1","status":"approved","reviews":[{"reviewerUserId":"u2","status":"approved","reviewedAt":"2026-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/transactions"):
			if r.URL.Query().Get("requestId") != "r1" {
				t.Errorf("transactions not filtered by request id: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"total":1,"transactions":[{"id":"t1","status":"sent","postedAt":"2026-01-02T00:00:00Z"}]}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"errors":{}}`)
		}
	}))
	defer srv.Close()
	m, err := NewMercury(MercuryConfig{BaseURL: srv.URL, AccountID: "acct-1", Token: "dummy-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req, err := m.RequestSend(ctx, SendReq{RecipientID: "rcpt", Cents: 1234, IdempotencyKey: "ag-1", Note: "n"})
	if err != nil || req.Status != ReqPending {
		t.Fatalf("%v %+v", err, req)
	}
	if sent["amount"] != 12.34 || sent["paymentMethod"] != "ach" || sent["idempotencyKey"] != "ag-1" || sent["recipientId"] != "rcpt" {
		t.Fatalf("request body %v", sent)
	}
	got, err := m.GetRequest(ctx, "r1")
	if err != nil || got.Status != ReqApproved || len(got.Reviews) != 1 || got.Reviews[0].Reviewer != "u2" {
		t.Fatalf("%v %+v", err, got)
	}
	txn, err := m.TxnForRequest(ctx, "r1")
	if err != nil || txn == nil || txn.Status != TxnSent || !txn.Posted {
		t.Fatalf("%v %+v", err, txn)
	}
	for _, p := range paths {
		if p == "POST /account/acct-1/transactions" {
			t.Fatal("direct send money must never be called")
		}
	}
	// A 4xx is a refusal, not a transient error.
	if _, err := m.GetRequest(ctx, "missing"); !errors.Is(err, ErrRejected) {
		t.Fatalf("404 should be ErrRejected, got %v", err)
	}
}

func TestMercuryNeedsTokenAndAccount(t *testing.T) {
	if _, err := NewMercury(MercuryConfig{AccountID: "a"}); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, err := NewMercury(MercuryConfig{Token: "t"}); err == nil {
		t.Fatal("empty account accepted")
	}
}
