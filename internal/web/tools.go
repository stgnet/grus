package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stgnet/grus/internal/store"
	"github.com/stgnet/grus/internal/tools"
)

// The admin page's Tools section (/admin/tools): the operator's tools,
// which used to be commands on the server's command line. There are no
// command-line controls: everything is here.
//
//   - Import an archive: a knowledge base (docs/archive-format.md) loaded
//     into a group as archive threads, from an uploaded .json, or a .zip
//     of the .json and its photos.
//   - Measure a model: before trusting a model with the site, run each kind
//     of AI job and some searches on an uploaded archive, on a node that
//     has a model.
//   - Load test: read a group's public pages hard for a while, and report.
//
// One tool runs at a time, in the background; the page shows what it has
// written so far, and refreshes itself while it runs.

// toolRun is the running (or last) tool.
type toolRun struct {
	mu      sync.Mutex
	name    string
	started time.Time
	running bool
	err     error
	out     bytes.Buffer
}

// Write collects the tool's report, keeping the last 256 KB.
func (t *toolRun) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.out.Write(p)
	if t.out.Len() > 256<<10 {
		keep := t.out.Bytes()[t.out.Len()-128<<10:]
		t.out.Reset()
		t.out.WriteString("…\n")
		t.out.Write(keep)
	}
	return len(p), nil
}

// start runs fn in the background as the current tool, unless one is
// already running.
func (s *Server) startTool(name string, fn func(ctx context.Context, out io.Writer) error, cleanup func()) error {
	t := &s.tool
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return errors.New("a tool is already running: wait for it to finish")
	}
	t.name, t.started, t.running, t.err = name, s.Now(), true, nil
	t.out.Reset()
	t.mu.Unlock()
	go func() {
		// Hours at most: a model measurement on a big archive is the
		// longest thing here.
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		err := fn(ctx, t)
		if cleanup != nil {
			cleanup()
		}
		t.mu.Lock()
		t.running, t.err = false, err
		t.mu.Unlock()
	}()
	return nil
}

type toolsData struct {
	Name     string
	Started  string
	Running  bool
	Err      string
	Output   string
	HasModel bool // some node can measure a model
	Groups   []store.Group
	Form     map[string]string
}

func (s *Server) adminTools(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	s.renderTools(w, r, u, http.StatusOK, "", nil)
}

func (s *Server) renderTools(w http.ResponseWriter, r *http.Request, u *store.User, status int, msg string, form map[string]string) {
	groups, err := s.Store.Groups()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	t := &s.tool
	t.mu.Lock()
	d := toolsData{Name: t.name, Running: t.running, Output: t.out.String(), HasModel: s.Bench != nil,
		Groups: groups, Form: form}
	if !t.started.IsZero() {
		d.Started = t.started.UTC().Format("2006-01-02 15:04 UTC")
	}
	if t.err != nil {
		d.Err = t.err.Error()
	}
	t.mu.Unlock()
	if d.Running {
		// Refresh while it runs, to show more of the report (no scripts).
		w.Header().Set("Refresh", "3")
	}
	s.render(w, r, status, "admin-tools", &page{Title: "Tools", User: u, Error: msg, Data: d})
}

// toolFail shows an error on the tools page.
func (s *Server) toolFail(w http.ResponseWriter, r *http.Request, u *store.User, err error, form map[string]string) {
	s.renderTools(w, r, u, http.StatusBadRequest, capitalize(cmdMessage(err))+".", form)
}

// maxArchiveUpload bounds an uploaded archive (a zip with every photo).
const maxArchiveUpload = 4 << 30

// saveUpload saves the uploaded file field to a new directory in the data
// directory, returning the directory (the caller removes it), the saved
// file's path and its original name. A big upload takes a while, so the
// server's usual read timeout is lifted for it.
func (s *Server) saveUpload(w http.ResponseWriter, r *http.Request, field string) (dir, path, name string, err error) {
	http.NewResponseController(w).SetReadDeadline(s.Now().Add(time.Hour))
	r.Body = http.MaxBytesReader(w, r.Body, maxArchiveUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return "", "", "", fmt.Errorf("the upload didn't arrive whole: %v", err)
	}
	f, fh, err := r.FormFile(field)
	if err != nil {
		return "", "", "", errors.New("choose a file to upload")
	}
	defer f.Close()
	dir, err = os.MkdirTemp(s.Store.Dir(), "tool-")
	if err != nil {
		return "", "", "", err
	}
	path = filepath.Join(dir, "upload")
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err == nil {
		_, err = io.Copy(out, f)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", "", "", err
	}
	return dir, path, fh.Filename, nil
}

