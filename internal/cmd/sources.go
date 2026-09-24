package cmd

import (
	"database/sql"
	"errors"
	"net/url"
	"strings"
)

// Outside sources (plan section 5): a short summary of an outside page, in
// our own words, plus the link. The page's text is read once to write the
// summary and thrown away; only the URL, title, date, summary and a hash
// (to spot changes) are kept.
//
// Pages are read only when someone asked for that URL, and only from sites
// a mod allowed (a URL a mod adds is allowed by that act). Facebook is
// never read: a Facebook link comes with a description the person adding
// it writes ("described"), and any other link can be added that way too.

// MaxSummaryLen caps an outside source's summary: two or three sentences.
const MaxSummaryLen = 600

// Source "via" values.
const (
	ViaMember    = "member"
	ViaMod       = "mod"
	ViaSeed      = "seed"
	ViaDescribed = "described"
)

// AddSource adds an outside page, or links an existing one to one more
// place. It returns the source's id (the existing one if the URL is known).
//
//   - Described (a person wrote Summary): shown at once, never read.
//   - A member's link on a site the mods allowed: read and summarized now,
//     shown once summarized.
//   - A member's link on any other site: waits in the mod queue.
//   - A mod's link: read now. A seed link: read now, but waits in the mod
//     queue until approved, so nothing seeded is published unseen.
//   - List: a seed list page, read only for the links on it (same site).
type AddSource struct {
	GroupID  int64
	SourceID int64
	URL      string
	Summary  string // a person's description; set = via "described"
	Via      string
	List     bool
	AddedBy  int64
	PostID   int64 // show it on this post (0 = nowhere yet)
	EntryID  int64 // or cite it in this FAQ entry
	At       int64
}

func (c *AddSource) Apply(a *Applier) (any, error) {
	u, site, err := SourceURL(c.URL)
	if err != nil {
		return nil, err
	}
	summary := strings.Join(strings.Fields(c.Summary), " ")
	via := c.Via
	if summary != "" {
		via = ViaDescribed
		if len(summary) > MaxSummaryLen {
			return nil, Invalid("keep the description under %d characters", MaxSummaryLen)
		}
	} else if IsFacebook(site) {
		return nil, Invalid("Facebook pages can't be read here; add a short description of what the post says")
	}
	var id int64
	err = a.Group(c.GroupID, func(tx *sql.Tx) error {
		id, err = addSource(tx, c.GroupID, c.SourceID, u, site, summary, via, c.List, c.AddedBy, c.PostID, c.EntryID, c.At)
		return err
	})
	return id, err
}

// addSource is AddSource inside a transaction (AddSeeds adds many in one).
func addSource(tx *sql.Tx, groupID, id int64, u, site, summary, via string, list bool, addedBy, post, entry, at int64) (int64, error) {
	var existing int64
	err := tx.QueryRow(`SELECT id FROM sources WHERE url = ?`, u).Scan(&existing)
	switch {
	case err == nil:
		// Known already (perhaps removed on request, which sticks).
		return existing, linkSource(tx, groupID, existing, post, entry, at)
	case !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}
	status, read := "active", false
	switch via {
	case ViaDescribed:
	case ViaMod:
		read = true
	case ViaSeed:
		status, read = "pending", true
	case ViaMember:
		var allowed int
		tx.QueryRow(`SELECT COUNT(*) FROM source_domains WHERE ?1 = domain OR ?1 LIKE '%.' || domain`, site).Scan(&allowed)
		if allowed > 0 {
			read = true
		} else {
			status = "pending"
		}
	default:
		return 0, Invalid("unknown source kind %q", via)
	}
	if _, err := tx.Exec(`INSERT INTO sources (id, url, site, summary, via, added_by, status, is_list, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, u, site, summary, via, nullIfZero(addedBy), status, list, at); err != nil {
		return 0, err
	}
	if read {
		kind := JobSource
		if list {
			kind = JobSeed
		}
		if err := schedule(tx, kind, id, 1, at, at); err != nil {
			return 0, err
		}
	}
	if err := sourceIndex(tx, id); err != nil {
		return 0, err
	}
	return id, linkSource(tx, groupID, id, post, entry, at)
}

// SourceURL checks and tidies a link: http or https, no credentials, no
// fragment. It returns the URL and its site (host without "www.").
func SourceURL(raw string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || len(raw) > 2000 {
		return "", "", Invalid("that doesn't look like a web link (it should start with https://)")
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	site := strings.TrimPrefix(u.Hostname(), "www.")
	return u.String(), site, nil
}

// IsFacebook says whether a site is Facebook's, which is never read (its
// groups need a login, and its terms forbid automated collection).
func IsFacebook(site string) bool {
	for _, d := range []string{"facebook.com", "fb.com", "fb.me", "fb.watch"} {
		if site == d || strings.HasSuffix(site, "."+d) {
			return true
		}
	}
	return false
}

// linkSource places a source on a post or cites it in an entry (either may
// be 0). A new citation is new information for the entry.
func linkSource(tx *sql.Tx, groupID, source, post, entry, at int64) error {
	if post == 0 && entry == 0 {
		return nil
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO source_links (source_id, post_id, faq_entry_id, created_at) VALUES (?, ?, ?, ?)`,
		source, post, entry, at)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 && entry != 0 {
		return entryChanged(tx, entry)
	}
	if post != 0 {
		return syncCombined(tx, groupID, post, 0, at)
	}
	return nil
}

