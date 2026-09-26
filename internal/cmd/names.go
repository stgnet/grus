package cmd

import (
	"errors"
	"fmt"
	"strings"
)

// Rules for names people choose: group slugs and handles. They live with the
// commands because Apply is the last word on them; handlers check them first
// only to give a friendlier error.

// ReservedSlugs can never be group slugs. Subdomains of a domain are for
// groups only, so nothing the system needs ever takes a group's name. This
// list is the reverse protection: nobody can make login.nfb.group and
// imitate the sign-in page, or claim a name that looks official.
var ReservedSlugs = map[string]bool{
	"www": true, "mail": true, "smtp": true, "imap": true, "pop": true,
	"mx": true, "ns": true, "ns1": true, "ns2": true, "api": true,
	"admin": true, "login": true, "signin": true, "auth": true, "account": true,
	"static": true, "assets": true, "img": true, "cdn": true, "status": true,
	"help": true, "support": true, "abuse": true, "postmaster": true,
	"security": true, "root": true, "ftp": true, "webmail": true,
	"autoconfig": true, "autodiscover": true, "dev": true, "test": true,
	"staging": true, "app": true, "blog": true, "docs": true, "faq": true,
	"grus": true, "nfb": true, "about": true, "home": true,
}

var (
	ErrSlugTaken   = errors.New("that group name is taken")
	ErrHandleTaken = errors.New("that handle is taken")
	ErrLoginDead   = errors.New("sign-in link or code expired or already used")
	ErrNotFound    = errors.New("not found")
)

// InputError is a command refused because of what someone typed or chose
// (too long, missing, not allowed), as opposed to something failing. Pages
// show its message to the person; anything else is a server error.
type InputError struct{ msg string }

func (e *InputError) Error() string { return e.msg }

// Invalid makes an InputError.
func Invalid(format string, args ...any) error {
	return &InputError{msg: fmt.Sprintf(format, args...)}
}

// sentinels are the errors callers test for with errors.Is.
var sentinels = []error{ErrSlugTaken, ErrHandleTaken, ErrLoginDead, ErrNotFound, ErrGone, ErrLocked, ErrNotMember}

// Remote rebuilds a command's error after it crossed the network (a write
// forwarded to the leader), so errors.Is and IsInput still work on it.
func Remote(msg string, input bool) error {
	if !input {
		return errors.New(msg)
	}
	for _, s := range sentinels {
		if prefix, ok := strings.CutSuffix(msg, s.Error()); ok {
			return fmt.Errorf("%s%w", prefix, s)
		}
	}
	return &InputError{msg: msg}
}

// IsInput reports whether err is the person's to fix: an InputError or one of
// the sentinel errors above.
func IsInput(err error) bool {
	var ie *InputError
	return errors.As(err, &ie) || errors.Is(err, ErrSlugTaken) || errors.Is(err, ErrHandleTaken) ||
		errors.Is(err, ErrLoginDead) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrGone) ||
		errors.Is(err, ErrLocked) || errors.Is(err, ErrNotMember)
}

// ValidSlug checks a group slug: 2-32 of a-z, 0-9 and "-", not starting or
// ending with "-", and no "--" (which is how punycode names start, and
// looks like a typo anyway).
func ValidSlug(slug string) error {
	if len(slug) < 2 || len(slug) > 32 {
		return fmt.Errorf("group names are 2 to 32 characters")
	}
	for _, r := range slug {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return fmt.Errorf("group names use only a-z, 0-9 and -")
		}
	}
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") || strings.Contains(slug, "--") {
		return fmt.Errorf("group names can't start or end with - or contain --")
	}
	if ReservedSlugs[slug] {
		return fmt.Errorf("%q is reserved", slug)
	}
	return nil
}

// ValidHandle checks a handle: 3-20 of a-z, 0-9 and "_". Lowercase only, so
// "Scott" and "scott" can't be two different people.
func ValidHandle(h string) error {
	if len(h) < 3 || len(h) > 20 {
		return fmt.Errorf("handles are 3 to 20 characters")
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return fmt.Errorf("handles use only a-z, 0-9 and _")
		}
	}
	return nil
}

// ValidDomain does a light check of a domain name: lowercase labels of
// a-z, 0-9 and "-", at least two of them. DNS is the real check.
func ValidDomain(d string) error {
	labels := strings.Split(d, ".")
	if len(labels) < 2 || len(d) > 253 {
		return fmt.Errorf("%q is not a domain name", d)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return fmt.Errorf("%q is not a domain name", d)
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return fmt.Errorf("%q is not a domain name", d)
			}
		}
	}
	return nil
}
