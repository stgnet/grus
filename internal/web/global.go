package web

import (
	"log"

	"github.com/stgnet/grus/internal/mail"
	"github.com/stgnet/grus/internal/store"
)

// The global settings and the per-domain mail settings, as the web side
// uses them. Both are read from site.db each time (a few small rows), so a
// change on the admin page applies at once on every node.

// global reads the global settings. On an error it logs and returns the
// defaults: a page shouldn't fail because, say, the ask limit couldn't be
// read.
func (s *Server) global() *store.Global {
	g, err := s.Store.Global()
	if err != nil {
		log.Printf("reading global settings: %v", err)
		return &store.Global{SMTPPort: 587, AIContext: 16384, AskLimit: 20, FAQHour: 8, DigestHour: 12}
	}
	return g
}

// mailer is how email for domain goes out: the domain's own SMTP relay if
// it has one, otherwise the global relay, otherwise printed to MailLog.
// The sender is the domain's mail_from, or login@<domain>, so email about
// a domain always comes from that domain.
func (s *Server) mailer(domain string) (*mail.Mailer, error) {
	g, err := s.Store.Global()
	if err != nil {
		return nil, err
	}
	if isLocal(domain) {
		// Email about localhost (a sign-in while testing) goes out in the
		// name of the first listed domain: login@localhost would bounce.
		// Its links still lead back to localhost.
		domains, err := s.Store.Domains()
		if err != nil {
			return nil, err
		}
		if len(domains) > 0 {
			domain = domains[0].Name
		}
	}
	m := &mail.Mailer{Host: g.SMTPHost, Port: g.SMTPPort, User: g.SMTPUser, Pass: g.SMTPPass,
		From: "login@" + domain, Dev: s.MailLog}
	d, err := s.Store.DomainNamed(domain)
	if err != nil {
		return nil, err
	}
	if d != nil {
		if d.SMTPHost != "" {
			m.Host, m.User, m.Pass = d.SMTPHost, d.SMTPUser, d.SMTPPass
			m.Port = d.SMTPPort
			if m.Port == 0 {
				m.Port = 587
			}
		}
		if d.MailFrom != "" {
			m.From = d.MailFrom
		}
	}
	return m, nil
}

// sendMail sends one email in domain's name.
func (s *Server) sendMail(domain, to, subject, body string) error {
	m, err := s.mailer(domain)
	if err != nil {
		return err
	}
	return m.Send(to, subject, body)
}

// userDomain is the domain to email u about: the one they last signed in
// on, while it's still listed; otherwise the oldest listed domain. ""
// only when no domain is listed at all.
func (s *Server) userDomain(u *store.User, domains []store.Domain) string {
	for _, d := range domains {
		if d.Name == u.Domain {
			return d.Name
		}
	}
	if len(domains) > 0 {
		return domains[0].Name
	}
	return ""
}
