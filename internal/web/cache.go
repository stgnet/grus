package web

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// The public-page render cache (M8): what a signed-out visitor sees of a
// public group is the same for every signed-out visitor, so it's rendered
// once and served from memory until something changes.
//
// "Until something changes" needs no invalidation code at all: each cached
// page is stored with the version (store.Version) of the group's file and
// of site.db (handles, the group list and domains live there), and it's
// only used while both are still the same. Any write to either, or a
// rewind that rebuilds it, moves its version on, so the next request
// renders afresh. A page can never be served stale; the
// cost is that a busy group re-renders after each write, which is still
// far fewer renders than one per view.

// maxCachedPages bounds the cache's memory. When it's full, it's simply
// emptied: a public page is a few tens of KB, and refilling costs one
// render per page, so anything cleverer isn't worth its code.
const maxCachedPages = 2000

type pageCache struct {
	mu    sync.Mutex
	pages map[string]*cachedPage
	hits  atomic.Int64 // for tests and the admin page
}

type cachedPage struct {
	site, group int64 // the files' versions (store.Version) it was rendered at
	header      http.Header
	body        []byte
}

// cacheable reports whether a request's page can come from the cache: a
// GET of a page (not a photo or a static file) in a public group, from a
// browser with no session.
//
// "Public" is read from the group's own file, not the copy in site.db's
// group list: the pages themselves decide what to show from the group
// file, so the cache must ask the same source. A one-row read is still far
// cheaper than the render it saves.
func (s *Server) cacheable(r *http.Request, rt *route) bool {
	if r.Method != http.MethodGet || rt.kind != siteGroup || rt.group.Visibility != "public" {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/img/") || strings.HasPrefix(r.URL.Path, "/static/") {
		return false
	}
	if _, err := r.Cookie(sessionCookie); err == nil {
		return false
	}
	st, err := s.Store.GroupSettings(rt.group.ID)
	return err == nil && st != nil && st.Visibility == "public"
}

// serveCached serves a group page through the cache: from memory when a
// copy made at the current indexes is there, otherwise by rendering it with
// next and keeping the result if it's a plain 200 page.
func (s *Server) serveCached(w http.ResponseWriter, r *http.Request, rt *route, next http.Handler) {
	// A copy is current while neither file has changed since it was made.
	siteIdx, groupIdx := s.Store.Version(0), s.Store.Version(rt.group.ID)
	key := r.Host + r.URL.RequestURI()
	s.cache.mu.Lock()
	p := s.cache.pages[key]
	s.cache.mu.Unlock()
	if p != nil && p.site == siteIdx && p.group == groupIdx {
		s.cache.hits.Add(1)
		for k, v := range p.header {
			w.Header()[k] = v
		}
		w.WriteHeader(http.StatusOK)
		w.Write(p.body)
		return
	}

	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	next.ServeHTTP(rec, r)
	if rec.status != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
		w.Header().Get("Set-Cookie") != "" {
		return
	}
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	if s.cache.pages == nil || len(s.cache.pages) >= maxCachedPages {
		s.cache.pages = map[string]*cachedPage{}
	}
	s.cache.pages[key] = &cachedPage{site: siteIdx, group: groupIdx, header: w.Header().Clone(), body: rec.body.Bytes()}
}

// recorder passes a response through while keeping a copy of it.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}
