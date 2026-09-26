package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/stgnet/grus/internal/ai"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/store"
)

// Bench measures a model on real content before it's trusted with the
// site (plan section 9, "Capacity: measure, then buy"). It loads an archive
// (the same format as an import) into a throwaway database, runs each kind
// of AI job on a sample of its threads through the same code the site
// uses, and reports jobs per hour and search latency. With questions it
// also scores search: for each question, whether a thread you expect
// shows up in the top three cards.
//
// It runs on a node with a model; the admin page's Tools section sends it
// there. Try two or three candidate models and compare.

// Question is a search to score: a question, and the refs of threads (in
// the archive) that answer it.
type Question = benchQuestion

type benchQuestion struct {
	Q      string   `json:"q"`
	Expect []string `json:"expect"` // refs of threads that answer it
}

type benchStat struct {
	n       int
	total   time.Duration
	worst   time.Duration
	failed  int
	example string
}

func (b *benchStat) add(d time.Duration, err error) {
	if err != nil {
		b.failed++
		return
	}
	b.n++
	b.total += d
	b.worst = max(b.worst, d)
}

func (b *benchStat) String() string {
	if b.n == 0 {
		return fmt.Sprintf("no successful runs (%d failed)", b.failed)
	}
	avg := b.total / time.Duration(b.n)
	return fmt.Sprintf("%d runs, avg %.1fs, worst %.1fs, %.0f per hour, %d failed",
		b.n, avg.Seconds(), b.worst.Seconds(), float64(time.Hour)/float64(avg), b.failed)
}

// BenchOptions says what to measure.
type BenchOptions struct {
	URL       string // the model server (Ollama), e.g. http://127.0.0.1:11434
	Model     string
	Context   int // context window in tokens (0: 16384)
	N         int // threads to sample for each job type (0: 20)
	Questions []Question
}

