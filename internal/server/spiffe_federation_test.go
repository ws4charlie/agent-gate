package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/ws4charlie/agent-gate/internal/agents"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/server"
)

const acmeBilling = "spiffe://acme.example/agent/billing"

// fedServer starts an mTLS listener for the domains in listed, with only the
// bundles in have in the (static) bundle set, as the Workload API would hold.
func fedServer(t *testing.T, e *env, own *testCA, listed []string, have ...*testCA) *httptest.Server {
	t.Helper()
	set := x509bundle.NewSet(own.bundle())
	for _, ca := range have {
		set.Add(ca.bundle())
	}
	var tds []spiffeid.TrustDomain
	for _, l := range listed {
		tds = append(tds, spiffeid.RequireTrustDomainFromString(l))
	}
	ts := httptest.NewUnstartedServer(server.SPIFFEHandler(e.srv.Handler()))
	ts.TLS = server.ServerTLSConfig(own.svid(t, "spiffe://example.org/gateway"), set, tds...)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func refused(t *testing.T, c *http.Client, base string) {
	t.Helper()
	resp, err := c.Get(base + "/v1/balance")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("got status %d, want a TLS failure", resp.StatusCode)
	}
}

func TestFederatedTrustDomains(t *testing.T) {
	e := newEnv(t, 5*usd, nil)
	own := newTestCA(t, "example.org")
	acme := newTestCA(t, "acme.example")
	both := []string{"example.org", "acme.example"}

	st := agents.NewStore(e.pool)
	ctx := context.Background()
	_, ours, err := st.Create(ctx, e.acct, "ours", policy.DefaultScope())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.SetSpiffeID(ctx, ours.ID, spiffeRunner); err != nil {
		t.Fatal(err)
	}
	_, fed, err := st.Create(ctx, e.acct, "billing", policy.DefaultScope())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.SetSpiffeID(ctx, fed.ID, acmeBilling); err != nil {
		t.Fatal(err)
	}

	t.Run("listed and bundle present: both domains accepted, each as its own agent", func(t *testing.T) {
		ts := fedServer(t, e, own, both, acme)
		if code, id := balanceAs(t, own.clientTo(own.svid(t, spiffeRunner)), ts.URL, "", nil); code != 200 || id != ours.ID {
			t.Errorf("own domain: got %d %q, want 200 %q", code, id, ours.ID)
		}
		if code, id := balanceAs(t, acme.clientTo(acme.svid(t, acmeBilling)), ts.URL, "", nil); code != 200 || id != fed.ID {
			t.Errorf("federated: got %d %q, want 200 %q", code, id, fed.ID)
		}
	})

	t.Run("federated ID nobody owns is 401", func(t *testing.T) {
		ts := fedServer(t, e, own, both, acme)
		c := acme.clientTo(acme.svid(t, "spiffe://acme.example/agent/other"))
		if code, _ := balanceAs(t, c, ts.URL, "", nil); code != 401 {
			t.Errorf("got %d, want 401", code)
		}
	})

	t.Run("not listed: refused even with a bundle", func(t *testing.T) {
		ts := fedServer(t, e, own, []string{"example.org"}, acme)
		refused(t, acme.clientTo(acme.svid(t, acmeBilling)), ts.URL)
		if code, _ := balanceAs(t, own.clientTo(own.svid(t, spiffeRunner)), ts.URL, "", nil); code != 200 {
			t.Errorf("own domain: got %d, want 200", code)
		}
	})

	t.Run("listed but no bundle: refused, own domain still works", func(t *testing.T) {
		ts := fedServer(t, e, own, both)
		refused(t, acme.clientTo(acme.svid(t, acmeBilling)), ts.URL)
		if code, _ := balanceAs(t, own.clientTo(own.svid(t, spiffeRunner)), ts.URL, "", nil); code != 200 {
			t.Errorf("own domain: got %d, want 200", code)
		}
	})

	t.Run("a federated CA cannot issue IDs of our domain", func(t *testing.T) {
		ts := fedServer(t, e, own, both, acme)
		refused(t, acme.clientTo(acme.svid(t, spiffeRunner)), ts.URL)
	})
}

func TestMatchMemberOfAny(t *testing.T) {
	m := server.MatchMemberOfAny(spiffeid.RequireTrustDomainFromString("a.example"), spiffeid.RequireTrustDomainFromString("b.example"))
	for id, want := range map[string]bool{"spiffe://a.example/x": true, "spiffe://b.example/y": true, "spiffe://c.example/z": false} {
		if got := m(spiffeid.RequireFromString(id)) == nil; got != want {
			t.Errorf("%s: accepted=%v, want %v", id, got, want)
		}
	}
	if server.MatchMemberOfAny()(spiffeid.RequireFromString("spiffe://a.example/x")) == nil {
		t.Error("empty list accepted an ID")
	}
}
