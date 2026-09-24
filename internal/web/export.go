package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// Group export (M8): a group's owner can download everything the group is,
// as one .tar.gz, to keep or to take elsewhere. It holds:
//
//	README.txt      what's in it
//	group.db        the group's SQLite file (posts, comments, versions,
//	                FAQ, members, mod log), as of the download
//	handles.json    {"<user id>": "<handle>"} for every account the file
//	                mentions; no emails, which aren't the group's to give
//	photos/<hash>   every photo, full size (JPEG)
//
// It's built from this node's copy, so it's served by a node that holds
// the group (others pass the request on, like any group page).

const exportReadme = `This is a complete export of a group.

group.db is a SQLite database (open it with the sqlite3 command-line tool,
or DB Browser for SQLite). Posts, comments and their earlier versions,
the FAQ, members and the mod log are all in it. Accounts appear by number;
handles.json gives each number's handle. Photos are in photos/, named by
the hash that group.db's images table refers to them by.
`

func (s *Server) groupExport(w http.ResponseWriter, r *http.Request) {
	c := s.owner(w, r)
	if c == nil {
		return
	}
	tmp, err := os.MkdirTemp(s.Store.Dir(), "export-")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer os.RemoveAll(tmp)
	dbPath := filepath.Join(tmp, "group.db")
	if err := s.Store.CopyFile(c.g.ID, dbPath); err != nil {
		s.serverError(w, r, err)
		return
	}
	ids, err := s.Store.GroupUserIDs(c.g.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	names, err := s.Store.Handles(ids)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	handles := map[string]string{}
	for id, h := range names {
		handles[strconv.FormatInt(id, 10)] = h
	}
	hashes, err := s.Store.GroupBlobHashes(c.g.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	name := fmt.Sprintf("%s-%s.tar.gz", c.g.Slug, s.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	// From here on the response is under way: an error can only cut the
	// download short (the browser reports it failed), not become a page.
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := s.Now()
	add := func(path string, size int64, body io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: size, ModTime: now}); err != nil {
			return err
		}
		_, err := io.Copy(tw, body)
		return err
	}
	addBytes := func(path string, b []byte) error { return add(path, int64(len(b)), bytes.NewReader(b)) }
	err = func() error {
		if err := addBytes("README.txt", []byte(exportReadme)); err != nil {
			return err
		}
		f, err := os.Open(dbPath)
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if err := add("group.db", fi.Size(), f); err != nil {
			return err
		}
		hj, _ := json.MarshalIndent(handles, "", "  ")
		if err := addBytes("handles.json", hj); err != nil {
			return err
		}
		for _, h := range hashes {
			data, err := s.Blobs.Read(h, false)
			if err != nil && s.FetchBlob != nil && s.FetchBlob(c.g.ID, h) == nil {
				data, err = s.Blobs.Read(h, false)
			}
			if err != nil {
				log.Printf("export %s: photo %s missing: %v", c.g.Slug, h, err)
				continue
			}
			if err := addBytes("photos/"+h+".jpg", data); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}()
	if err != nil {
		log.Printf("export %s: %v", c.g.Slug, err)
	}
}
