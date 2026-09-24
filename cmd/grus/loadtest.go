package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// loadtest reads a group's public pages as fast as a number of signed-out
// visitors can, for a while, and reports how the site held up (plan, M8).
// It's what to run against a new setup before opening it to a group, and
// while killing nodes to see the others carry the load.
//
//	grus loadtest -url https://travato.nfb.group -c 20 -d 30s
//
// It starts from the group's front page and its FAQ, and adds every post
// the front page links to, so it reads the pages people actually read.
// It only reads: nothing it does changes the site.
func loadtest(args []string) error {
	fs := flag.NewFlagSet("loadtest", flag.ExitOnError)
	base := fs.String("url", "", "a group's address, like https://travato.nfb.group")
	conc := fs.Int("c", 10, "visitors reading at once")
	dur := fs.Duration("d", 30*time.Second, "how long to run")
	extra := fs.String("paths", "", "more paths to read, comma-separated (like /search?q=solar)")
	fs.Parse(args)
	if *base == "" {
		return fmt.Errorf("loadtest: -url is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	r, err := runLoad(ctx, strings.TrimSuffix(*base, "/"), *conc, splitPaths(*extra))
	if err != nil {
		return err
	}
	r.print(os.Stdout, *dur)
	return nil
}

func splitPaths(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// postLink finds links to posts on a page.
var postLink = regexp.MustCompile(`href="(/p/[0-9]+)"`)

type loadResult struct {
	paths     []string
	latencies []time.Duration
	statuses  map[int]int
	errors    int
}

// runLoad reads the pages with conc workers until ctx ends.
func runLoad(ctx context.Context, base string, conc int, extra []string) (*loadResult, error) {
	client := &http.Client{Timeout: 30 * time.Second,
		// Redirects are answers too (a moved group); don't follow them.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	front, err := client.Get(base + "/")
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(io.LimitReader(front.Body, 4<<20))
	front.Body.Close()
	if front.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/: %s", base, front.Status)
	}
	paths := append([]string{"/", "/faq"}, extra...)
	seen := map[string]bool{}
	for _, m := range postLink.FindAllStringSubmatch(string(body), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			paths = append(paths, m[1])
		}
	}

	res := &loadResult{paths: paths, statuses: map[int]int{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; ctx.Err() == nil; i++ {
				start := time.Now()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+paths[i%len(paths)], nil)
				resp, err := client.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				took := time.Since(start)
				mu.Lock()
				switch {
				case ctx.Err() != nil:
					// The run ended mid-request: not the site's fault.
				case err != nil:
					res.errors++
				default:
					res.statuses[resp.StatusCode]++
					res.latencies = append(res.latencies, took)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	return res, nil
}

func (r *loadResult) print(w io.Writer, dur time.Duration) {
	sort.Slice(r.latencies, func(i, j int) bool { return r.latencies[i] < r.latencies[j] })
	pct := func(p float64) time.Duration {
		if len(r.latencies) == 0 {
			return 0
		}
		return r.latencies[int(p*float64(len(r.latencies)-1))].Round(time.Millisecond)
	}
	n := len(r.latencies)
	fmt.Fprintf(w, "%d pages (%d distinct), %.0f a second, %d failed to connect\n",
		n, len(r.paths), float64(n)/dur.Seconds(), r.errors)
	var codes []int
	for c := range r.statuses {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	for _, c := range codes {
		fmt.Fprintf(w, "  %d: %d\n", c, r.statuses[c])
	}
	fmt.Fprintf(w, "time per page: half under %s, 90%% under %s, 99%% under %s\n", pct(.5), pct(.9), pct(.99))
}
