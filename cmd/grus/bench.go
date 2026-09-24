package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/stgnet/grus/internal/ai"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/config"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/store"
)

// bench-llm measures a model on real content before it's trusted with the
// site (plan section 9, "Capacity: measure, then buy"). It loads an archive
// file (the same format as import-archive) into a throwaway database, runs
// each kind of AI job on a sample of its threads through the same code the
// site uses, and reports jobs per hour and search latency. With a questions
// file it also scores search: for each question, whether a thread you
// expect shows up in the top three cards.
//
//	grus bench-llm -model <name> [-url http://127.0.0.1:11434] [-n 20] [-questions q.json] archive.json
//
// Run it on the Studio with two or three candidate models and compare.

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

func benchLLM(args []string) error {
	fs := flag.NewFlagSet("bench-llm", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:11434", "Ollama address")
	model := fs.String("model", "", "model to test (required)")
	ctxTokens := fs.Int("context", 16384, "context window in tokens")
	n := fs.Int("n", 20, "threads to sample for each job type")
	qfile := fs.String("questions", "", "JSON list of {\"q\": question, \"expect\": [thread refs]}")
	fs.Parse(args)
	if *model == "" || fs.NArg() != 1 {
		return errors.New("usage: grus bench-llm -model <name> [-url ...] [-n 20] [-questions q.json] archive.json")
	}

	// Load the archive into a throwaway database.
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var f archiveFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
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
	gen := ids.New(config.ToolNodeNum)
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
	fmt.Printf("loaded %d threads\n", len(postIDs))

	eng := &ai.Engine{LLM: ai.NewOllama(*url, *model, *ctxTokens), Store: st, Meter: &ai.Meter{}}
	w := &ai.Worker{Engine: eng, Log: lg, IDs: gen, Name: "bench", Now: time.Now}
	ctx := context.Background()

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
			fmt.Printf("  %s %d: %v\n", kind, ref, err)
		}
	}
	var digests, checks, notes benchStat
	fmt.Println("digests…")
	for _, id := range postIDs {
		// Search reads digests, so with questions to ask every thread gets
		// one; only the sample's are timed.
		stat := &digests
		if !slices.Contains(sample, id) {
			if *qfile == "" {
				continue
			}
			stat = &benchStat{}
		}
		run(cmd.JobDigest, id, stat)
	}
	fmt.Println("link checks…")
	for _, id := range sample {
		run(cmd.JobCheck, id, &checks)
	}
	fmt.Println("link notes…")
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

	fmt.Printf("\nmodel %s\n", *model)
	fmt.Printf("  digest:     %s\n", &digests)
	fmt.Printf("  link check: %s\n", &checks)
	fmt.Printf("  link note:  %s\n", &notes)
	if example != "" {
		fmt.Printf("  example note: %s\n", example)
	}

	if *qfile == "" {
		return nil
	}
	qdata, err := os.ReadFile(*qfile)
	if err != nil {
		return err
	}
	var qs []benchQuestion
	if err := json.Unmarshal(qdata, &qs); err != nil {
		return err
	}
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
		fmt.Printf("\nQ: %s (%.1fs)\n", q.Q, d.Seconds())
		if err != nil {
			fmt.Printf("  error: %v\n", err)
			continue
		}
		found := false
		for i, c := range cards {
			p, _ := st.Post(group, c.PostID)
			title := ""
			if p != nil {
				title = p.Title
			}
			fmt.Printf("  %d. %s: %s\n", i+1, title, c.Statement)
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
	fmt.Printf("\nsearch: %s\n", &search)
	if len(lat) > 0 {
		p90 := lat[len(lat)*9/10]
		fmt.Printf("  90%% of searches took under %.1fs (the site gives up at 8s)\n", p90.Seconds())
	}
	if scored > 0 {
		fmt.Printf("  an expected thread was in the top 3 for %d of %d questions\n", hits, scored)
	}
	return nil
}
