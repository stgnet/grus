// Command grus runs a Grus node, and has the few operator tools that go
// with it.
//
//	grus serve   -config /etc/grus/grus.conf    run the node
//	grus ca init  -dir /etc/grus/cluster        make the cluster CA (once)
//	grus ca issue -dir /etc/grus/cluster <id>   make a node's certificate
//	grus backup  -config ... -to <dir>          consistent copy of every database
//	grus recover -config ...                    make this node the only voter (disaster runbook)
//	grus import-archive -config ... -group <slug> <file>   load a knowledge base as archive threads
//
// See docs/operations.md for how they fit together.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/stgnet/grus/internal/blob"
	"github.com/stgnet/grus/internal/cluster"
	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/config"
	"github.com/stgnet/grus/internal/ids"
	"github.com/stgnet/grus/internal/mail"
	"github.com/stgnet/grus/internal/store"
	"github.com/stgnet/grus/internal/web"
)

// version is set at build time: go build -ldflags "-X main.version=v0.1.0"
var version = "dev"

func main() {
	log.SetFlags(0) // journald adds timestamps
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "ca":
		err = ca(os.Args[2:])
	case "backup":
		err = backup(os.Args[2:])
	case "recover":
		err = recoverCmd(os.Args[2:])
	case "import-archive":
		err = importArchive(os.Args[2:])
	case "version":
		fmt.Println("grus", version)
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  grus serve   -config /etc/grus/grus.conf
  grus ca init  -dir <dir>
  grus ca issue -dir <dir> <node-id>
  grus backup  -config <file> -to <dir>
  grus recover -config <file>
  grus import-archive -config <file> -group <slug> [-n] <archive.json>
  grus version`)
	os.Exit(2)
}

func loadConfig(args []string, name string, extra func(*flag.FlagSet)) (*config.Config, *flag.FlagSet, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	path := fs.String("config", "/etc/grus/grus.conf", "config file")
	if extra != nil {
		extra(fs)
	}
	fs.Parse(args)
	c, err := config.Load(*path)
	return c, fs, err
}

func clusterOptions(c *config.Config) (cluster.Options, error) {
	t, err := cluster.LoadTLS(c.TLSCA, c.TLSCert, c.TLSKey)
	if err != nil {
		return cluster.Options{}, err
	}
	return cluster.Options{
		ID: c.NodeID, Listen: c.ClusterAddr, Advertise: c.Advertise, TLS: t,
		Bootstrap: c.Bootstrap, Peers: c.Peers,
	}, nil
}

func serve(args []string) error {
	c, _, err := loadConfig(args, "serve", nil)
	if err != nil {
		return err
	}
	st, err := store.Open(c.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

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

	// The first time the cluster runs, the domains table is empty: seed it
	// from the config. After that the table is the truth.
	if err := node.WaitLeader(30 * time.Second); err != nil {
		log.Printf("warning: %v; continuing, waiting for the cluster", err)
	}
	if node.IsLeader() {
		if p, err := st.PrimaryDomain(); err == nil && p == "" {
			if _, err := node.Apply(&cmd.SetPrimaryDomain{Domain: c.PrimaryDomain, At: time.Now().Unix()}); err != nil {
				return fmt.Errorf("seeding primary domain: %w", err)
			}
		}
	}

	go purgeDaily(ctx, node)

	// Photos live on disk beside the databases, outside the Raft log.
	// Every node, including a copy-only Studio, serves them to the others
	// on the cluster port and keeps its own set complete (blobs.go).
	blobs, err := blob.Open(filepath.Join(c.DataDir, "blobs"))
	if err != nil {
		return err
	}
	client := cluster.NewClient(opts.TLS)
	rpc := &http.Server{Handler: cluster.RPCHandler(node, st, blobs), ReadHeaderTimeout: 10 * time.Second}
	// The listener closes with the node (it shares the cluster port with
	// Raft), which ends this Serve; nothing to shut down separately.
	go rpc.Serve(node.RPCListener())
	go syncBlobs(ctx, node, st, blobs, client, c.Peers)
	go gcBlobs(ctx, st, blobs)

	if c.HTTPAddr == "" && c.HTTPSAddr == "" {
		// The Studio: a live full copy, serving no web pages.
		log.Printf("no http_addr or https_addr: running as a copy only")
		<-ctx.Done()
		return nil
	}

	srv, err := web.New(&web.Server{
		Store: st,
		Log:   node,
		IDs:   ids.New(c.NodeNum),
		Mail: &mail.Mailer{Host: c.SMTPHost, Port: c.SMTPPort, User: c.SMTPUser, Pass: c.SMTPPass,
			From: c.MailFrom, Dev: os.Stderr},
		Dev:        c.Dev,
		PortSuffix: portSuffix(c),
		IsOperator: c.IsOperator,
		Blobs:      blobs,
		PushBlob:   pushBlob(node, client, blobs, c.Peers),
	})
	if err != nil {
		return err
	}
	return listen(ctx, c, srv)
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
		log.Printf("dev: serving http on %s (http://%s%s/)", c.HTTPAddr, c.PrimaryDomain, portSuffix(c))
	} else {
		m := srv.CertManager(c.ACMEEmail)
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

// purgeDaily has the leader submit a Purge once a day (and shortly after
// start), which hard-deletes whatever's past its retention on every node.
// Only the leader submits it, so it happens once per day, not once per node.
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
		if !node.IsLeader() || time.Since(last) < 24*time.Hour {
			continue
		}
		if _, err := node.Apply(&cmd.Purge{Before: time.Now().Unix()}); err != nil {
			log.Printf("purge: %v", err)
			continue
		}
		last = time.Now()
	}
}

func ca(args []string) error {
	if len(args) < 1 {
		usage()
	}
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	dir := fs.String("dir", "/etc/grus/cluster", "directory holding ca.crt and ca.key")
	fs.Parse(args[1:])
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	switch args[0] {
	case "init":
		if err := cluster.InitCA(*dir); err != nil {
			return err
		}
		fmt.Printf("made %s/ca.crt and ca.key; keep ca.key somewhere safe and off the nodes\n", *dir)
	case "issue":
		if fs.NArg() != 1 {
			usage()
		}
		id := fs.Arg(0)
		if err := cluster.IssueNodeCert(*dir, id); err != nil {
			return err
		}
		fmt.Printf("made %s/%s.crt and %s.key; copy them and ca.crt to node %s\n", *dir, id, id, id)
	default:
		usage()
	}
	return nil
}

// backup writes a consistent copy of every database to a directory, safe to
// run while the server is running. The Studio runs it daily into a dated
// directory on the NAS (deploy/grus-backup.*).
func backup(args []string) error {
	var to *string
	c, _, err := loadConfig(args, "backup", func(fs *flag.FlagSet) {
		to = fs.String("to", "", "directory to write the copy into (must not exist)")
	})
	if err != nil {
		return err
	}
	if *to == "" {
		return errors.New("backup: -to is required")
	}
	if _, err := os.Stat(*to); err == nil {
		return fmt.Errorf("backup: %s already exists", *to)
	}
	st, err := store.Open(c.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.CopyTo(*to)
}

// recoverCmd is the disaster runbook's key step: with the server stopped,
// make this node the cluster's only voter, keeping its data and log.
func recoverCmd(args []string) error {
	c, _, err := loadConfig(args, "recover", nil)
	if err != nil {
		return err
	}
	st, err := store.Open(c.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	opts, err := clusterOptions(c)
	if err != nil {
		return err
	}
	if err := cluster.Recover(opts, st); err != nil {
		return err
	}
	fmt.Printf("recovered: %s is now the only voter; start it with `grus serve`\n", c.NodeID)
	return nil
}
