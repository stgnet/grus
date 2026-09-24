// Package web is the HTTP side: which site a request is for (from its Host
// header), the pages, and the middleware every request passes through.
//
// Handlers read through internal/store and write by submitting commands to
// the cluster log. They never write SQL.
package web

import (
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/stgnet/grus/internal/ai"
	"github.com/stgnet/grus/internal/auth"
	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/mail"
	"github.com/stgnet/grus/internal/store"
	webfiles "github.com/stgnet/grus/web"
)

// Server is the web front end of one node.
type Server struct {
	Store *store.Store
	Log   cluster.Log
	Mail  *mail.Mailer
	IDs   *ids.Generator

	// Dev serves plain HTTP with non-Secure cookies. PortSuffix (":8080")
	// is added to every URL we build, for the same reason.
	Dev        bool
	PortSuffix string

	// Photos. PushBlob (optional) copies a newly stored photo to the other
	// nodes before the post that uses it is written, so every photo exists
	// in at least two places from the start.
	Blobs    *blob.Store
	PushBlob func(hash string)

	IsOperator func(email string) bool // from the config file
	Limiter    *auth.SendLimiter
	Now        func() time.Time // replaced in tests

	// AI: where searches run (nil when no node has a model), usage
	// counting, and the per-person daily question limit.
	AI       *ai.Pool
	Meter    *ai.Meter
	AskLimit int

	pages     map[string]*template.Template
	fragments *template.Template // pieces of pages the scripts fetch
	asks      askCounter
	homeMux   *http.ServeMux // the bare primary domain: sign-in, home, admin
	groupMux  *http.ServeMux // any group's host
}

