package web

import (
	"context"
	"errors"

	"golang.org/x/crypto/acme/autocert"

	"github.com/stgnet/grus/internal/cmd"
)

// Certificates: one Let's Encrypt certificate per host, fetched the first
// time the host is used. (A single wildcard certificate would need a DNS
// challenge through the DNS provider's API; per-host is simpler to start.)
//
// Two details matter:
//   - The host policy allows exactly the hosts resolve() knows: the primary,
//     www, groups, custom domains, aliases and alternates. A random
//     subdomain gets no certificate, so it can't burn Let's Encrypt's rate
//     limit (50 new certificates per domain per week), and typo.nfb.group
//     fails to connect rather than showing a page.
//   - The cache is the certs table in site.db, written through the log, so
//     every node has every certificate and any node can answer any group's
//     HTTPS.

// CertManager returns the autocert manager for this server.
func (s *Server) CertManager(email string) *autocert.Manager {
	return &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Email:  email,
		Cache:  certCache{s},
		HostPolicy: func(_ context.Context, host string) error {
			rt, err := s.resolve(host)
			if err != nil {
				return err
			}
			if rt.kind == siteUnknown {
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
