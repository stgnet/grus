package store

import (
	"database/sql"
	"errors"
)

// Source is an outside page: its summary and link, never its text.
type Source struct {
	ID          int64
	URL         string
	Site        string
	Title       string
	PublishedAt int64
	Summary     string
	Via         string
	AddedBy     int64
	Status      string // pending | active | gone | removed
	Problem     string
	CheckedAt   int64
	CreatedAt   int64
	Version     int64
	IsList      bool
	Hash        string // of the text last summarized
}

// Shown says whether a source appears to readers: active, with a summary.
// A page that's gone still shows, marked "no longer available", at the
// bottom of an entry's sources.
func (s *Source) Shown() bool {
	return (s.Status == "active" || s.Status == "gone") && s.Summary != "" && !s.IsList
}

const sourceCols = `id, url, site, title, COALESCE(published_at, 0), summary, via, COALESCE(added_by, 0), status,
	problem, COALESCE(checked_at, 0), created_at, version, is_list, COALESCE(content_hash, '')`

func scanSource(row interface{ Scan(...any) error }) (*Source, error) {
	var s Source
	err := row.Scan(&s.ID, &s.URL, &s.Site, &s.Title, &s.PublishedAt, &s.Summary, &s.Via, &s.AddedBy, &s.Status,
		&s.Problem, &s.CheckedAt, &s.CreatedAt, &s.Version, &s.IsList, &s.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Store) sources(groupID int64, where string, args ...any) ([]Source, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT `+sourceCols+` FROM sources WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *src)
	}
	return out, rows.Err()
}

// Source reads one source.
func (s *Store) Source(groupID, id int64) (*Source, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	return scanSource(db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE id = ?`, id))
}

// SourcesForPost lists the outside pages shown on a post.
func (s *Store) SourcesForPost(groupID, postID int64) ([]Source, error) {
	return s.sources(groupID, `id IN (SELECT source_id FROM source_links WHERE post_id = ?) ORDER BY status = 'gone', id`, postID)
}

// SourcesForEntry lists the outside pages an entry cites.
func (s *Store) SourcesForEntry(groupID, entryID int64) ([]Source, error) {
	return s.sources(groupID, `id IN (SELECT source_id FROM source_links WHERE faq_entry_id = ?) ORDER BY status = 'gone', id`, entryID)
}

// SourceQueue lists what mods have to look at: pending sources (seeded, or
// a member's link from a site not allowed yet), and ones that couldn't be
// read. Oldest first.
func (s *Store) SourceQueue(groupID int64) ([]Source, error) {
	return s.sources(groupID, `is_list = 0 AND (status = 'pending' OR (status = 'active' AND problem != '')) ORDER BY created_at, id LIMIT 200`)
}

// RecentSources lists the newest shown and removed sources, for the mods'
// sources page.
func (s *Store) RecentSources(groupID int64, limit int) ([]Source, error) {
	return s.sources(groupID, `is_list = 0 AND status != 'pending' ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
}

// AllowedDomains lists the sites mods allowed pages to be read from.
func (s *Store) AllowedDomains(groupID int64) ([]string, error) {
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT domain FROM source_domains ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
