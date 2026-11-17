package server

import (
	"context"
	"crypto/tls"
	"net/http"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// spiffeKey is unexported on purpose: only SPIFFEHandler can put an identity
// in a request context, and it only does so from a certificate the TLS layer
// has verified. A header, a query parameter or a body cannot reach it.
type spiffeKey struct{}

// SPIFFEIDFromContext returns the verified SPIFFE ID of the caller, if the
// request came in over the mutual-TLS listener.
func SPIFFEIDFromContext(ctx context.Context) (spiffeid.ID, bool) {
	id, ok := ctx.Value(spiffeKey{}).(spiffeid.ID)
	return id, ok
}

// ServerTLSConfig is the mutual-TLS config of the SPIFFE listener: it
// presents the gateway's own SVID, requires a client certificate and accepts
// only SVIDs of one trust domain. src and bundles are normally one
// workloadapi.X509Source, which keeps both fresh as SPIRE rotates them.
func ServerTLSConfig(src x509svid.Source, bundles x509bundle.Source, td spiffeid.TrustDomain) *tls.Config {
	return tlsconfig.MTLSServerConfig(src, bundles, tlsconfig.AuthorizeMemberOf(td))
}

// SPIFFEHandler wraps the gateway handler for the mutual-TLS listener. It
// stores the verified peer's SPIFFE ID in the request context, which is what
// authenticate looks for. The plain listener does not use this wrapper, so it
// never carries an identity.
func SPIFFEHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, err := x509svid.IDFromCert(r.TLS.PeerCertificates[0])
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), spiffeKey{}, id)))
	})
}