// sourceIndex puts a source in the search index when it's shown (active,
// with a summary), and takes it out otherwise.
func sourceIndex(tx *sql.Tx, id int64) error {
	var title, summary, status string
	var list bool
	if err := tx.QueryRow(`SELECT title, summary, status, is_list FROM sources WHERE id = ?`, id).Scan(&title, &summary, &status, &list); err != nil {
		return notFoundGone(err)
	}
	if status != "active" || summary == "" || list {
		return ftsDel(tx, id)
	}
	return ftsPut(tx, "source", id, 0, title, summary)
}

// sourceChanged tells everything written from a source that it changed:
// FAQ entries citing it are rewritten at the next batch, and notes built
// from it are refreshed once things settle.
func sourceChanged(tx *sql.Tx, id, at int64) error {
	entries, err := queryIDs(tx, `SELECT faq_entry_id FROM source_links WHERE source_id = ? AND faq_entry_id != 0`, id)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := entryChanged(tx, e); err != nil {
			return err
		}
	}
	notes, err := queryIDs(tx, `SELECT note_id FROM note_sources WHERE source_id = ?`, id)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if _, err := tx.Exec(`UPDATE notes SET stale = 1 WHERE id = ?`, n); err != nil {
			return err
		}
		v, err := noteVersion(tx, n)
		if err != nil {
			return err
		}
		if err := schedule(tx, JobNote, n, v, at+QuietPeriod, at); err != nil {
			return err
		}
	}
	return nil
}

// SetSource is a source job's result: the page as read now. Gone means the
// page no longer exists; Problem says why it couldn't be read (blocked by
// robots.txt, not a web page, too big), which mods see in their queue. An
// unchanged page (same hash) just records the check.
type SetSource struct {
	GroupID     int64
	JobID       int64
	Worker      string
	SourceID    int64
	Version     int64
	Title       string
	PublishedAt int64
	Summary     string
	ContentHash string
	Gone        bool
	Problem     string
	At          int64
}

