package fetch

import (
	"bytes"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// extract pulls a page's title, date, main text and same-site links out of
// its HTML. The main text is the page's <article> (or <main>, or the whole
// <body>), without scripts, menus, headers, footers, sidebars and forms:
// the part a person would actually read.
func extract(p *Page, body []byte, base *url.URL) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return
	}
	var title, ogTitle string
	var article, main, bodyNode *html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if title == "" && n.FirstChild != nil {
					title = n.FirstChild.Data
				}
			case "meta":
				prop := attr(n, "property") + attr(n, "name") + attr(n, "itemprop")
				content := attr(n, "content")
				switch prop {
				case "og:title":
					ogTitle = content
				case "article:published_time", "date", "datePublished", "pubdate", "DC.date.issued":
					if p.Published == 0 {
						p.Published = parseDate(content)
					}
				}
			case "time":
				if p.Published == 0 {
					p.Published = parseDate(attr(n, "datetime"))
				}
			case "article":
				if article == nil {
					article = n
				}
			case "main":
				if main == nil {
					main = n
				}
			case "body":
				bodyNode = n
			case "a":
				if link := sameSite(base, attr(n, "href")); link != "" && len(p.Links) < 200 {
					p.Links = append(p.Links, link)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	p.Links = dedupe(p.Links)
	p.Title = tidy(title)
	if ogTitle != "" {
		p.Title = tidy(ogTitle)
	}
	root := article
	if root == nil {
		root = main
	}
	if root == nil {
		root = bodyNode
	}
	if root == nil {
		return
	}
	var b strings.Builder
	text(&b, root)
	p.Text = tidy(b.String())
}

// skip are elements whose text isn't the page's content.
var skip = map[string]bool{
	"script": true, "style": true, "noscript": true, "nav": true, "header": true, "footer": true,
	"aside": true, "form": true, "svg": true, "iframe": true, "button": true, "select": true, "template": true,
}

// block elements end a line, so words from different paragraphs don't run
// together.
var block = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "section": true, "article": true, "blockquote": true, "pre": true, "td": true,
}

func text(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(n.Data)
		return
	case html.ElementNode:
		if skip[n.Data] {
			return
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		text(b, c)
	}
	if n.Type == html.ElementNode && block[n.Data] {
		b.WriteString("\n")
	}
}

// tidy collapses runs of spaces within lines and drops blank lines.
func tidy(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func parseDate(s string) int64 {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	// Anything else that starts with a date ("2024-05-01 10:00 EST").
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// sameSite resolves a link and keeps it only if it's a web page on the same
// site (ignoring "www."): a seed list's links never lead off the site a mod
// chose.
func sameSite(base *url.URL, href string) string {
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := base.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") != strings.TrimPrefix(strings.ToLower(base.Hostname()), "www.") {
		return ""
	}
	u.Fragment = ""
	if u.String() == base.String() {
		return ""
	}
	return u.String()
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