// toolImport starts an archive import into a group.
func (s *Server) toolImport(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	dir, path, name, err := s.saveUpload(w, r, "archive")
	form := map[string]string{"group": r.FormValue("group")}
	if err != nil {
		s.toolFail(w, r, u, err, form)
		return
	}
	g, err := s.Store.GroupBySlug(strings.TrimSpace(r.FormValue("group")))
	dry := r.FormValue("dry") == "on"
	if err == nil && g == nil && !dry {
		err = errors.New("choose the group to import into")
	}
	var a *tools.Archive
	if err == nil {
		a, err = tools.UnpackArchive(path, name, dir)
	}
	if err != nil {
		os.RemoveAll(dir)
		s.toolFail(w, r, u, err, form)
		return
	}
	o := tools.ImportOptions{Dry: dry, Apply: s.Log.Apply, IDs: s.IDs}
	if g != nil {
		o.GroupID = g.ID
		o.PutPhoto = func(hash string, full, thumb []byte) error {
			got, err := s.Blobs.Put(full, thumb)
			if err == nil && got != hash {
				err = fmt.Errorf("photo stored as %s, expected %s", got, hash)
			}
			if err == nil && s.PushBlob != nil {
				s.PushBlob(g.ID, hash)
			}
			return err
		}
	}
	label := "Import into " + r.FormValue("group")
	if dry {
		label = "Check an archive"
	}
	err = s.startTool(label, func(ctx context.Context, out io.Writer) error {
		fmt.Fprintf(out, "%s: %d threads\n", name, a.Threads())
		return tools.Import(ctx, a, o, out)
	}, func() { os.RemoveAll(dir) })
	if err != nil {
		os.RemoveAll(dir)
		s.toolFail(w, r, u, err, form)
		return
	}
	http.Redirect(w, r, "/admin/tools", http.StatusSeeOther)
}

// toolBench starts a model measurement, on a node that has a model.
func (s *Server) toolBench(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	if s.Bench == nil {
		s.toolFail(w, r, u, errors.New("no node has a model (ai_url in its config)"), nil)
		return
	}
	dir, path, name, err := s.saveUpload(w, r, "archive")
	form := map[string]string{"model": r.FormValue("model"), "n": r.FormValue("n")}
	if err != nil {
		s.toolFail(w, r, u, err, form)
		return
	}
	o := tools.BenchOptions{Model: strings.TrimSpace(r.FormValue("model"))}
	o.N, _ = strconv.Atoi(r.FormValue("n"))
	var a *tools.Archive
	if o.Model == "" {
		err = errors.New("which model? (its name in Ollama)")
	}
	if err == nil {
		a, err = tools.UnpackArchive(path, name, dir)
	}
	if err == nil {
		if qf, _, qerr := r.FormFile("questions"); qerr == nil {
			err = json.NewDecoder(io.LimitReader(qf, 4<<20)).Decode(&o.Questions)
			qf.Close()
			if err != nil {
				err = fmt.Errorf("the questions file: %v", err)
			}
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		s.toolFail(w, r, u, err, form)
		return
	}
	err = s.startTool("Measure "+o.Model, func(ctx context.Context, out io.Writer) error {
		return s.Bench(ctx, a, o, out)
	}, func() { os.RemoveAll(dir) })
	if err != nil {
		os.RemoveAll(dir)
		s.toolFail(w, r, u, err, form)
		return
	}
	http.Redirect(w, r, "/admin/tools", http.StatusSeeOther)
}

// toolLoad starts a load test against a group's address.
func (s *Server) toolLoad(w http.ResponseWriter, r *http.Request) {
	u := s.operator(w, r)
	if u == nil {
		return
	}
	form := map[string]string{"url": r.FormValue("url"), "c": r.FormValue("c"), "d": r.FormValue("d"), "paths": r.FormValue("paths")}
	base := strings.TrimSpace(r.FormValue("url"))
	conc, _ := strconv.Atoi(r.FormValue("c"))
	secs, _ := strconv.Atoi(r.FormValue("d"))
	switch {
	case !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://"):
		s.toolFail(w, r, u, errors.New("give a group's address, like https://travato.nfb.group"), form)
		return
	case conc < 1 || conc > 200 || secs < 1 || secs > 600:
		s.toolFail(w, r, u, errors.New("1 to 200 visitors, for 1 to 600 seconds"), form)
		return
	}
	paths := tools.SplitPaths(r.FormValue("paths"))
	err := s.startTool("Load test "+base, func(ctx context.Context, out io.Writer) error {
		return tools.LoadTest(ctx, base, conc, time.Duration(secs)*time.Second, paths, out)
	}, nil)
	if err != nil {
		s.toolFail(w, r, u, err, form)
		return
	}
	http.Redirect(w, r, "/admin/tools", http.StatusSeeOther)
}
