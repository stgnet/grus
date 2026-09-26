// Command grus is a Grus node. It runs as a service (systemd on Linux,
// launchd on macOS), which `make install` sets up; it has no other
// commands. Everything an operator adjusts is on the admin page, and
// everything a node needs to start is in its grus.conf, which make install
// writes:
//
//	grus -config /etc/grus/grus.conf
//
// See docs/operations.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/stgnet/grus/internal/ai"
	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/config"
	"github.com/stgnet/grus/internal/fetch"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/store"
	"github.com/stgnet/grus/internal/tools"
	"github.com/stgnet/grus/internal/web"
)

// version is set at build time: go build -ldflags "-X main.version=v0.1.0"
var version = "dev"

func main() {
	log.SetFlags(0) // journald adds timestamps
	path := flag.String("config", "/etc/grus/grus.conf", "config file")
	flag.Parse()
	// A service file from before the binary had only one job says
	// `grus serve -config ...`; read what follows "serve" the same way.
	if flag.Arg(0) == "serve" {
		flag.CommandLine.Parse(flag.Args()[1:])
	}
	if err := serve(*path); err != nil {
		log.Fatal(err)
	}
}

// clusterOptions makes whatever cluster certificates are missing (the CA
// too, on the first node of a new site: one with no join line), then
// loads them.
func clusterOptions(c *config.Config) (cluster.Options, error) {
	if err := cluster.EnsureCerts(c.TLSCA, c.TLSCert, c.TLSKey, c.NodeID, len(c.Join) == 0); err != nil {
		return cluster.Options{}, err
	}
	t, err := cluster.LoadTLS(c.TLSCA, c.TLSCert, c.TLSKey)
	if err != nil {
		return cluster.Options{}, err
	}
	return cluster.Options{
		ID: c.NodeID, Listen: c.ClusterAddr, Advertise: c.Advertise, TLS: t,
		Num: c.NodeNum, Voter: c.Voter, Full: c.Full, Join: c.Join, AI: c.AIURL != "",
		Version: version,
	}, nil
}

