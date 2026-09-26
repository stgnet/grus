package web

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/store"
)

// Sign-in: type your email, tap the link (or type the code), and land back
// on the page you started from. No passwords, ever. The full flow is in the
// plan, section 8, "From a shared link to the post".

const (
	sessionCookie = "grus_session"
	pendingCookie = "grus_pending" // which sign-in the code form is for
	loginTTL      = 15 * time.Minute
	sessionTTL    = 90 * 24 * time.Hour
)

// user returns the signed-in account, or nil.
func (s *Server) user(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := s.Store.UserBySession(auth.Hash(c.Value), s.Now().Unix())
	if err != nil || u == nil {
		return nil
	}
	// A suspended account reads as signed out everywhere (SuspendUser also
	// ends its sessions; this covers a sign-in made during the suspension).
	if u.SuspendedUntil > s.Now().Unix() {
		return nil
	}
	return u
}

// setSession sets the session cookie on the request's domain, which covers
// every <slug>.<domain> group: one sign-in, every group, no redirects.
// A browser keeps each domain's cookie apart, so someone is signed in on
// each domain they use separately; that's what lets a domain (and the
// nodes behind it) be tried out on its own.
func (s *Server) setSession(w http.ResponseWriter, domain, raw string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    raw,
		Domain:   domain,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,                 // scripts can't read it
		Secure:   !s.Dev,               // HTTPS only
		SameSite: http.SameSiteLaxMode, // sent when following a link in, not on cross-site posts
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name, domain string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Domain: domain, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: !s.Dev, SameSite: http.SameSiteLaxMode})
}

