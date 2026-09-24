package store

import (
	"strings"
	"unicode"
)

// Full-text search over one group's search_fts (plan section 3, step 2).
// Plain SQLite FTS5 with BM25 ranking: no vector database. The model's job
// in Ask is to supply good search phrases; this part is ordinary search.

// Hit is one thread that matched, best first.
type Hit struct {
	PostID  int64
	Title   string
	Snippet string // the best-matching passage, with [ ] around matched words
	Rank    float64
}

// stopWords are dropped from queries: they match everything and turn a
// question ("has anyone fixed the fridge fan on a 2019?") into noise.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a about after all also am an and any anyone are as at be been but by can
		could did do does doing done for from get got had has have having he her here him his how i if in into is
		it its just know like me my no not of on one or our out so some someone than that the their them then
		there these they this those to too up us was we were what when where which who why will with would you
		your yours thanks thank please help anybody anything everyone does did ive im dont cant has hasnt`) {
		stopWords[w] = true
	}
}

// Terms splits text into lowercased search words, without stop words or
// duplicates.
func Terms(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) < 2 || stopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// FTSQuery builds an FTS5 query from free text: any of the words, each
// quoted so nothing a person types is read as FTS syntax. BM25 ranks threads
// that match more (and rarer) words higher, so OR is right for questions.
// Each phrase in phrases becomes a quoted phrase too.
func FTSQuery(text string, phrases ...string) string {
	var parts []string
	for _, t := range Terms(text) {
		parts = append(parts, `"`+t+`"`)
	}
	for _, p := range phrases {
		words := Terms(p)
		if len(words) > 1 {
			parts = append(parts, `"`+strings.Join(words, " ")+`"`)
		} else if len(words) == 1 {
			parts = append(parts, `"`+words[0]+`"`)
		}
	}
	return strings.Join(parts, " OR ")
}

// Search finds threads in a group matching an FTS query, best first. A
// thread's score adds up its matching post and comments (so a thread that
// discusses the thing beats one that mentions it), with the title weighted
// ten times the body. Only readable threads count: visible, plus flagged
// ones when withFlagged (Ask leaves flagged content out until a vote
// clears it).
func (s *Store) Search(groupID int64, query string, limit int, withFlagged bool) ([]Hit, error) {
	if query == "" {
		return nil, nil
	}
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	statuses := `'visible'`
	if withFlagged {
		statuses = `'visible', 'flagged'`
	}
	// bm25() can only be called in the query that does the MATCH, so the
	// per-row scores are worked out first (MATERIALIZED stops SQLite from
	// folding that step into the outer query) and summed after.
	rows, err := db.Query(`WITH m AS MATERIALIZED (
		  SELECT post_id, bm25(search_fts, 0, 0, 0, 10.0, 1.0) AS score FROM search_fts WHERE search_fts MATCH ?)
		SELECT m.post_id, p.title, SUM(m.score) AS rank
		FROM m JOIN posts p ON p.id = m.post_id
		WHERE p.status IN (`+statuses+`)
		GROUP BY m.post_id ORDER BY rank LIMIT ?`, query, limit)
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.PostID, &h.Title, &h.Rank); err != nil {
			rows.Close()
			return nil, err
		}
		hits = append(hits, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The passage that matched best, per thread.
	for i := range hits {
		db.QueryRow(`SELECT snippet(search_fts, 4, '[', ']', '…', 16) FROM search_fts
			WHERE search_fts MATCH ? AND post_id = ? ORDER BY bm25(search_fts, 0, 0, 0, 10.0, 1.0) LIMIT 1`,
			query, hits[i].PostID).Scan(&hits[i].Snippet)
	}
	return hits, nil
}

// KindHit is a match on a FAQ entry or an outside source.
type KindHit struct {
	ID      int64
	Title   string
	Snippet string
	Rank    float64
}

// SearchKind finds FAQ entries (kind "faq") or outside sources (kind
// "source") matching an FTS query, best first. Only shown ones are in the
// index: an entry or source leaves it in the same transaction that hides
// it.
func (s *Store) SearchKind(groupID int64, kind, query string, limit int) ([]KindHit, error) {
	if query == "" {
		return nil, nil
	}
	db, err := s.Group(groupID)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT ref_id, title, snippet(search_fts, 4, '[', ']', '…', 16), bm25(search_fts, 0, 0, 0, 10.0, 1.0) AS r
		FROM search_fts WHERE search_fts MATCH ? AND kind = ? ORDER BY r LIMIT ?`, query, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KindHit
	for rows.Next() {
		var h KindHit
		if err := rows.Scan(&h.ID, &h.Title, &h.Snippet, &h.Rank); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