func serve(path string) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	st, err := store.Open(c.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	// Raft's files, from before replication without a leader
	// (docs/replication.md, "Upgrading from Raft"). The databases carry
	// everything over; the old log isn't needed.
	if _, err := os.Stat(filepath.Join(c.DataDir, "raft")); err == nil {
		log.Printf("removing %s: Raft's files, no longer used", filepath.Join(c.DataDir, "raft"))
		os.RemoveAll(filepath.Join(c.DataDir, "raft"))
	}

	opts, err := clusterOptions(c)
	if err != nil {
		return err
	}
	node, err := cluster.Start(opts, st)
	if err != nil {
		return err
	}
	defer node.Shutdown()
	log.Printf("grus %s: node %s, cluster port %s", version, c.NodeID, c.Advertise)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The first time the cluster runs (or the first time after upgrading
	// to the global level), the global settings are seeded from this
	// config. SeedGlobal does nothing after the first time: from then on
	// site.db is the truth, and the admin page changes it. Any node may
	// send it; a node that joined has site.db already, seeded.
	seed := &cmd.SeedGlobal{Values: c.Seed.Values, Domains: c.Seed.Domains, MailFrom: c.Seed.MailFrom, At: time.Now().Unix()}
	if _, err := node.Apply(seed); err != nil {
		return fmt.Errorf("seeding the global settings from %s: %w", c.NodeID, err)
	}
	for _, k := range c.Obsolete {
		log.Printf("config: %s no longer does anything and can be deleted", k)
	}
	if domains, err := st.Domains(); err == nil && len(domains) == 0 {
		log.Printf("warning: the site has no domain, so no page can be reached; " +
			"add a domain line to this node's grus.conf and restart (make install asks for one)")
	}

	go purgeDaily(ctx, node)
	go faqNightly(ctx, node, st)
	go exportSettings(ctx, node, st)

	// Photos live on disk beside the databases, outside the Raft logs.
	// Every node serves them to the others on the cluster port and keeps
	// its own set complete for the groups it holds (blobs.go).
	blobs, err := blob.Open(filepath.Join(c.DataDir, "blobs"))
	if err != nil {
		return err
	}
	client := cluster.NewClient(opts.TLS)
	rpcMux := node.RPC()
	cluster.ServeBlobs(rpcMux, blobs)
	go syncBlobs(ctx, node, st, blobs, client)
	go gcBlobs(ctx, st, blobs)

	// One id generator for everything this node creates: two generators
	// with the same node number could hand out the same id.
	gen := ids.New(node.Num())

	// AI (plan section 9). Usage is counted on every node and reported to
	// the replicated daily totals. A node with a model runs the job queue
	// and answers searches; every web node routes searches through a pool
	// of the nodes that have one.
	meter := &ai.Meter{}
	go meter.Report(ctx, node, c.NodeID, 5*time.Minute)
	var engine *ai.Engine
	if c.AIURL != "" {
		// The model and its context window are global settings, read on
		// every call, so every node with a model runs the same one and a
		// change on the admin page applies without a restart.
		llm := ai.NewOllama(c.AIURL, "", 0)
		llm.Current = func() (string, int) {
			g, err := st.Global()
			if err != nil {
				return "", 16384
			}
			return g.AIModel, g.AIContext
		}
		engine = &ai.Engine{LLM: llm, Store: st, Meter: meter}
		engine.Handler(rpcMux, node.SiteStamp)
		// The worker also reads outside pages for their summaries, with a
		// fetcher that only reads what a group allows (internal/fetch).
		w := &ai.Worker{Engine: engine, Log: node, IDs: gen, Name: c.NodeID, Now: time.Now, Fetch: fetch.New()}
		go w.Run(ctx)
		if g, err := st.Global(); err == nil && g.AIModel == "" {
			log.Printf("ai: ai_url is set but no ai_model is set on the admin page; model calls will fail until it is")
		}
		log.Printf("ai: local model at %s", c.AIURL)
	}
	// Searches go to this node's own model and to every other node with
	// one, from the node map (each node records there whether it has one).
	pool := &ai.Pool{Local: engine, Client: client, Applied: node.SiteStamp,
		Workers: func() []string {
			nodes, err := st.Nodes()
			if err != nil {
				return nil
			}
			var addrs []string
			for _, n := range nodes {
				if n.AI && n.ID != c.NodeID && n.Addr != "" {
					addrs = append(addrs, n.Addr)
				}
			}
			return addrs
		}}
	go pool.Poll(ctx)

	srv, err := web.New(&web.Server{
		Store:      st,
		Log:        node,
		IDs:        gen,
		MailLog:    os.Stderr,
		Dev:        c.Dev,
		PortSuffix: portSuffix(c),
		Blobs:      blobs,
		PushBlob:   pushBlob(node, client, blobs),
		FetchBlob:  fetchNow(node, client, blobs),
		Holds:      node.Holds,
		HoldsAll:   node.HoldsAll,
		PassOn:     node.PassOn,
		Leads:      node.OnDutyFor,
		AI:         pool,
		Meter:      meter,
		Bench:      benchOn(c, st, client, rpcMux),
		Version:    version,
	})
	if err != nil {
		return err
	}
	// Other nodes pass this one requests for groups it holds (and they
	// don't), on the cluster port.
	node.ServeWeb(srv.Handler())
	// Notification emails and the digest go out from the node on duty for
	// each group (cluster.Node.OnDutyFor).
	go srv.RunMail(ctx)

	if c.HTTPAddr == "" && c.HTTPSAddr == "" {
		// The Studio: a live full copy, serving pages only when another
		// node passes them on.
		log.Printf("no http_addr or https_addr: serving other nodes only")
		<-ctx.Done()
		return nil
	}
	return listen(ctx, c, srv)
}

// benchRequest is a model measurement sent to a node with a model.
type benchRequest struct {
	Options tools.BenchOptions `json:"options"`
	Archive []byte             `json:"archive"` // the archive's JSON (no photos)
}

// benchOn is how the admin page measures a model (tools.Bench): on this
// node when it runs one, otherwise on a node that does, over the cluster
// port, with its report streamed back. A node with a model also answers
// those requests from the others.
func benchOn(c *config.Config, st *store.Store, client *cluster.Client, rpcMux *http.ServeMux) func(context.Context, *tools.Archive, tools.BenchOptions, io.Writer) error {
	if c.AIURL != "" {
		rpcMux.HandleFunc("POST /tools/bench", func(w http.ResponseWriter, r *http.Request) {
			var req benchRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<30)).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			a, err := tools.ArchiveFromJSON(req.Archive)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			req.Options.URL = c.AIURL
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			out := flushWriter{w, http.NewResponseController(w)}
			if err := tools.Bench(r.Context(), a, req.Options, out); err != nil {
				fmt.Fprintf(out, "\n%v\n", err)
			}
		})
		return func(ctx context.Context, a *tools.Archive, o tools.BenchOptions, out io.Writer) error {
			o.URL = c.AIURL
			return tools.Bench(ctx, a, o, out)
		}
	}
	return func(ctx context.Context, a *tools.Archive, o tools.BenchOptions, out io.Writer) error {
		nodes, err := st.Nodes()
		if err != nil {
			return err
		}
		for _, nd := range nodes {
			if !nd.AI || nd.Addr == "" {
				continue
			}
			data, err := a.JSON()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "measuring on %s\n", nd.ID)
			return client.PostStream(ctx, nd.Addr, "/tools/bench", benchRequest{Options: o, Archive: data}, out)
		}
		return errors.New("no node runs a model: set ai_url in a node's grus.conf (the Studio's)")
	}
}

