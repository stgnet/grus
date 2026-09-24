package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/config"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/img"
)

// import-archive loads an existing knowledge base (Scott's Travato posts)
// into a group as locked archive threads. The file format is in
// docs/archive-format.md.
//
// It runs on a node, beside the running server, and talks to it over the
// cluster port with that node's own certificate: photos go to the node's
// blob store, and each thread is one ImportPost command, which the node
// forwards to the leader. So an import is replicated like any other write,
// and can run while the site is up.
//
// Authors never come across. Any "author" field in the file is used only to
// scrub those names out of the text, along with @mentions.

// archiveFile is the top level of the import file.
type archiveFile struct {
	Threads []archiveThread `json:"threads"`
}

type archiveThread struct {
	Ref       string           `json:"ref"`
	Permalink string           `json:"permalink"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	Author    string           `json:"author"`
	Created   archiveTime      `json:"created"`
	Photos    []string         `json:"photos"`
	Comments  []archiveComment `json:"comments"`
}

type archiveComment struct {
	Ref     string           `json:"ref"`
	Body    string           `json:"body"`
	Author  string           `json:"author"`
	Created archiveTime      `json:"created"`
	Photo   string           `json:"photo"`
	Replies []archiveComment `json:"replies"`
}

// archiveTime accepts the date formats knowledge bases tend to use: a Unix
// timestamp, RFC 3339, or a plain date.
type archiveTime int64

func (t *archiveTime) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*t = archiveTime(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if v, err := time.Parse(layout, s); err == nil {
			*t = archiveTime(v.Unix())
			return nil
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*t = archiveTime(n)
		return nil
	}
	return fmt.Errorf("unrecognized date %q", s)
}

func importArchive(args []string) error {
	var group *string
	var dry *bool
	c, fs, err := loadConfig(args, "import-archive", func(fs *flag.FlagSet) {
		group = fs.String("group", "", "slug of the group to import into")
		dry = fs.Bool("n", false, "check the file and report, without importing")
	})
	if err != nil {
		return err
	}
	if fs.NArg() != 1 || (*group == "" && !*dry) {
		return errors.New("usage: grus import-archive -config <file> -group <slug> [-n] <archive.json>")
	}
	path := fs.Arg(0)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f archiveFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	names := namesRE(authorNames(f.Threads))
	gen := ids.New(config.ToolNodeNum)
	base := filepath.Dir(path)

	// A dry run converts everything (so bad dates, missing refs, and
	// unreadable photos show up) but sends nothing.
	var client *cluster.Client
	var groupID int64
	if !*dry {
		opts, err := clusterOptions(c)
		if err != nil {
			return err
		}
		client = cluster.NewClient(opts.TLS)
		if groupID, err = client.GroupID(c.Advertise, *group); err != nil {
			return err
		}
	}

	var done, failed int
	for i, t := range f.Threads {
		// photo loads, processes, and (unless dry) uploads one photo.
		photo := func(rel string) (*cmd.Image, error) {
			if rel == "" {
				return nil, nil
			}
			return uploadPhoto(client, c.Advertise, filepath.Join(base, rel), gen)
		}
		p, err := convertThread(t, names, gen, photo)
		if err == nil && !*dry {
			p.GroupID = groupID
			_, err = client.Apply(c.Advertise, p)
		}
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "thread %d (%s): %v\n", i+1, t.Ref, err)
			continue
		}
		done++
	}
	verb := "imported"
	if *dry {
		verb = "checked"
	}
	fmt.Printf("%s %d threads, %d failed\n", verb, done, failed)
	if failed > 0 {
		return fmt.Errorf("%d threads failed", failed)
	}
	return nil
}

// uploadPhoto re-encodes a photo like any upload (which strips its
// metadata) and stores it on the local node, which shares it with the
// others. With no client (a dry run) it only processes it.
func uploadPhoto(client *cluster.Client, addr, path string, gen *ids.Generator) (*cmd.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	res, err := img.Process(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	hash := blob.Hash(res.Full)
	if client != nil {
		if err := client.PutBlob(addr, hash, false, res.Full); err != nil {
			return nil, err
		}
		if err := client.PutBlob(addr, hash, true, res.Thumb); err != nil {
			return nil, err
		}
	}
	return &cmd.Image{ID: gen.Next(), Hash: hash, Width: res.Width, Height: res.Height, Bytes: len(res.Full)}, nil
}

// convertThread turns one thread from the file into an ImportPost.
func convertThread(t archiveThread, names *regexp.Regexp, gen *ids.Generator, photo func(string) (*cmd.Image, error)) (*cmd.ImportPost, error) {
	if t.Ref == "" {
		return nil, errors.New(`missing "ref"`)
	}
	if t.Created == 0 {
		return nil, errors.New(`missing "created"`)
	}
	body := scrub(t.Body, names)
	title := strings.TrimSpace(scrub(t.Title, names))
	if title == "" {
		// Facebook posts have no title: use the first line of the body,
		// and drop it from the body when it fit whole (so it isn't shown
		// twice). A cut-off first line stays in the body in full.
		title = firstLine(body, 120)
		if rest, ok := strings.CutPrefix(strings.TrimSpace(body), title); ok {
			body = strings.TrimSpace(rest)
		}
	}
	if title == "" {
		title = "Photo post"
	}
	p := &cmd.ImportPost{
		PostID: gen.Next(), OriginRef: t.Ref, Title: title, Body: body,
		CreatedAt: int64(t.Created), LastActivity: int64(t.Created),
	}
	// Only web links become "View the original"; anything else in the
	// field is dropped rather than rendered.
	if strings.HasPrefix(t.Permalink, "https://") || strings.HasPrefix(t.Permalink, "http://") {
		p.Permalink = t.Permalink
	}
	for _, rel := range t.Photos {
		im, err := photo(rel)
		if err != nil {
			return nil, err
		}
		p.Images = append(p.Images, *im)
	}
	// Comments and replies flatten into one list, parents before replies,
	// which is the order ImportPost needs to find each reply's parent.
	var add func(list []archiveComment, parentRef string) error
	add = func(list []archiveComment, parentRef string) error {
		for j, cm := range list {
			ref := cm.Ref
			if ref == "" {
				// Without its own ref, a comment's place in the thread
				// identifies it, so re-imports still update in place.
				// "fb-123/2" is the second comment, "fb-123/2.1" its first reply.
				if parentRef == "" {
					ref = fmt.Sprintf("%s/%d", t.Ref, j+1)
				} else {
					ref = fmt.Sprintf("%s.%d", parentRef, j+1)
				}
			}
			created := int64(cm.Created)
			if created == 0 {
				created = p.CreatedAt
			}
			im, err := photo(cm.Photo)
			if err != nil {
				return err
			}
			if b := strings.TrimSpace(scrub(cm.Body, names)); b != "" || im != nil {
				p.Comments = append(p.Comments, cmd.ArchiveComment{ID: gen.Next(), OriginRef: ref,
					ParentRef: parentRef, Body: b, CreatedAt: created, Image: im})
			}
			p.LastActivity = max(p.LastActivity, created)
			if err := add(cm.Replies, ref); err != nil {
				return err
			}
		}
		return nil
	}
	if err := add(t.Comments, ""); err != nil {
		return nil, err
	}
	return p, nil
}

// authorNames collects every author named anywhere in the file, longest
// first so "Mary Ann Smith" is scrubbed before "Mary Ann".
func authorNames(threads []archiveThread) []string {
	seen := map[string]bool{}
	var walk func([]archiveComment)
	walk = func(cs []archiveComment) {
		for _, c := range cs {
			seen[strings.TrimSpace(c.Author)] = true
			walk(c.Replies)
		}
	}
	for _, t := range threads {
		seen[strings.TrimSpace(t.Author)] = true
		walk(t.Comments)
	}
	var out []string
	for n := range seen {
		// A one-letter "name" would scrub half the text.
		if len([]rune(n)) >= 3 {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// An @mention: "@" and up to three capitalized words ("@Jane Doe").
var mentionRE = regexp.MustCompile(`@[\p{L}\d_.]+(?:\s+\p{Lu}[\p{L}'-]*){0,2}`)

// scrub removes @mentions and the names of anyone who posted in the
// archive. A mention becomes "@member" so the sentence still reads; a bare
// name (as Facebook shows a reply-to) becomes "a member". Names are
// matched as whole words, ignoring case, so "Bob" doesn't eat "Bobcat".
func scrub(text string, names *regexp.Regexp) string {
	text = mentionRE.ReplaceAllString(text, "@member")
	if names != nil {
		text = names.ReplaceAllString(text, "a member")
	}
	return text
}

// namesRE is one pattern matching any of the names, built once for the
// whole import (thousands of threads times hundreds of names adds up).
func namesRE(names []string) *regexp.Regexp {
	if len(names) == 0 {
		return nil
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	// Alternation tries left to right, so longest-first order matters.
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)\b`)
}

// firstLine is the first non-blank line of text, cut to n characters.
func firstLine(text string, n int) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return excerptRunes(line, n)
		}
	}
	return ""
}

func excerptRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return cut + "…"
}
