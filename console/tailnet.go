package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The console can also be served on a Tailscale network, so a few named
// people can open it from their own machines. There it needs no console key:
// each connection's identity comes from tailscaled, which knows which signed-in
// user owns the device at the other end of the WireGuard tunnel. Nothing a
// browser or another program sends can change that answer, so only the logins
// on the allow list get in.

// Identity is who is at the other end of a tailnet connection.
type Identity struct {
	Login string // the user's login, for example someone@gmail.com
	Name  string // display name
}

// Whois says who is at a remote address on the tailnet.
type Whois func(ctx context.Context, remoteAddr string) (Identity, error)

// TailscaleWhois asks the local tailscaled through its LocalAPI socket.
func TailscaleWhois(socket string) Whois {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	return func(ctx context.Context, remoteAddr string) (Identity, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://local-tailscaled.sock/localapi/v0/whois?addr="+url.QueryEscape(remoteAddr), nil)
		if err != nil {
			return Identity{}, err
		}
		res, err := c.Do(req)
		if err != nil {
			return Identity{}, fmt.Errorf("tailscale whois: %w", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return Identity{}, fmt.Errorf("tailscale whois: %s", res.Status)
		}
		var out struct {
			UserProfile struct {
				LoginName   string
				DisplayName string
			}
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			return Identity{}, fmt.Errorf("tailscale whois: %w", err)
		}
		if out.UserProfile.LoginName == "" {
			return Identity{}, errors.New("tailscale whois: no user for this address")
		}
		return Identity{Login: out.UserProfile.LoginName, Name: out.UserProfile.DisplayName}, nil
	}
}

// TailnetHandler serves the console on the tailnet under one host name, to
// the logins on allow only. It is the same console as Handler, without the
// console key: the identity check takes its place.
func (s *Server) TailnetHandler(host string, allow []string, who Whois) http.Handler {
	allowed := map[string]bool{}
	for _, a := range allow {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			allowed[a] = true
		}
	}
	host = strings.ToLower(host)
	inner := s.routes(func(h http.Handler) http.Handler { return h })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := strings.ToLower(r.Host)
		if hh, port, err := net.SplitHostPort(h); err == nil && port == "443" {
			h = hh
		}
		if h != host {
			http.Error(w, "console: host not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != "https://"+host {
			http.Error(w, "console: cross-origin request refused", http.StatusForbidden)
			return
		}
		id, err := who(r.Context(), r.RemoteAddr)
		if err != nil {
			http.Error(w, "console: could not tell who you are on the tailnet", http.StatusForbidden)
			return
		}
		if !allowed[strings.ToLower(id.Login)] {
			http.Error(w, "console: "+id.Login+" is not on this console's list. Ask its owner to add you.", http.StatusForbidden)
			return
		}
		securityHeaders(w)
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		inner.ServeHTTP(w, r)
	})
}

// TailnetAddr reports whether an address is on a tailnet (Tailscale's
// 100.64.0.0/10 and fd7a:115c:a1e0::/48), so the console is never served on
// a public interface by mistake.
func TailnetAddr(ip net.IP) bool {
	for _, cidr := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} {
		_, n, _ := net.ParseCIDR(cidr)
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