type loginData struct {
	Email string
	Next  string
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	next := s.safeNext(r.URL.Query().Get("next"), rt.domain)
	if s.user(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", &page{Title: "Sign in", Data: loginData{Next: next}})
}

// loginSend emails a sign-in link and code.
func (s *Server) loginSend(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	next := s.safeNext(r.FormValue("next"), rt.domain)
	addr, err := mail.ParseAddress(strings.TrimSpace(r.FormValue("email")))
	if err != nil || len(addr.Address) > 254 {
		s.render(w, r, http.StatusBadRequest, "login", &page{Title: "Sign in",
			Error: "That doesn't look like an email address.", Data: loginData{Email: r.FormValue("email"), Next: next}})
		return
	}
	email := strings.ToLower(addr.Address)

	if !s.Limiter.Allow(email, clientIP(r), s.Now()) {
		s.render(w, r, http.StatusTooManyRequests, "login", &page{Title: "Sign in",
			Error: "Too many sign-in emails were sent recently. Please try again in an hour.",
			Data:  loginData{Email: email, Next: next}})
		return
	}

	raw, hash := auth.NewToken()
	code := auth.NewCode()
	now := s.Now()
	_, err = s.Log.Apply(&cmd.CreateLogin{
		TokenHash: hash,
		CodeHash:  auth.CodeHash(hash, code),
		Email:     email,
		ReturnURL: next,
		Domain:    rt.domain,
		At:        now.Unix(),
		ExpiresAt: now.Add(loginTTL).Unix(),
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	link := s.siteURL(rt.domain, "/link/"+raw)
	body := fmt.Sprintf("Tap to sign in to %s:\n\n%s\n\n"+
		"Or type this code on the page where you asked to sign in:\n\n    %s\n\n"+
		"The link and the code work once, for 15 minutes.\n"+
		"If you didn't ask to sign in, you can ignore this email.\n",
		rt.domain, link, code)
	if err := s.sendMail(rt.domain, email, "Sign in to "+rt.domain, body); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Remember, on this browser only, which sign-in the code form is for.
	// The cookie holds the token's hash, which can't sign anyone in by
	// itself; the code (from the email) is still needed.
	http.SetCookie(w, &http.Cookie{Name: pendingCookie, Value: hash, Path: "/",
		MaxAge: int(loginTTL.Seconds()), HttpOnly: true, Secure: !s.Dev, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/code", http.StatusSeeOther)
}

// codeForm is the "check your email" page, with a box for the code.
func (s *Server) codeForm(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(pendingCookie); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "code", &page{Title: "Check your email"})
}

func (s *Server) codeCheck(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(pendingCookie)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	t, err := s.Store.LoginToken(c.Value)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !s.loginUsable(t) {
		s.render(w, r, http.StatusGone, "dead", &page{Title: "Sign-in expired"})
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))
	if !auth.CodeMatches(t.TokenHash, code, t.CodeHash) {
		if _, err := s.Log.Apply(&cmd.FailLoginCode{TokenHash: t.TokenHash}); err != nil {
			s.serverError(w, r, err)
			return
		}
		if t.Tries+1 >= cmd.MaxCodeTries {
			s.render(w, r, http.StatusGone, "dead", &page{Title: "Sign-in expired",
				Error: "Too many wrong codes. Please ask for a new email."})
			return
		}
		s.render(w, r, http.StatusBadRequest, "code", &page{Title: "Check your email",
			Error: "That code didn't match. Check the latest email and try again."})
		return
	}
	s.finishSignIn(w, r, t)
}

// linkPage is where the emailed link lands. It shows a Continue button
// rather than signing in straight away, because corporate email scanners
// (Outlook Safe Links and friends) open every link in an email; if loading
// the page used the token up, the scanner would sign in instead of you.
func (s *Server) linkPage(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.LoginToken(auth.Hash(r.PathValue("token")))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !s.loginUsable(t) {
		s.render(w, r, http.StatusGone, "dead", &page{Title: "Sign-in expired"})
		return
	}
	s.render(w, r, http.StatusOK, "link", &page{Title: "Sign in"})
}

func (s *Server) linkRedeem(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.LoginToken(auth.Hash(r.PathValue("token")))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !s.loginUsable(t) {
		s.render(w, r, http.StatusGone, "dead", &page{Title: "Sign-in expired"})
		return
	}
	s.finishSignIn(w, r, t)
}

// loginUsable is the handler's early check, for a friendly page. The
// RedeemLogin command checks again, inside the log, and that's the one that
// counts.
func (s *Server) loginUsable(t *store.LoginToken) bool {
	return t != nil && t.UsedAt == 0 && t.ExpiresAt > s.Now().Unix() && t.Tries < cmd.MaxCodeTries
}

// finishSignIn redeems the sign-in, sets the session cookie, and sends the
// person back where they started (by way of picking a handle, the first
// time).
func (s *Server) finishSignIn(w http.ResponseWriter, r *http.Request, t *store.LoginToken) {
	rt := routeOf(r)
	raw, hash := auth.NewToken()
	now := s.Now()
	expires := now.Add(sessionTTL)
	v, err := s.Log.Apply(&cmd.RedeemLogin{
		TokenHash:      t.TokenHash,
		NewUserID:      s.IDs.Next(),
		SessionHash:    hash,
		SessionExpires: expires.Unix(),
		UserAgentHint:  uaHint(r.UserAgent()),
		Operator:       s.global().IsOperator(t.Email),
		At:             now.Unix(),
	})
	if errors.Is(err, cmd.ErrLoginDead) {
		s.render(w, r, http.StatusGone, "dead", &page{Title: "Sign-in expired"})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	red := v.(cmd.Redeemed)
	s.setSession(w, rt.domain, raw, expires)
	s.clearCookie(w, pendingCookie, "")

	u, err := s.Store.UserByID(red.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	dest := s.safeNext(red.ReturnURL, rt.domain)
	if u == nil || u.Handle == "" {
		dest = "/welcome?next=" + queryEscape(dest)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

type welcomeData struct {
	Handle string
	Next   string
}

// welcomeForm is the first sign-in: pick a handle. Nothing else is asked.
func (s *Server) welcomeForm(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	u := s.user(r)
	next := s.safeNext(r.URL.Query().Get("next"), rt.domain)
	if u == nil {
		http.Redirect(w, r, "/login?next="+queryEscape(next), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "welcome", &page{Title: "Welcome", User: u,
		Data: welcomeData{Handle: u.Handle, Next: next}})
}

func (s *Server) welcomeSave(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	u := s.user(r)
	next := s.safeNext(r.FormValue("next"), rt.domain)
	if u == nil {
		http.Redirect(w, r, "/login?next="+queryEscape(next), http.StatusSeeOther)
		return
	}
	handle := strings.ToLower(strings.TrimSpace(r.FormValue("handle")))
	fail := func(msg string) {
		s.render(w, r, http.StatusBadRequest, "welcome", &page{Title: "Welcome", User: u, Error: msg,
			Data: welcomeData{Handle: handle, Next: next}})
	}
	if err := cmd.ValidHandle(handle); err != nil {
		fail(capitalize(err.Error()) + ".")
		return
	}
	_, err := s.Log.Apply(&cmd.SetHandle{UserID: u.ID, Handle: handle})
	if errors.Is(err, cmd.ErrHandleTaken) {
		fail("Someone already uses that handle. Try another.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// logout ends this session on every node and goes back to the same site's
// front page.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	rt := routeOf(r)
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if _, err := s.Log.Apply(&cmd.EndSession{TokenHash: auth.Hash(c.Value)}); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.clearCookie(w, sessionCookie, rt.domain)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// clientIP is the connecting address. Behind bserver's proxy mode every
// request appears to come from localhost, so the per-IP limit becomes a
// second global limit there; the per-address and global limits still apply.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// uaHint keeps a short, rough description of the browser for a future "your
// sessions" list, not the full user agent string.
func uaHint(ua string) string {
	for _, b := range []string{"Firefox", "Edg", "Chrome", "Safari"} {
		if strings.Contains(ua, b) {
			if b == "Edg" {
				b = "Edge"
			}
			for _, os := range []string{"iPhone", "iPad", "Android", "Mac OS", "Windows", "Linux"} {
				if strings.Contains(ua, os) {
					return b + " on " + strings.TrimSuffix(os, " OS")
				}
			}
			return b
		}
	}
	return ""
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