// Bench runs the measurement and writes the report to out.
func Bench(ctx context.Context, a *Archive, o BenchOptions, out io.Writer) error {
	if o.Model == "" {
		return errors.New("which model?")
	}
	if o.Context == 0 {
		o.Context = 16384
	}
	if o.N == 0 {
		o.N = 20
	}
	n := &o.N
	// Load the archive into a throwaway database.
	f := a.file
	dir, err := os.MkdirTemp("", "grus-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer st.Close()
	lg, err := cluster.NewLocal(st)
	if err != nil {
		return err
	}
	const group = 1
	now := time.Now().Unix()
	if _, err := lg.Apply(&cmd.CreateGroup{GroupID: group, Slug: "bench", Name: "Bench", At: now}); err != nil {
		return err
	}
	// The throwaway database's ids never meet the site's, so any node
	// number will do.
	gen := ids.New(1023)
	names := namesRE(authorNames(f.Threads))
	noPhotos := func(string) (*cmd.Image, error) { return nil, nil }
	var postIDs []int64
	refs := map[string]int64{}
	for _, t := range f.Threads {
		p, err := convertThread(t, names, gen, noPhotos)
		if err != nil {
			continue
		}
		p.GroupID = group
		v, err := lg.Apply(p)
		if err != nil {
			return err
		}
		id := v.(int64)
		postIDs = append(postIDs, id)
		refs[t.Ref] = id
	}
	fmt.Fprintf(out, "loaded %d threads\n", len(postIDs))

	eng := &ai.Engine{LLM: ai.NewOllama(o.URL, o.Model, o.Context), Store: st, Meter: &ai.Meter{}}
	w := &ai.Worker{Engine: eng, Log: lg, IDs: gen, Name: "bench", Now: time.Now}

	// A sample spread across the archive, not just its first threads.
	sample := postIDs
	if len(sample) > *n {
		step := len(postIDs) / *n
		sample = nil
		for i := 0; i < len(postIDs) && len(sample) < *n; i += step {
			sample = append(sample, postIDs[i])
		}
	}

	// Each job runs through the real queue: claim it, run it, apply its
	// result. Digests go first, since matching and search read them.
	run := func(kind string, ref int64, stat *benchStat) {
		db, _ := st.Group(group)
		var j store.Job
		err := db.QueryRow(`SELECT id, kind, ref_id, ref_version FROM jobs WHERE kind = ? AND ref_id = ?`, kind, ref).
			Scan(&j.ID, &j.Kind, &j.RefID, &j.RefVersion)
		if err != nil {
			return
		}
		lg.Apply(&cmd.ClaimJob{GroupID: group, JobID: j.ID, Worker: "bench", At: time.Now().Unix() + cmd.MaxWait})
		start := time.Now()
		res, err := w.RunJob(ctx, group, j)
		stat.add(time.Since(start), err)
		if err == nil {
			lg.Apply(res)
		}
		if err != nil {
			fmt.Fprintf(out, "  %s %d: %v\n", kind, ref, err)
		}
	}
	var digests, checks, notes benchStat
	fmt.Fprintln(out, "digests…")
	for _, id := range postIDs {
		// Search reads digests, so with questions to ask every thread gets
		// one; only the sample's are timed.
		stat := &digests
		if !slices.Contains(sample, id) {
			if len(o.Questions) == 0 {
				continue
			}
			stat = &benchStat{}
		}
		run(cmd.JobDigest, id, stat)
	}
	fmt.Fprintln(out, "link checks…")
	for _, id := range sample {
		run(cmd.JobCheck, id, &checks)
	}
	fmt.Fprintln(out, "link notes…")
	db, _ := st.Group(group)
	rows, err := db.Query(`SELECT ref_id FROM jobs WHERE kind = 'note' AND done_at IS NULL LIMIT ?`, *n)
	if err != nil {
		return err
	}
	var noteIDs []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		noteIDs = append(noteIDs, id)
	}
	rows.Close()
	for _, id := range noteIDs {
		run(cmd.JobNote, id, &notes)
	}
	var example string
	db.QueryRow(`SELECT text FROM notes WHERE text != '' LIMIT 1`).Scan(&example)

	fmt.Fprintf(out, "\nmodel %s\n", o.Model)
	fmt.Fprintf(out, "  digest:     %s\n", &digests)
	fmt.Fprintf(out, "  link check: %s\n", &checks)
	fmt.Fprintf(out, "  link note:  %s\n", &notes)
	if example != "" {
		fmt.Fprintf(out, "  example note: %s\n", example)
	}

	if len(o.Questions) == 0 {
		return nil
	}
	qs := o.Questions
	var search benchStat
	hits, scored := 0, 0
	var lat []time.Duration
	for _, q := range qs {
		start := time.Now()
		qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		cards, err := eng.Ask(qctx, ai.AskRequest{GroupIDs: []int64{group}, Question: q.Q})
		cancel()
		d := time.Since(start)
		search.add(d, err)
		lat = append(lat, d)
		fmt.Fprintf(out, "\nQ: %s (%.1fs)\n", q.Q, d.Seconds())
		if err != nil {
			fmt.Fprintf(out, "  error: %v\n", err)
			continue
		}
		found := false
		for i, c := range cards {
			p, _ := st.Post(group, c.PostID)
			title := ""
			if p != nil {
				title = p.Title
			}
			fmt.Fprintf(out, "  %d. %s: %s\n", i+1, title, c.Statement)
			for _, ref := range q.Expect {
				if refs[ref] == c.PostID && i < 3 {
					found = true
				}
			}
		}
		if len(q.Expect) > 0 {
			scored++
			if found {
				hits++
			}
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	fmt.Fprintf(out, "\nsearch: %s\n", &search)
	if len(lat) > 0 {
		p90 := lat[len(lat)*9/10]
		fmt.Fprintf(out, "  90%% of searches took under %.1fs (the site gives up at 8s)\n", p90.Seconds())
	}
	if scored > 0 {
		fmt.Fprintf(out, "  an expected thread was in the top 3 for %d of %d questions\n", hits, scored)
	}
	return nil
}