func (c *SetSource) Apply(a *Applier) (any, error) {
	summary := strings.Join(strings.Fields(c.Summary), " ")
	if len(summary) > MaxSummaryLen {
		summary = summary[:MaxSummaryLen]
	}
	title := strings.Join(strings.Fields(c.Title), " ")
	if len(title) > 300 {
		title = title[:300]
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var version int64
		var status, hash string
		var via string
		if err := tx.QueryRow(`SELECT version, status, COALESCE(content_hash, ''), via FROM sources WHERE id = ?`, c.SourceID).
			Scan(&version, &status, &hash, &via); err != nil {
			return notFoundGone(err)
		}
		ok, err := finishJob(tx, c.JobID, c.Worker, c.Version, version, c.At)
		if err != nil || !ok || status == "removed" || via == ViaDescribed {
			return err
		}
		switch {
		case c.Gone:
			if status == "active" {
				status = "gone"
			}
			_, err = tx.Exec(`UPDATE sources SET status = ?, checked_at = ?, problem = '' WHERE id = ?`, status, c.At, c.SourceID)
		case c.Problem != "":
			_, err = tx.Exec(`UPDATE sources SET checked_at = ?, problem = ? WHERE id = ?`, c.At, c.Problem, c.SourceID)
		case c.ContentHash == hash:
			if status == "gone" {
				status = "active"
			}
			_, err = tx.Exec(`UPDATE sources SET status = ?, checked_at = ?, problem = '' WHERE id = ?`, status, c.At, c.SourceID)
		default:
			if status == "gone" {
				status = "active"
			}
			_, err = tx.Exec(`UPDATE sources SET status = ?, title = ?, published_at = ?, summary = ?, content_hash = ?,
				checked_at = ?, problem = '' WHERE id = ?`, status, title, nullIfZero(c.PublishedAt), summary, c.ContentHash, c.At, c.SourceID)
			if err == nil {
				err = sourceChanged(tx, c.SourceID, c.At)
			}
		}
		if err != nil {
			return err
		}
		return sourceIndex(tx, c.SourceID)
	})
}

// SeedURL is one link found on a seed list page.
type SeedURL struct {
	ID  int64
	URL string
}

// AddSeeds is a seed job's result: the links on a list page (same site
// only; the job never follows links further). Each becomes a seed source,
// read and summarized, and waiting for a mod's approval.
type AddSeeds struct {
	GroupID int64
	JobID   int64
	Worker  string
	ListID  int64
	Version int64
	URLs    []SeedURL
	Problem string
	At      int64
}

func (c *AddSeeds) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var version int64
		if err := tx.QueryRow(`SELECT version FROM sources WHERE id = ?`, c.ListID).Scan(&version); err != nil {
			return notFoundGone(err)
		}
		ok, err := finishJob(tx, c.JobID, c.Worker, c.Version, version, c.At)
		if err != nil || !ok {
			return err
		}
		if _, err := tx.Exec(`UPDATE sources SET checked_at = ?, problem = ? WHERE id = ?`, c.At, c.Problem, c.ListID); err != nil {
			return err
		}
		for i, s := range c.URLs {
			if i == 50 {
				break
			}
			u, site, err := SourceURL(s.URL)
			if err != nil || IsFacebook(site) {
				continue
			}
			if _, err := addSource(tx, c.GroupID, s.ID, u, site, "", ViaSeed, false, 0, 0, 0, c.At); err != nil {
				return err
			}
		}
		return nil
	})
}

// ApproveSources publishes pending sources (mods): the seed queue in bulk,
// or a member's link from a site not yet allowed. One that hasn't been read
// yet is read now, and shows once it's summarized.
type ApproveSources struct {
	GroupID   int64
	SourceIDs []int64
	By        int64
	At        int64
}

