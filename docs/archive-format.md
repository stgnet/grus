# Importing an existing knowledge base

The admin page's **Tools**, "Import an archive", loads threads from an
existing knowledge base (the Travato group's history) into a group as
**archive threads**:

- Each thread keeps its original date and is locked. An archive thread is a
  record of what was said, not a place to reply.
- The page says "From the Facebook group archive. Names have been removed."
  It also links to the original when the file gives a permalink.
- **No names come across.** Every post and comment shows "Facebook member".
  Every `author` named anywhere in the file is scrubbed from all the text
  (whole words, ignoring case), and so is every @mention.
- Photos are re-encoded like any upload, which strips their metadata.
- Archive threads are indexed for search like any other post.
- Re-running is safe. Threads and comments are matched by their `ref`, so a
  second run updates them in place instead of duplicating them.

## Running it

On the admin page's Tools, upload the file (or a .zip of the file and its
photos), choose the group, and start. The site stays up while it runs, and
the page shows the report as it goes.

"Only check the file" is a dry run. It converts every thread and processes
every photo, which catches bad dates, missing refs and unreadable photos,
but it imports nothing. Otherwise, photos go to the node serving the admin
page, which shares them with the others, and each thread is one write, so
the import replicates like anything else.

## The file

The file is JSON. Photo paths are relative to the JSON file (inside the
zip, when it's uploaded as one).

```json
{
  "threads": [
    {
      "ref": "fb-1234567890",
      "permalink": "https://www.facebook.com/groups/travato/posts/1234567890",
      "title": "",
      "body": "Fridge won't cool on propane\nAny ideas? It works fine on 120V.",
      "author": "Jane Doe",
      "created": "2021-06-03T14:22:00Z",
      "photos": ["photos/1234567890-1.jpg"],
      "comments": [
        {
          "ref": "fb-c-111",
          "author": "Bob Smith",
          "created": 1622764800,
          "body": "Clean the burner tube, it's usually spiders.",
          "photo": "photos/c-111.jpg",
          "replies": [
            {"author": "Jane Doe", "created": "2021-06-05", "body": "That was it, thanks!"}
          ]
        }
      ]
    }
  ]
}
```

| Field | Required | Notes |
|---|---|---|
| `ref` | yes, on threads | The original's unique id. Re-imports match on it. |
| `permalink` | no | Must be an `http(s)://` link. Anything else is dropped. |
| `title` | no | Facebook posts have none, so it defaults to the body's first line, up to 120 characters. |
| `body` | no | Plain text. Line breaks are kept and links become clickable. |
| `author` | no | Never stored. It's only used to scrub that name from all the text. Names shorter than 3 characters are ignored. |
| `created` | yes, on threads | A Unix timestamp, RFC 3339, `2006-01-02 15:04:05`, or `2006-01-02`. A comment without one gets its thread's date. |
| `photos` | no | Photos on the thread, in order. |
| `comments` | no | Top-level comments. |
| `comments[].ref` | no | Without one, a comment is identified by its position (`fb-123/2` is the second comment and `fb-123/2.1` is its first reply). That works as long as the file keeps the same order between runs. |
| `comments[].photo` | no | One photo per comment. |
| `comments[].replies` | no | Replies. A reply to a reply shows under the same top comment, as on the site. |

Blank comments with no photo are skipped. The thread's last activity is
set to its newest comment, so the Active sort puts archive threads in
their historical place, below everything current.