// New sets up the templates and routes.
func New(s *Server) (*Server, error) {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Limiter == nil {
		s.Limiter = auth.NewSendLimiter()
	}
	if s.IsOperator == nil {
		s.IsOperator = func(string) bool { return false }
	}
	if err := s.loadPages(); err != nil {
		return nil, err
	}
	static, err := fs.Sub(webfiles.Files, "static")
	if err != nil {
		return nil, err
	}
	staticHandler := cacheStatic(http.StripPrefix("/static/", http.FileServerFS(static)))

	// Routes on the bare primary domain (plan section 10).
	h := http.NewServeMux()
	h.HandleFunc("GET /{$}", s.home)
	h.HandleFunc("GET /login", s.loginForm)
	h.HandleFunc("POST /login", s.loginSend)
	h.HandleFunc("GET /code", s.codeForm)
	h.HandleFunc("POST /code", s.codeCheck)
	h.HandleFunc("GET /link/{token}", s.linkPage)
	h.HandleFunc("POST /link/{token}", s.linkRedeem)
	h.HandleFunc("GET /welcome", s.welcomeForm)
	h.HandleFunc("POST /welcome", s.welcomeSave)
	h.HandleFunc("POST /logout", s.logout)
	h.HandleFunc("GET /admin", s.admin)
	h.HandleFunc("POST /admin/groups", s.adminCreateGroup)
	h.HandleFunc("POST /admin/domains", s.adminDomain)
	h.HandleFunc("POST /admin/aliases", s.adminAlias)
	h.HandleFunc("GET /how-it-works", s.howItWorks)
	// The root FAQ: the same pages as a group's, over the reserved root
	// group file, edited by operators.
	h.HandleFunc("GET /faq", s.faqPage)
	h.HandleFunc("GET /faq/e/{id}", s.entryPage)
	h.HandleFunc("GET /faq/e/{id}/edit", s.entryEditForm)
	h.HandleFunc("POST /faq/e/{id}/edit", s.entryEdit)
	h.HandleFunc("GET /faq/e/{id}/history", s.entryHistory)
	h.HandleFunc("POST /faq/e/{id}/rollback", s.entryRollback)
	h.HandleFunc("GET /faq/new", s.entryNewForm)
	h.HandleFunc("POST /faq/new", s.entryNew)
	h.HandleFunc("POST /faq/topics", s.topicCreate)
	h.HandleFunc("GET /faq/t/{id}", s.topicPage)
	h.HandleFunc("POST /faq/t/{id}", s.topicEdit)
	h.Handle("GET /static/", staticHandler)
	h.HandleFunc("/", s.notFound)
	s.homeMux = h

	// Routes on a group's own host. The group comes from the Host header,
	// so paths are short: travato.nfb.group/p/123.
	g := http.NewServeMux()
	g.HandleFunc("GET /{$}", s.groupHome)
	g.HandleFunc("GET /about", s.groupAbout)
	g.HandleFunc("POST /join", s.groupJoin)
	g.HandleFunc("GET /submit", s.submitForm)
	g.HandleFunc("POST /submit", s.submitPost)
	g.HandleFunc("GET /p/{id}", s.postPage)
	g.HandleFunc("GET /p/{id}/edit", s.editPostForm)
	g.HandleFunc("POST /p/{id}/edit", s.editPost)
	g.HandleFunc("POST /p/{id}/delete", s.deletePost)
	g.HandleFunc("POST /p/{id}/restore", s.restorePost)
	g.HandleFunc("POST /p/{id}/comment", s.addComment)
	g.HandleFunc("GET /c/{id}/edit", s.editCommentForm)
	g.HandleFunc("POST /c/{id}/edit", s.editComment)
	g.HandleFunc("POST /c/{id}/delete", s.deleteComment)
	g.HandleFunc("POST /c/{id}/restore", s.restoreComment)
	g.HandleFunc("POST /p/{id}/link", s.linkPost)
	g.HandleFunc("POST /p/{id}/unlink", s.unlinkPost)
	g.HandleFunc("POST /p/{id}/move", s.movePost)
	g.HandleFunc("POST /p/{id}/moveout", s.moveOutPost)
	g.HandleFunc("GET /search", s.searchPage)
	g.HandleFunc("POST /ask", s.ask)
	g.HandleFunc("POST /ask/feedback", s.askFeedback)
	g.HandleFunc("GET /similar", s.similar)
	g.HandleFunc("GET /settings", s.settingsForm)
	g.HandleFunc("POST /settings", s.settingsSave)
	g.HandleFunc("GET /how-it-works", s.howItWorks)
	// M3: the FAQ, topics, outside sources, and thread arrangement.
	g.HandleFunc("GET /faq", s.faqPage)
	g.HandleFunc("GET /faq/e/{id}", s.entryPage)
	g.HandleFunc("GET /faq/e/{id}/edit", s.entryEditForm)
	g.HandleFunc("POST /faq/e/{id}/edit", s.entryEdit)
	g.HandleFunc("GET /faq/e/{id}/history", s.entryHistory)
	g.HandleFunc("POST /faq/e/{id}/rollback", s.entryRollback)
	g.HandleFunc("POST /faq/e/{id}/comment", s.entryComment)
	g.HandleFunc("POST /faq/e/{id}/unsource", s.entryUnsource)
	g.HandleFunc("POST /faq/c/{id}/delete", s.entryCommentDelete)
	g.HandleFunc("GET /faq/new", s.entryNewForm)
	g.HandleFunc("POST /faq/new", s.entryNew)
	g.HandleFunc("POST /faq/topics", s.topicCreate)
	g.HandleFunc("GET /faq/t/{id}", s.topicPage)
	g.HandleFunc("POST /faq/t/{id}", s.topicEdit)
	g.HandleFunc("POST /p/{id}/topics", s.postTopics)
	g.HandleFunc("GET /p/{id}/removal", s.archiveRemovalForm)
	g.HandleFunc("POST /p/{id}/removal", s.archiveRemoval)
	g.HandleFunc("POST /nudge/{id}/reverse", s.reverseNudge)
	g.HandleFunc("POST /note/{id}/remove", s.removeNote)
	g.HandleFunc("GET /sources/new", s.sourceNewForm)
	g.HandleFunc("POST /sources/new", s.sourceNew)
	g.HandleFunc("GET /sources/{id}/remove", s.sourceRemovalForm)
	g.HandleFunc("POST /sources/{id}/remove", s.sourceRemoval)
	g.HandleFunc("POST /sources/{id}/detach", s.sourceDetach)
	g.HandleFunc("GET /mod/sources", s.modSources)
	g.HandleFunc("POST /mod/sources/{action}", s.modSourcesAction)
	g.HandleFunc("GET /img/{hash}", s.serveImage)
	g.HandleFunc("GET /img/{hash}/t", s.serveImage)
	g.HandleFunc("GET /login", s.groupLogin)
	g.HandleFunc("POST /logout", s.logout)
	g.Handle("GET /static/", staticHandler)
	g.HandleFunc("/", s.notFound)
	s.groupMux = g

	return s, nil
}