func (c *ApproveSources) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		for _, id := range c.SourceIDs {
			var status, summary string
			var version int64
			err := tx.QueryRow(`SELECT status, summary, version FROM sources WHERE id = ? AND is_list = 0`, id).Scan(&status, &summary, &version)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if status != "pending" {
				continue
			}
			if _, err := tx.Exec(`UPDATE sources SET status = 'active' WHERE id = ?`, id); err != nil {
				return err
			}
			if summary == "" {
				if err := schedule(tx, JobSource, id, version, c.At, c.At); err != nil {
					return err
				}
			}
			if err := sourceIndex(tx, id); err != nil {
				return err
			}
			if err := modLog(tx, c.By, "source_approve", "source", id, "", c.At); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveSource takes a source down: a mod's call, or a removal request from
// anyone (the "request removal" link on every outside source). Requests are
// honored at once, with no argument (plan section 5). The summary is erased
// too, and the URL row stays so the same page isn't simply added again; a
// mod can restore it, which reads the page afresh.
type RemoveSource struct {
	GroupID  int64
	SourceID int64
	By       int64 // 0 for a request from someone signed out
	Request  bool
	Reason   string
	At       int64
}

func (c *RemoveSource) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE sources SET status = 'removed', summary = '', title = '', content_hash = NULL
			WHERE id = ? AND status != 'removed'`, c.SourceID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if err := ftsDel(tx, c.SourceID); err != nil {
			return err
		}
		if err := sourceChanged(tx, c.SourceID, c.At); err != nil {
			return err
		}
		action := "source_remove"
		if c.Request {
			action = "removal_request"
		}
		return modLog(tx, c.By, action, "source", c.SourceID, c.Reason, c.At)
	})
}

// RestoreSource undoes a removal (mods). The page is read again, since its
// summary was erased.
type RestoreSource struct {
	GroupID  int64
	SourceID int64
	By       int64
	At       int64
}

func (c *RestoreSource) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var via string
		var version int64
		if err := tx.QueryRow(`SELECT via, version FROM sources WHERE id = ? AND status = 'removed'`, c.SourceID).Scan(&via, &version); err != nil {
			return notFoundGone(err)
		}
		if via == ViaDescribed {
			return Invalid("its description was erased; add the link again with a new one")
		}
		if _, err := tx.Exec(`UPDATE sources SET status = 'active', version = version + 1 WHERE id = ?`, c.SourceID); err != nil {
			return err
		}
		if err := schedule(tx, JobSource, c.SourceID, version+1, c.At, c.At); err != nil {
			return err
		}
		return modLog(tx, c.By, "source_restore", "source", c.SourceID, "", c.At)
	})
}

// AllowDomain adds or removes a site mods allow pages to be read from.
type AllowDomain struct {
	GroupID int64
	Domain  string
	Allow   bool
	By      int64
	At      int64
}

func (c *AllowDomain) Apply(a *Applier) (any, error) {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(c.Domain)), "www.")
	if err := ValidDomain(d); err != nil {
		return nil, err
	}
	if IsFacebook(d) {
		return nil, Invalid("Facebook pages are never read; members add them with a description instead")
	}
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if !c.Allow {
			_, err := tx.Exec(`DELETE FROM source_domains WHERE domain = ?`, d)
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO source_domains (domain, added_by, created_at) VALUES (?, ?, ?)`, d, c.By, c.At); err != nil {
			return err
		}
		return modLog(tx, c.By, "source_domain_allow", "group", 0, d, c.At)
	})
}

// DetachSource takes a source off a post or out of an entry (mods, or the
// post's author for their own post).
type DetachSource struct {
	GroupID  int64
	SourceID int64
	PostID   int64
	EntryID  int64
	By       int64
	At       int64
}

func (c *DetachSource) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM source_links WHERE source_id = ? AND post_id = ? AND faq_entry_id = ?`,
			c.SourceID, c.PostID, c.EntryID); err != nil {
			return err
		}
		if c.EntryID != 0 {
			return entryChanged(tx, c.EntryID)
		}
		return syncCombined(tx, c.GroupID, c.PostID, 0, c.At)
	})
}

// RequestArchiveRemoval takes down an imported archive thread on request,
// the same way as an outside source: at once, no argument, logged, and a
// mod can restore it.
type RequestArchiveRemoval struct {
	GroupID int64
	PostID  int64
	By      int64
	Reason  string
	At      int64
}

func (c *RequestArchiveRemoval) Apply(a *Applier) (any, error) {
	return nil, a.Group(c.GroupID, func(tx *sql.Tx) error {
		var origin, status string
		if err := tx.QueryRow(`SELECT origin, status FROM posts WHERE id = ?`, c.PostID).Scan(&origin, &status); err != nil {
			return notFoundGone(err)
		}
		if origin != "archive" {
			return Invalid("only archive threads are removed on request; report other posts to the mods")
		}
		if status == "removed" || status == "deleted" {
			return nil
		}
		if _, err := tx.Exec(`UPDATE posts SET status = 'removed', removed_reason = 'removal request', deleted_at = ?,
			purge_after = ? WHERE id = ?`, c.At, c.At+ModRemoveKeep, c.PostID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM search_fts WHERE post_id = ?`, c.PostID); err != nil {
			return err
		}
		if err := threadChanged(tx, c.GroupID, c.PostID, c.At); err != nil {
			return err
		}
		return modLog(tx, c.By, "removal_request", "post", c.PostID, c.Reason, c.At)
	})
}

func queryIDs(tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
