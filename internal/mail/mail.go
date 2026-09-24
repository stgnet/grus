// Package mail sends the few emails the site sends (sign-in links for now;
// notifications and digests later).
//
// It speaks plain SMTP to a relay (your mail provider, or a local
// Postfix). The relay is what DKIM-signs the message; docs/dns.md lists the
// SPF, DKIM and DMARC records that make magic links land in the inbox rather
// than in spam.
package mail

import (
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mailer sends email through an SMTP relay. With no Host set it writes each
// message to Dev instead, which is how you sign in on a laptop: the link
// appears in the server's output.
type Mailer struct {
	Host string
	Port int
	User string
	Pass string
	From string

	Dev   io.Writer
	devMu sync.Mutex
}

// Send sends one plain-text email.
func (m *Mailer) Send(to, subject, body string) error {
	// Header injection guard: nothing we put in a header may contain a line
	// break. The address has been parsed already; this is belt and braces.
	if strings.ContainsAny(to+subject, "\r\n") {
		return fmt.Errorf("mail: line break in header")
	}
	msg := "From: " + m.From + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" + strings.ReplaceAll(body, "\n", "\r\n")

	if m.Host == "" {
		m.devMu.Lock()
		defer m.devMu.Unlock()
		_, err := fmt.Fprintf(m.Dev, "---- mail (no smtp_host set, not sent) ----\n%s\n---- end mail ----\n", msg)
		return err
	}

	addr := net.JoinHostPort(m.Host, strconv.Itoa(m.Port))
	var a smtp.Auth
	if m.User != "" {
		// PlainAuth refuses to send the password unless the connection is
		// TLS (smtp.SendMail upgrades with STARTTLS when offered).
		a = smtp.PlainAuth("", m.User, m.Pass, m.Host)
	}
	return smtp.SendMail(addr, a, m.From, []string{to}, []byte(msg))
}