// Handler is the whole site: every request passes through these layers,
// outermost first.
func (s *Server) Handler() http.Handler {
	// CSRF protection: browsers mark cross-site requests (Sec-Fetch-Site,
	// Origin), and Go's CrossOriginProtection refuses any state-changing
	// request that isn't from the same origin. There are no tokens to thread
	// through forms, and it holds because every form posts to the host that
	// served it (sign-in forms live on the primary; group pages link to them
	// rather than embedding them).
	csrf := http.NewCrossOriginProtection()
	return s.logRequests(securityHeaders(!s.Dev, csrf.Handler(http.HandlerFunc(s.route))))
}

// route sends the request to the home site or a group, or redirects it.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	rt, err := s.resolve(r.Host)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	switch rt.kind {
	case siteRedirect:
		// 301 keeps the path, so every old link still lands on the same page.
		http.Redirect(w, r, rt.redirect+r.URL.RequestURI(), http.StatusMovedPermanently)
	case siteHome:
		s.homeMux.ServeHTTP(w, withRoute(r, rt))
	case siteGroup:
		s.groupMux.ServeHTTP(w, withRoute(r, rt))
	default:
		s.render(w, withRoute(r, rt), http.StatusNotFound, "notfound", &page{Title: "No such group"})
	}
}

// loadPages parses each page template together with the shared layout.
func (s *Server) loadPages() error {
	s.pages = map[string]*template.Template{}
	names, err := fs.Glob(webfiles.Files, "templates/*.html")
	if err != nil {
		return err
	}
	for _, n := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if name == "layout" || name == "fragments" {
			continue
		}
		t, err := template.New(name).Funcs(s.templateFuncs()).ParseFS(webfiles.Files, "templates/layout.html", n)
		if err != nil {
			return err
		}
		s.pages[name] = t
	}
	var err2 error
	s.fragments, err2 = template.New("fragments").Funcs(s.templateFuncs()).ParseFS(webfiles.Files, "templates/fragments.html")
	return err2
}

// page is what every template gets.
type page struct {
	Title    string
	User     *store.User
	HomeURL  string // the primary's home page
	LoginURL string // sign-in, coming back to this page
	Group    *store.Group
	Manage   bool // show the group's Settings link
	NoIndex  bool // ask search engines not to list this page
	Error    string
	Data     any // the page's own data
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p *page) {
	t := s.pages[name]
	if t == nil {
		s.serverError(w, r, fmt.Errorf("no template %q", name))
		return
	}
	if p.HomeURL == "" {
		if rt := routeOf(r); rt != nil {
			p.HomeURL = s.primaryURL(rt.primary, "/")
			p.LoginURL = s.primaryURL(rt.primary, "/login?next="+queryEscape(s.currentURL(r)))
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Pages can show who's signed in, so shared caches mustn't keep them.
	w.Header().Set("Cache-Control", "private, no-cache")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout", p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "notfound", &page{Title: "Not found", User: s.user(r)})
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("error: %s %s%s: %v", r.Method, r.Host, r.URL.Path, err)
	http.Error(w, "Something went wrong on our side. Please try again.", http.StatusInternalServerError)
}

// securityHeaders sets the headers every response carries. The CSP allows
// nothing from anywhere else: no CDNs, trackers or third-party scripts, and
// no inline scripts, so injected markup can't run.
func securityHeaders(https bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; "+
			"script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// same-origin: sign-in links carry a token in the path, and this
		// keeps any URL from leaking to another site in a Referer header.
		h.Set("Referrer-Policy", "same-origin")
		if https {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// logRequests logs one line per request. Sign-in link tokens are cut from
// the path first: logs get copied around, and a token in a log is a way in.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		path := r.URL.Path
		if strings.HasPrefix(path, "/link/") {
			path = "/link/…"
		}
		log.Printf("%s %s%s %d %s", r.Method, r.Host, path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}