// flushWriter sends each write to the other node as it's made, so a long
// report arrives as it goes.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	f.rc.Flush()
	return n, err
}

// portSuffix is ":8080" in dev when not on port 80, so links we build work.
func portSuffix(c *config.Config) string {
	if !c.Dev {
		return ""
	}
	_, port, err := net.SplitHostPort(c.HTTPAddr)
	if err != nil || port == "80" {
		return ""
	}
	return ":" + port
}

// listen serves until ctx is cancelled, then shuts down gracefully.
func listen(ctx context.Context, c *config.Config, srv *web.Server) error {
	newServer := func(addr string, h http.Handler) *http.Server {
		// Timeouts keep slow or stalled clients from holding connections open.
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	}
	var servers []*http.Server
	errc := make(chan error, 2)

	if c.Dev {
		s := newServer(c.HTTPAddr, srv.Handler())
		servers = append(servers, s)
		go func() { errc <- s.ListenAndServe() }()
		log.Printf("dev: serving http on %s", c.HTTPAddr)
		if domains, err := srv.Store.Domains(); err == nil {
			for _, d := range domains {
				log.Printf("dev: http://%s%s/", d.Name, portSuffix(c))
			}
		}
	} else {
		m := srv.CertManager()
		s := newServer(c.HTTPSAddr, srv.Handler())
		s.TLSConfig = m.TLSConfig()
		servers = append(servers, s)
		go func() { errc <- s.ListenAndServeTLS("", "") }()
		if c.HTTPAddr != "" {
			// Port 80 answers Let's Encrypt's HTTP challenge and redirects
			// everything else to HTTPS.
			h := newServer(c.HTTPAddr, m.HTTPHandler(nil))
			servers = append(servers, h)
			go func() { errc <- h.ListenAndServe() }()
		}
		log.Printf("serving https on %s", c.HTTPSAddr)
	}

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Printf("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	return nil
}

// purgeDaily has the node on duty (cluster.Node.OnDuty) submit a Purge once
// a day (and shortly after start), which hard-deletes whatever's past its
// retention on every node; it sends each group's part to that group's
// file. Only the node on duty submits it, so it's normally once a day for
// the whole site. When duty moves to another node it may run a second
// time that day, which is harmless: a purge deletes only what's due.
func purgeDaily(ctx context.Context, node *cluster.Node) {
	var last time.Time
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	first := time.After(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-first:
		case <-tick.C:
		}
		if !node.OnDuty() || time.Since(last) < 24*time.Hour {
			continue
		}
		if _, err := node.Apply(&cmd.Purge{Before: time.Now().Unix()}); err != nil {
			log.Printf("purge: %v", err)
			continue
		}
		last = time.Now()
	}
}

// faqNightly has the node on duty queue the nightly FAQ batch (cmd.QueueFAQ) once
// a day at the faq_hour global setting (UTC), when the model is otherwise
// idle, and the weekly outline pass and outside-page re-checks on Sundays.
// The batch only queues jobs; workers do them.
func faqNightly(ctx context.Context, node *cluster.Node, st *store.Store) {
	var lastDay string
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		g, err := st.Global()
		if err != nil {
			continue
		}
		now := time.Now().UTC()
		day := now.Format("2006-01-02")
		if !node.OnDuty() || now.Hour() != g.FAQHour || day == lastDay {
			continue
		}
		weekly := now.Weekday() == time.Sunday
		if _, err := node.Apply(&cmd.QueueFAQ{Weekly: weekly, At: now.Unix()}); err != nil {
			log.Printf("faq batch: %v", err)
			continue
		}
		lastDay = day
	}
}

// exportSettings copies the settings of groups made before settings moved
// to site.db up from each group's own file, once (cmd.ExportSettings).
// The node on duty for the group does it: it's a command on the group's
// file, and needs a node that holds it. It checks every few seconds (one
// small query) until no group is left, which after an upgrade is within
// moments of the nodes hearing from each other, and then stops for good;
// a new site has none to start with.
func exportSettings(ctx context.Context, node *cluster.Node, st *store.Store) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		missing, err := st.GroupsMissingSettings()
		if err == nil && len(missing) == 0 {
			return
		}
		for _, g := range missing {
			if node.OnDutyFor(g) {
				if _, err := node.Apply(&cmd.ExportSettings{GroupID: g, At: time.Now().Unix()}); err != nil {
					log.Printf("copying group %d's settings to site.db: %v", g, err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
