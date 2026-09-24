package web

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/stgnet/grus/internal/cmd"
)

// Public profiles (M7): /u/<handle> on the primary domain shows a
// member's handle, photo, bio and when they joined. It never lists their
// groups or posts: a profile must not undo an anonymous post, or show that
// someone is in a private group.

type userPageData struct {
	Handle   string
	Bio      []string // paragraphs
	PhotoURL string
	Since    int64
}

// userPage is a member's public profile.
func (s *Server) userPage(w http.ResponseWriter, r *http.Request) {
	u, err := s.Store.UserByHandle(r.PathValue("handle"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if u == nil || u.SuspendedUntil > s.Now().Unix() {
		s.notFound(w, r)
		return
	}
	d := userPageData{Handle: u.Handle, Since: u.CreatedAt}
	for _, p := range strings.Split(u.Bio, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			d.Bio = append(d.Bio, p)
		}
	}
	if u.Photo != "" {
		d.PhotoURL = "/u/" + u.Handle + "/photo"
	}
	s.render(w, r, http.StatusOK, "user", &page{Title: u.Handle, User: s.user(r), Data: d})
}

// userPhoto serves a member's profile photo (the thumbnail size).
func (s *Server) userPhoto(w http.ResponseWriter, r *http.Request) {
	u, err := s.Store.UserByHandle(r.PathValue("handle"))
	if err != nil || u == nil || u.Photo == "" {
		http.NotFound(w, r)
		return
	}
	f, err := s.Blobs.Open(u.Photo, true)
	if err != nil && s.FetchBlob != nil && s.FetchBlob(0, u.Photo) == nil {
		f, err = s.Blobs.Open(u.Photo, true)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	// The URL stays the same when the photo changes, so it can't be
	// cached forever the way group photos (named by their hash) are.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", "image/jpeg")
	io.Copy(w, f)
}

// profileAbout saves the public side of the settings page: bio and photo.
func (s *Server) profileAbout(w http.ResponseWriter, r *http.Request) {
	u := s.user(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	fail := func(msg string) {
		s.render(w, r, http.StatusBadRequest, "profile", &page{Title: "Your settings", User: u, Error: msg,
			Data: profileData{}})
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPostRequest)
	if err := r.ParseMultipartForm(32 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		fail("That photo was too large.")
		return
	}
	c := &cmd.SetProfile{UserID: u.ID, Bio: r.FormValue("bio"), ClearPhoto: r.FormValue("remove_photo") == "on",
		At: s.Now().Unix()}
	var files []*multipart.FileHeader
	if r.MultipartForm != nil {
		files = r.MultipartForm.File["photo"]
	}
	if len(files) > 0 && !c.ClearPhoto {
		// Group 0: a profile photo belongs to no group, so it goes to
		// every node (they all serve profiles).
		imgs, err := s.storePhotos(0, files[:1])
		if err != nil {
			fail(err.Error())
			return
		}
		if len(imgs) == 1 {
			c.Photo = imgs[0].Hash
		}
	}
	if _, err := s.Log.Apply(c); err != nil {
		if cmd.IsInput(err) {
			fail(capitalize(cmdMessage(err)) + ".")
			return
		}
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/profile?saved=1", http.StatusSeeOther)
}

// profileURL is where an author's name links to: their public profile,
// or "" for an anonymous post, an archive post, or a deleted account.
func (s *Server) profileURL(primary string, names map[int64]string, id int64, anonymous bool) string {
	if anonymous || id == 0 || names[id] == "" {
		return ""
	}
	return s.primaryURL(primary, "/u/"+names[id])
}
