package web

import (
	"fmt"
	"github.com/stgnet/grus/internal/store"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Members write plain text: no Markdown, nothing to learn or escape wrong.
// On the way out it's HTML-escaped, line breaks are kept, and URLs become
// links. Links to any of our own domains are rewritten to the current
// address of what they point at, so a link typed before a domain change
// still goes straight to the right page (the one exception to "we never
// store our own absolute URLs" is text members type).

var urlRE = regexp.MustCompile(`https?://[^\s<>"']+`)

// renderText turns member text into safe HTML.
func (s *Server) renderText(text string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range urlRE.FindAllStringIndex(text, -1) {
		b.WriteString(html.EscapeString(text[last:m[0]]))
		raw := text[m[0]:m[1]]
		// Trailing punctuation is almost always the sentence's, not the URL's.
		trail := ""
		for len(raw) > 0 && strings.ContainsRune(".,;:!?)]'\"", rune(raw[len(raw)-1])) {
			trail = raw[len(raw)-1:] + trail
			raw = raw[:len(raw)-1]
		}
		href := s.rewriteOwnLink(raw)
		fmt.Fprintf(&b, `<a href="%s" rel="nofollow ugc noopener">%s</a>%s`,
			html.EscapeString(href), html.EscapeString(raw), html.EscapeString(trail))
		last = m[1]
	}
	b.WriteString(html.EscapeString(text[last:]))
	out := strings.ReplaceAll(b.String(), "\r\n", "\n")
	return template.HTML(strings.ReplaceAll(out, "\n", "<br>\n"))
}

// rewriteOwnLink points a link at one of our hosts to where that host now
// redirects. Anything else is returned as it was.
func (s *Server) rewriteOwnLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	rt, err := s.resolve(u.Host)
	if err != nil || rt.kind != siteRedirect {
		return raw
	}
	return rt.redirect + u.RequestURI()
}

// shortDate is how times show on pages: "3h ago" for today, "Jun 3" this
// year, "Jun 3, 2023" before that.
func shortDate(now time.Time, unix int64) string {
	if unix == 0 {
		return ""
	}
	t := time.Unix(unix, 0).UTC()
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("Jan 2, 2006")
	}
}

// excerpt is the first n characters of text on one line, for feed cards.
func excerpt(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len([]rune(text)) <= n {
		return text
	}
	r := []rune(text)[:n]
	if i := strings.LastIndex(string(r), " "); i > n/2 {
		return string(r)[:i] + "…"
	}
	return string(r) + "…"
}

// templateFuncs are available in every page.
func (s *Server) templateFuncs() template.FuncMap {
	return template.FuncMap{
		"text":    s.renderText,
		"date":    func(unix int64) string { return shortDate(s.Now(), unix) },
		"excerpt": excerpt,
		"month":   func(unix int64) string { return time.Unix(unix, 0).UTC().Format("Jan 2006") },
		// dict passes several values to a sub-template: {{template "x" (dict "A" 1 "B" 2)}}
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return fmt.Sprintf("%d %s", n, one)
			}
			return fmt.Sprintf("%d %s", n, many)
		},
		// hasTopic: is this topic among a post's topics? (for checkboxes)
		"hasTopic": func(ts []store.Topic, id int64) bool {
			for _, t := range ts {
				if t.ID == id {
					return true
				}
			}
			return false
		},
	}
}
