package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"golang.org/x/crypto/acme/autocert"

	"github.com/stgnet/grus/internal/cmd"
)

// Certificates: one Let's Encrypt certificate per host, fetched the first
// time the host is used. (A single wildcard certificate would need a DNS
// challenge through the DNS provider's API; per-host is simpler to start.)
//
// Two details matter:
//   - The host policy allows exactly the hosts resolve() knows: each listed
//     domain, its www, and its groups. A random
//     subdomain gets no certificate, so it can't burn Let's Encrypt's rate
//     limit (50 new certificates per domain per week), and typo.nfb.group
//     fails to connect rather than showing a page.
//   - The cache is the certs table in site.db, written through the log, so
//     every node has every certificate and any node can answer any group's
//     HTTPS.

// CertManager returns the autocert manager for this server. The contact
// email is the acme_email global setting at start; Let's Encrypt only uses
// it when the account is first made.
func (s *Server) CertManager() *autocert.Manager {
	return &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Email:  s.global().ACMEEmail,
		Cache:  certCache{s},
		HostPolicy: func(_ context.Context, host string) error {
			rt, err := s.resolve(host)
			if err != nil {
				return err
			}
			// localhost is plain HTTP only (LocalOr): no certificate can be
			// had for it, and asking Let's Encrypt would only fail.
			if rt.kind == siteUnknown || isLocal(host) {
				return errUnknownHost
			}
			return nil
		},
	}
}

var errUnknownHost = errors.New("not a host this site serves")

// certCache implements autocert.Cache on site.db.
type certCache struct{ s *Server }

func (c certCache) Get(_ context.Context, name string) ([]byte, error) {
	pem, err := c.s.Store.Cert(name)
	if err != nil {
		return nil, err
	}
	if pem == nil {
		return nil, autocert.ErrCacheMiss
	}
	return pem, nil
}

func (c certCache) Put(_ context.Context, name string, data []byte) error {
	_, err := c.s.Log.Apply(&cmd.PutCert{Name: name, PEM: data, At: c.s.Now().Unix()})
	return err
}

func (c certCache) Delete(_ context.Context, name string) error {
	_, err := c.s.Log.Apply(&cmd.DeleteCert{Name: name})
	return err
}

// LocalOr serves requests for localhost from this machine as the site, over
// plain HTTP, and hands everything else to next: on port 80 that's the
// certificate manager's handler, which answers Let's Encrypt and redirects
// the rest to HTTPS. So http://localhost/ (or an ssh tunnel to port 80)
// reaches the site on a node with no DNS pointing at it yet.
func (s *Server) LocalOr(next http.Handler) http.Handler {
	site := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLocal(strings.ToLower(r.Host)) && isLoopback(r) {
			site.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
