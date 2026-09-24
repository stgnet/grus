package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stgnet/grus/internal/cmd"
	"github.com/stgnet/grus/internal/ids"
)

func TestConvertThread(t *testing.T) {
	const file = `{"threads": [{
		"ref": "fb-123", "permalink": "https://www.facebook.com/groups/x/posts/123",
		"author": "Jane Doe", "created": "2021-06-03",
		"body": "\n  Fridge won't cool on propane\nAny ideas @Bob Smith? Jane Doe here.",
		"comments": [
			{"author": "Bob Smith", "created": 1622764800, "body": "Clean the burner, jane doe.",
			 "replies": [{"author": "Jane Doe", "created": "2021-06-05T10:00:00Z", "body": "Thanks Bob Smith!"}]},
			{"author": "Al", "body": "   "}
		]}]}`
	var f archiveFile
	if err := json.Unmarshal([]byte(file), &f); err != nil {
		t.Fatal(err)
	}
	names := namesRE(authorNames(f.Threads))
	noPhotos := func(string) (*cmd.Image, error) { return nil, nil }
	p, err := convertThread(f.Threads[0], names, ids.New(1023), noPhotos)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Fridge won't cool on propane" {
		t.Errorf("title %q", p.Title)
	}
	if strings.HasPrefix(p.Body, "Fridge") {
		t.Errorf("title repeated in the body: %q", p.Body)
	}
	all := p.Title + p.Body
	for _, c := range p.Comments {
		all += c.Body
	}
	for _, name := range []string{"Jane", "Bob", "jane"} {
		if strings.Contains(all, name) {
			t.Errorf("%q survived scrubbing: %s", name, all)
		}
	}
	// The blank comment is dropped; the reply points at its parent.
	if len(p.Comments) != 2 || p.Comments[1].ParentRef != p.Comments[0].OriginRef {
		t.Fatalf("comments: %+v", p.Comments)
	}
	if p.Comments[0].OriginRef != "fb-123/1" || p.Comments[1].OriginRef != "fb-123/1.1" {
		t.Errorf("refs %q, %q", p.Comments[0].OriginRef, p.Comments[1].OriginRef)
	}
	if p.LastActivity != 1622887200 { // the reply's time
		t.Errorf("last activity %d", p.LastActivity)
	}
	if p.Permalink == "" {
		t.Error("permalink dropped")
	}

	// A permalink that isn't a web link is dropped, not rendered.
	f.Threads[0].Permalink = "javascript:alert(1)"
	p, _ = convertThread(f.Threads[0], names, ids.New(1023), noPhotos)
	if p.Permalink != "" {
		t.Error("non-web permalink kept")
	}

	// Short "names" aren't scrubbed, and scrubbing is by whole word.
	if got := scrub("Albert fixed the Bobcat", namesRE([]string{"Bob"})); got != "Albert fixed the Bobcat" {
		t.Errorf("scrub ate part of a word: %q", got)
	}
}

func TestArchiveTimeFormats(t *testing.T) {
	for in, want := range map[string]int64{
		`1622764800`:             1622764800,
		`"1622764800"`:           1622764800,
		`"2021-06-04"`:           1622764800,
		`"2021-06-04T00:00:00Z"`: 1622764800,
		`"2021-06-04 00:00:00"`:  1622764800,
	} {
		var a archiveTime
		if err := json.Unmarshal([]byte(in), &a); err != nil || int64(a) != want {
			t.Errorf("%s: got %d, %v", in, a, err)
		}
	}
	var a archiveTime
	if json.Unmarshal([]byte(`"last Tuesday"`), &a) == nil {
		t.Error("nonsense date accepted")
	}
}
