package store

// Schemas are lists of migrations. Each database records how many it has
// run in SQLite's user_version, and opening a database runs the rest, in
// order, in one transaction each. Never edit a migration that has shipped;
// add a new one to the end.
//
// Migrations run on every node when it opens a file, not through the
// replicated log. That's safe because they're fixed code: every node running
// the same binary ends up with the same schema.
//
// Tables from the plan (section 7) arrive with the milestone that uses them,
// as new migrations. M0 has identity, domains, the group list, and the
// columns for soft delete, revisions and purge that every later table
// follows.
//
// Conventions, from the plan:
//   - Times are UTC Unix seconds. Ids come from internal/ids.
//   - Nothing is hard-deleted when someone deletes it. status + deleted_at +
//     purge_after hide it now; the daily Purge command removes it later
//     unless legal_hold is set.
//   - `applied` holds the index of the last replicated command applied to
//     this file. See internal/cmd/applier.go for why.

var siteMigrations = []string{
	// 1: M0
	`
CREATE TABLE applied (
  id        INTEGER PRIMARY KEY CHECK (id = 1),
  log_index INTEGER NOT NULL
);
INSERT INTO applied VALUES (1, 0);

CREATE TABLE users (
  id              INTEGER PRIMARY KEY,
  handle          TEXT UNIQUE,  -- NULL until the first-sign-in screen picks one
  email           TEXT UNIQUE,  -- NULL once a deleted account is purged
  phone           TEXT UNIQUE,  -- SMS sign-in, later
  photo_key       TEXT,
  bio             TEXT,
  created_at      INTEGER NOT NULL,
  last_seen_at    INTEGER NOT NULL DEFAULT 0, -- updated at most daily, so page views aren't writes
  is_operator     INTEGER NOT NULL DEFAULT 0,
  suspended_until INTEGER NOT NULL DEFAULT 0,
  notify_email    INTEGER NOT NULL DEFAULT 1,
  digest          TEXT    NOT NULL DEFAULT 'off',
  deleted_at      INTEGER,
  purge_after     INTEGER
);

-- Re-applied after any restore from an older snapshot, so an erased
-- account stays erased.
CREATE TABLE erasures (
  user_id      INTEGER PRIMARY KEY,
  requested_at INTEGER NOT NULL,
  completed_at INTEGER
);

-- Sessions are random tokens stored (hashed) and replicated, rather than
-- signed cookies, so "log out everywhere" and suspensions take effect on
-- every node at once.
CREATE TABLE sessions (
  token_hash      TEXT PRIMARY KEY,
  user_id         INTEGER NOT NULL REFERENCES users(id),
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL,
  last_used_at    INTEGER NOT NULL,
  user_agent_hint TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);

-- One row per emailed sign-in. The magic link and the 6-digit code redeem
-- the same row, so using either burns both.
CREATE TABLE login_tokens (
  token_hash TEXT PRIMARY KEY,
  email      TEXT NOT NULL,
  code_hash  TEXT NOT NULL,
  return_url TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  tries      INTEGER NOT NULL DEFAULT 0,
  used_at    INTEGER
);

-- Exactly one primary. Alternates redirect to the same place on the primary.
CREATE TABLE domains (
  name       TEXT PRIMARY KEY,
  role       TEXT NOT NULL CHECK (role IN ('primary', 'alternate')),
  created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX domains_one_primary ON domains(role) WHERE role = 'primary';

-- main_host NULL means the group lives at <slug>.<primary>.
CREATE TABLE groups (
  id         INTEGER PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  main_host  TEXT UNIQUE,
  name       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  status     TEXT NOT NULL DEFAULT 'active'
);

-- Single extra hosts that 301 to a group's main address (group_id set) or
-- to the primary's home page (group_id NULL). Whole alternate domains don't
-- need rows here; see the domains table.
CREATE TABLE host_aliases (
  host       TEXT PRIMARY KEY,
  group_id   INTEGER REFERENCES groups(id),
  created_at INTEGER NOT NULL
);

-- Let's Encrypt certificates and the ACME account key (autocert's cache),
-- replicated so any node can answer any group's HTTPS.
CREATE TABLE certs (
  domain     TEXT PRIMARY KEY,
  pem        BLOB NOT NULL,
  updated_at INTEGER NOT NULL
);
`,
	// 2: M2 AI usage, counted per day per node, for the admin page and the
	// cost limits. No question text, no user ids: just how much work, and
	// how often search had to fall back to plain results (by cause).
	`
CREATE TABLE ai_usage (
  day           TEXT NOT NULL,  -- YYYY-MM-DD, UTC
  node          TEXT NOT NULL,
  purpose       TEXT NOT NULL,  -- ask | check | digest | note | faq | ...
  calls         INTEGER NOT NULL DEFAULT 0,
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  seconds       REAL    NOT NULL DEFAULT 0,
  failures      INTEGER NOT NULL DEFAULT 0, -- for ask: soft fails
  PRIMARY KEY (day, node, purpose)
);
`,
	// 3: M4 visibility and sister groups. visibility, ai_enabled and name
	// are copies of the group's own settings, kept in step by
	// UpdateSettings, so that a node which doesn't hold a group's file (M7)
	// can still list it correctly and apply the sister-group visibility
	// rule to notes that point into it.
	`
ALTER TABLE groups ADD COLUMN visibility TEXT NOT NULL DEFAULT 'public';
ALTER TABLE groups ADD COLUMN ai_enabled INTEGER NOT NULL DEFAULT 1;

-- Sister groups (plan section 2). One row per pair, smaller id first.
-- A pairing is proposed by one group's owner and only takes effect when an
-- owner of the other group accepts it. topics, if set, limits which posts
-- are matched across the pair (words, any of which must appear).
CREATE TABLE group_pairs (
  group_a     INTEGER NOT NULL,
  group_b     INTEGER NOT NULL,
  proposed_by_group INTEGER NOT NULL,
  topics      TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL CHECK (state IN ('proposed', 'active', 'ended')),
  proposed_by INTEGER NOT NULL,
  decided_by  INTEGER,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (group_a, group_b),
  CHECK (group_a < group_b)
);
`,
	// 4: M6 notification preferences. notify_email was created defaulting
	// to on, but the plan has email off until someone asks for it. SQLite
	// can't change a column's default, so new accounts get 0 explicitly
	// (cmd.Login) and this resets the accounts made before M6 (none had
	// been emailed anything but sign-in links yet).
	`
UPDATE users SET notify_email = 0;
ALTER TABLE users ADD COLUMN digest_sent_at INTEGER NOT NULL DEFAULT 0;
`,
	// 5: M7 one log per file: this file's outbox, and the node map.
	`
-- Follow-up commands for other logs, written by a command in the same
-- transaction and relayed by this log's leader (internal/cmd/logs.go).
CREATE TABLE outbox (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  target     INTEGER NOT NULL, -- 0 = site.db's log, else a group's
  command    BLOB NOT NULL,
  created_at INTEGER NOT NULL
);

-- The nodes of the cluster. voter: counts toward quorum for site.db and
-- the groups placed on it (a VPS); full: holds every group, as a
-- non-voter (the Studio).
CREATE TABLE nodes (
  id         TEXT PRIMARY KEY,
  addr       TEXT NOT NULL,     -- cluster host:port
  voter      INTEGER NOT NULL DEFAULT 0,
  full       INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Which nodes hold which group, and whether each votes in the group's
-- log. bootstrap: one of the group's first voters, which starts the
-- group's log if it has none (a host added later joins the running log
-- instead, added by its leader).
CREATE TABLE group_hosts (
  group_id   INTEGER NOT NULL,
  node_id    TEXT NOT NULL,
  voter      INTEGER NOT NULL DEFAULT 0,
  bootstrap  INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (group_id, node_id)
);
`,
}

var groupMigrations = []string{
	// 1: M0
	`
CREATE TABLE applied (
  id        INTEGER PRIMARY KEY CHECK (id = 1),
  log_index INTEGER NOT NULL
);
INSERT INTO applied VALUES (1, 0);

CREATE TABLE settings (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  name            TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  rules           TEXT NOT NULL DEFAULT '',
  visibility      TEXT NOT NULL DEFAULT 'public'
                  CHECK (visibility IN ('public', 'private', 'hidden')),
  join_policy     TEXT NOT NULL DEFAULT 'open'
                  CHECK (join_policy IN ('open', 'approval', 'invite')),
  join_questions  TEXT NOT NULL DEFAULT '',
  allow_anonymous INTEGER NOT NULL DEFAULT 0,
  hold_first_post INTEGER NOT NULL DEFAULT 0,
  allow_indexing  INTEGER NOT NULL DEFAULT 0,
  ai_enabled      INTEGER NOT NULL DEFAULT 1,
  ai_local_only   INTEGER NOT NULL DEFAULT 1,
  public_faq      INTEGER NOT NULL DEFAULT 1,
  vote_threshold  INTEGER NOT NULL DEFAULT 5,
  notify_hidden   INTEGER NOT NULL DEFAULT 1
);

-- user_id refers to site.db's users. There's no cross-file foreign key, but
-- every node holding this file also holds site.db.
CREATE TABLE memberships (
  user_id      INTEGER PRIMARY KEY,
  role         TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'mod', 'member')),
  status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'pending', 'banned')),
  join_answers TEXT NOT NULL DEFAULT '',
  banned_until INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL
);

CREATE TABLE invites (
  code       TEXT PRIMARY KEY,
  created_by INTEGER NOT NULL,
  max_uses   INTEGER NOT NULL,
  uses       INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL
);

CREATE TABLE posts (
  id                INTEGER PRIMARY KEY,
  user_id           INTEGER,  -- NULL only for imported archive posts
  is_anonymous      INTEGER NOT NULL DEFAULT 0,
  title             TEXT NOT NULL,
  body              TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL DEFAULT 'visible'
                    CHECK (status IN ('visible', 'flagged', 'auto_hidden', 'held', 'removed', 'deleted')),
  removed_reason    TEXT,
  pinned            INTEGER NOT NULL DEFAULT 0,
  locked            INTEGER NOT NULL DEFAULT 0,
  score             INTEGER NOT NULL DEFAULT 0,
  comment_count     INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  edited_at         INTEGER,
  last_activity_at  INTEGER NOT NULL,
  continues_post_id INTEGER,
  continued_at      INTEGER,
  origin            TEXT NOT NULL DEFAULT 'native' CHECK (origin IN ('native', 'archive')),
  origin_ref        TEXT,
  digest            TEXT,
  digest_updated_at INTEGER,
  deleted_at        INTEGER,
  deleted_by        INTEGER,
  purge_after       INTEGER,
  legal_hold        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX posts_active ON posts(status, last_activity_at);
CREATE INDEX posts_new    ON posts(status, created_at);

CREATE TABLE comments (
  id             INTEGER PRIMARY KEY,
  post_id        INTEGER NOT NULL REFERENCES posts(id),
  parent_id      INTEGER,  -- one level of replies
  user_id        INTEGER,  -- NULL only for imported archive comments
  is_anonymous   INTEGER NOT NULL DEFAULT 0,
  body           TEXT NOT NULL,
  status         TEXT NOT NULL DEFAULT 'visible'
                 CHECK (status IN ('visible', 'flagged', 'auto_hidden', 'held', 'removed', 'deleted')),
  removed_reason TEXT,
  score          INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL,
  edited_at      INTEGER,
  origin_ref     TEXT,
  deleted_at     INTEGER,
  deleted_by     INTEGER,
  purge_after    INTEGER,
  legal_hold     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX comments_post ON comments(post_id, created_at);

-- Every earlier version of a post or comment, written before the edit
-- changes the row, kept until purge_after.
CREATE TABLE revisions (
  kind        TEXT NOT NULL CHECK (kind IN ('post', 'comment')),
  ref_id      INTEGER NOT NULL,
  version     INTEGER NOT NULL,
  title       TEXT,
  body        TEXT NOT NULL,
  edited_by   INTEGER NOT NULL,
  edited_at   INTEGER NOT NULL,
  purge_after INTEGER NOT NULL,
  PRIMARY KEY (kind, ref_id, version)
);

-- actor_id NULL = automatic (AI or a member vote).
CREATE TABLE mod_log (
  id          INTEGER PRIMARY KEY,
  actor_id    INTEGER,
  action      TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id   INTEGER NOT NULL,
  reason      TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);

-- Full-text index: one row per visible post and comment (later FAQ entries
-- and outside sources too), maintained in the same transaction as every
-- create, edit and status change. Its rowid is the item's own id (ids are
-- unique across kinds), so updating or removing a row is a rowid lookup.
CREATE VIRTUAL TABLE search_fts USING fts5(
  kind UNINDEXED, ref_id UNINDEXED, post_id UNINDEXED, title, body
);
`,
	// 2: M1 photos. Each row places one blob (internal/blob) on a post, or on
	// one comment of it. The same blob can appear in several rows.
	`
CREATE TABLE images (
  id         INTEGER PRIMARY KEY,
  post_id    INTEGER NOT NULL,
  comment_id INTEGER,
  user_id    INTEGER,
  blob_hash  TEXT NOT NULL,
  width      INTEGER NOT NULL,
  height     INTEGER NOT NULL,
  bytes      INTEGER NOT NULL,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX images_post ON images(post_id, sort_order);
CREATE INDEX images_blob ON images(blob_hash);
CREATE UNIQUE INDEX posts_origin    ON posts(origin_ref) WHERE origin_ref IS NOT NULL;
CREATE UNIQUE INDEX comments_origin ON comments(origin_ref) WHERE origin_ref IS NOT NULL;
-- An archive thread's original permalink, shown as "view the original".
ALTER TABLE posts ADD COLUMN origin_url TEXT;
`,
	// 3: M2 search, link notes, digests, and the AI job queue.
	`
-- version counts edits to the post itself; thread_version counts any change
-- in the thread (the post, its comments). AI results carry the version they
-- were computed from, and a result for an older version is thrown away, so
-- a slow worker can never overwrite newer content.
ALTER TABLE posts    ADD COLUMN version        INTEGER NOT NULL DEFAULT 1;
ALTER TABLE posts    ADD COLUMN thread_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE comments ADD COLUMN version        INTEGER NOT NULL DEFAULT 1;
CREATE INDEX posts_continues ON posts(continues_post_id) WHERE continues_post_id IS NOT NULL;

-- That two posts are about the same thing. The text shown on each side is a
-- note (below). Ids grow with time, so the smaller id is the older post and
-- a pair can't be stored twice. A rejected pair stays, so it's never linked
-- again automatically.
CREATE TABLE post_links (
  older_post_id INTEGER NOT NULL,
  newer_post_id INTEGER NOT NULL,
  source        TEXT NOT NULL CHECK (source IN ('auto', 'author', 'mod')),
  created_by    INTEGER,
  state         TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'rejected')),
  created_at    INTEGER NOT NULL,
  PRIMARY KEY (older_post_id, newer_post_id),
  CHECK (older_post_id < newer_post_id)
);
CREATE INDEX post_links_newer ON post_links(newer_post_id);

-- Every system-written text in a thread. after_comment_id 0 = the top of
-- the thread. text is '' until a worker first writes it.
CREATE TABLE notes (
  id               INTEGER PRIMARY KEY,
  host_post_id     INTEGER NOT NULL,
  after_comment_id INTEGER NOT NULL DEFAULT 0,
  kind             TEXT NOT NULL CHECK (kind IN ('link', 'summary', 'combined')),
  text             TEXT NOT NULL DEFAULT '',
  stale            INTEGER NOT NULL DEFAULT 1,
  state            TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'removed')),
  removed_by       INTEGER,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);
CREATE INDEX notes_host ON notes(host_post_id, state);

-- What a note is written from. group_id lets a note cite a sister group
-- (M4); 0 in comment_id / source_id means "not a comment" / "not an
-- outside source" (0 rather than NULL, so the primary key works).
CREATE TABLE note_sources (
  note_id    INTEGER NOT NULL,
  group_id   INTEGER NOT NULL,
  post_id    INTEGER NOT NULL,
  comment_id INTEGER NOT NULL DEFAULT 0,
  source_id  INTEGER NOT NULL DEFAULT 0,
  covers     INTEGER NOT NULL DEFAULT 0, -- 1 = a summary folds this comment under itself
  PRIMARY KEY (note_id, group_id, post_id, comment_id, source_id)
);
CREATE INDEX note_sources_post ON note_sources(group_id, post_id);

-- The AI work queue. A job is created by the same command as the change
-- that needs it, so it can't be lost. There's one row per (kind, ref_id):
-- more changes before it runs just push run_after back (debouncing), and a
-- finished job is re-opened, not duplicated. Workers claim jobs through the
-- leader with a lease, so each runs once, and a crashed worker's job is
-- picked up again when its lease runs out.
CREATE TABLE jobs (
  id            INTEGER PRIMARY KEY,
  kind          TEXT NOT NULL,     -- check | digest | note
  ref_id        INTEGER NOT NULL,  -- the post, or the note
  ref_version   INTEGER NOT NULL,
  run_after     INTEGER NOT NULL,
  pending_since INTEGER NOT NULL,  -- when it last went from done to waiting
  attempts      INTEGER NOT NULL DEFAULT 0,
  claimed_by    TEXT,
  lease_until   INTEGER,
  done_at       INTEGER,
  last_run_at   INTEGER,
  last_error    TEXT,
  created_at    INTEGER NOT NULL,
  UNIQUE (kind, ref_id)
);
CREATE INDEX jobs_due ON jobs(done_at, run_after);

-- Only written when someone taps "not what I was looking for" under search
-- results; the link says so. Everything else about a search is counted,
-- never stored.
CREATE TABLE ai_feedback (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL,
  question   TEXT NOT NULL,
  cited_ids  TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
`,
	// 4: M3 the group FAQ, summaries and nudges, topics, outside sources.
	`
-- The FAQ outline. parent_id 0 = a top-level topic. A topic the weekly
-- outline pass merged away keeps its row (merged_into set), so old links
-- to it still land somewhere. locked = a mod named it; the pass leaves it.
CREATE TABLE faq_topics (
  id          INTEGER PRIMARY KEY,
  parent_id   INTEGER NOT NULL DEFAULT 0,
  title       TEXT NOT NULL,
  sort        INTEGER NOT NULL DEFAULT 0,
  locked      INTEGER NOT NULL DEFAULT 0,
  merged_into INTEGER,
  created_at  INTEGER NOT NULL
);

-- One question and what the group's threads say about it. version goes up
-- whenever anything it's written from changes (a source thread, a new
-- source, a comment on the entry); stale says a rewrite is due. locked =
-- a mod froze the text: rewrites become suggestions in the mod queue.
-- updated_by NULL = written by the system; a mod's id when a mod wrote it.
CREATE TABLE faq_entries (
  id         INTEGER PRIMARY KEY,
  topic_id   INTEGER NOT NULL,
  question   TEXT NOT NULL,
  answer     TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'hidden')),
  locked     INTEGER NOT NULL DEFAULT 0,
  stale      INTEGER NOT NULL DEFAULT 0,
  version    INTEGER NOT NULL DEFAULT 1,
  suggestion TEXT,  -- a rewrite proposed for a locked entry
  sort       INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  updated_by INTEGER
);
CREATE INDEX faq_entries_topic ON faq_entries(topic_id, status);

-- The threads an entry is written from. A post asked again later and
-- pointed at the entry becomes a source too, so what it adds flows back.
CREATE TABLE faq_sources (
  entry_id INTEGER NOT NULL,
  post_id  INTEGER NOT NULL,
  PRIMARY KEY (entry_id, post_id)
);
CREATE INDEX faq_sources_post ON faq_sources(post_id);

-- Every earlier version of an entry, so any change can be rolled back.
CREATE TABLE faq_history (
  id         INTEGER PRIMARY KEY,
  entry_id   INTEGER NOT NULL,
  question   TEXT NOT NULL,
  answer     TEXT NOT NULL,
  changed_by INTEGER,  -- NULL = the system
  created_at INTEGER NOT NULL
);
CREATE INDEX faq_history_entry ON faq_history(entry_id, id);

-- Members' comments on an entry ("this is out of date for 2024+ models"):
-- new information for the next rewrite, the same as a new thread.
CREATE TABLE faq_comments (
  id          INTEGER PRIMARY KEY,
  entry_id    INTEGER NOT NULL,
  user_id     INTEGER NOT NULL,
  body        TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'visible' CHECK (status IN ('visible', 'deleted', 'removed')),
  created_at  INTEGER NOT NULL,
  purge_after INTEGER
);
CREATE INDEX faq_comments_entry ON faq_comments(entry_id, created_at);

-- Browse by topic: one to three topics per post. topics_manual = the
-- author or a mod chose them, and the system stops changing them.
CREATE TABLE post_topics (
  post_id  INTEGER NOT NULL,
  topic_id INTEGER NOT NULL,
  source   TEXT NOT NULL CHECK (source IN ('auto', 'author', 'mod')),
  PRIMARY KEY (post_id, topic_id)
);
CREATE INDEX post_topics_topic ON post_topics(topic_id);
ALTER TABLE posts ADD COLUMN topics_manual INTEGER NOT NULL DEFAULT 0;

-- Display-only changes to how a thread is shown (plan section 2, "Nudging
-- order, not content"). Nothing a member wrote changes; each nudge has its
-- reason, and a mod can reverse it (state 'reversed', which also stops the
-- system from making the same nudge again).
--   rank:     target = a comment that carries the answer; shown first
--   tangent:  target = first comment of an off-topic stretch, value = the
--             last one, reason = what it's about ("tire pressure")
--   superseded: target = a comment, value = the later comment that
--             replaces its advice
--   feed_weight: target = a post that repeats a well-answered thread,
--             value = seconds it's treated as older in the Active feed
CREATE TABLE nudges (
  id          INTEGER PRIMARY KEY,
  post_id     INTEGER NOT NULL,  -- the thread it's shown in
  kind        TEXT NOT NULL CHECK (kind IN ('rank', 'tangent', 'superseded', 'feed_weight')),
  target_id   INTEGER NOT NULL,
  value       INTEGER NOT NULL DEFAULT 0,
  reason      TEXT NOT NULL DEFAULT '',
  set_by      INTEGER,           -- NULL = the system
  state       TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'reversed')),
  created_at  INTEGER NOT NULL
);
CREATE INDEX nudges_post ON nudges(post_id, state);
-- feed_weight, denormalized so the Active feed needs no join.
ALTER TABLE posts ADD COLUMN sink INTEGER NOT NULL DEFAULT 0;
CREATE INDEX posts_active_weighted ON posts(status, last_activity_at - sink);

-- Outside pages: a short summary in our own words plus the link, never the
-- page's text. content_hash spots a changed page on the weekly re-check.
-- via: member | mod | seed | described (a person wrote the summary, and
-- the page is never fetched: always so for Facebook).
CREATE TABLE sources (
  id           INTEGER PRIMARY KEY,
  url          TEXT NOT NULL UNIQUE,
  site         TEXT NOT NULL,
  title        TEXT NOT NULL DEFAULT '',
  published_at INTEGER,
  summary      TEXT NOT NULL DEFAULT '',
  content_hash TEXT,
  via          TEXT NOT NULL CHECK (via IN ('member', 'mod', 'seed', 'described')),
  added_by     INTEGER,
  status       TEXT NOT NULL CHECK (status IN ('pending', 'active', 'gone', 'removed')),
  version      INTEGER NOT NULL DEFAULT 1,
  problem      TEXT NOT NULL DEFAULT '',  -- why it couldn't be read, for mods
  is_list      INTEGER NOT NULL DEFAULT 0, -- a seed list page: read for its links, never shown
  checked_at   INTEGER,
  created_at   INTEGER NOT NULL
);

-- Where a source shows: on a post (as an "Elsewhere" card) or as a source
-- of a FAQ entry. 0 = not that kind of place.
CREATE TABLE source_links (
  source_id    INTEGER NOT NULL,
  post_id      INTEGER NOT NULL DEFAULT 0,
  faq_entry_id INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  PRIMARY KEY (source_id, post_id, faq_entry_id)
);
CREATE INDEX source_links_post  ON source_links(post_id);
CREATE INDEX source_links_entry ON source_links(faq_entry_id);

-- The sites a mod allowed pages to be read from. The fetcher reads nothing
-- else, and never follows links on its own.
CREATE TABLE source_domains (
  domain     TEXT PRIMARY KEY,
  added_by   INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
`,
	// 5: M4 invites, anonymous reveals, and sister-group links.
	`
ALTER TABLE invites ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE invites ADD COLUMN revoked    INTEGER NOT NULL DEFAULT 0;

-- A link between a post here and a post in a sister group. Both groups
-- keep a row (each from its own side), so either can remember a mod's
-- rejection without reading the other's file. Whether a note shows on
-- each side is decided by the visibility rule (auth.CanCite).
CREATE TABLE sister_links (
  post_id     INTEGER NOT NULL,
  other_group INTEGER NOT NULL,
  other_post  INTEGER NOT NULL,
  source      TEXT NOT NULL CHECK (source IN ('auto', 'mod')),
  state       TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'rejected')),
  -- The other post's title and date, for the note's heading, since this
  -- node may not hold the other group's file (M7).
  other_title TEXT NOT NULL DEFAULT '',
  other_date  INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (post_id, other_group, other_post)
);
CREATE INDEX sister_links_other ON sister_links(other_group, other_post);

-- A note written from a post in another group can't sum that post's
-- version from this file, so the version it was last queued for is kept
-- here; a worker sweep raises it when the other thread changes.
ALTER TABLE notes ADD COLUMN ext_version INTEGER NOT NULL DEFAULT 0;
`,
	// 6: M5 moderation: the AI check's flags, member votes and reports,
	// and the examples the check learns each group's standards from.
	`
-- Why an item is flagged or hidden, for the mod queue: by the AI check
-- ('auto'), a member's report ('report'), or a member vote ('vote').
-- ai_cleared = members or a mod said "keep"; the AI doesn't flag it again.
ALTER TABLE posts    ADD COLUMN flagged_by    TEXT;
ALTER TABLE posts    ADD COLUMN flag_category TEXT;
ALTER TABLE posts    ADD COLUMN flag_reason   TEXT;
ALTER TABLE posts    ADD COLUMN ai_cleared    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE comments ADD COLUMN flagged_by    TEXT;
ALTER TABLE comments ADD COLUMN flag_category TEXT;
ALTER TABLE comments ADD COLUMN flag_reason   TEXT;
ALTER TABLE comments ADD COLUMN ai_cleared    INTEGER NOT NULL DEFAULT 0;

-- A member's report of a post or comment. One per member per item.
CREATE TABLE reports (
  kind        TEXT NOT NULL CHECK (kind IN ('post', 'comment')),
  item_id     INTEGER NOT NULL,
  user_id     INTEGER NOT NULL,
  reason      TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  resolved_at INTEGER,
  PRIMARY KEY (kind, item_id, user_id)
);
CREATE INDEX reports_open ON reports(resolved_at, created_at);

-- Keep / Hide votes on flagged items. Only flagged items get votes.
CREATE TABLE flag_votes (
  kind       TEXT NOT NULL,
  item_id    INTEGER NOT NULL,
  user_id    INTEGER NOT NULL,
  vote       TEXT NOT NULL CHECK (vote IN ('keep', 'hide')),
  created_at INTEGER NOT NULL,
  PRIMARY KEY (kind, item_id, user_id)
);

-- Mods' decisions that overrode (or went beyond) the AI check, as short
-- anonymous excerpts. The newest ~20 go into each check as examples of
-- what this group accepts. No author ids: the text and the decision only.
CREATE TABLE mod_examples (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  text       TEXT NOT NULL,
  decision   TEXT NOT NULL CHECK (decision IN ('keep', 'hide')),
  created_at INTEGER NOT NULL
);
`,
	// 7: M6 engagement: "helpful" votes, following posts, notifications.
	`
-- "Helpful" votes on posts and comments: one per member per item, used
-- only for the Top sort. posts.score and comments.score are the counts,
-- kept by the same command.
CREATE TABLE votes (
  kind       TEXT NOT NULL CHECK (kind IN ('post', 'comment')),
  item_id    INTEGER NOT NULL,
  user_id    INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (kind, item_id, user_id)
);

-- Following a post: notified of its new comments and of newer posts
-- linked to it. Authors follow their own posts automatically.
CREATE TABLE follows (
  user_id    INTEGER NOT NULL,
  post_id    INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, post_id)
);
CREATE INDEX follows_post ON follows(post_id);

-- The bell. Rows hold ids only; the text and links are made when shown,
-- so a renamed domain or an edited title is always current.
--   kind: reply | comment | linked | joined | hidden | removed | approved
--   ref_id: the comment (reply, comment, hidden or removed comments), or
--   the newer post (linked); 0 otherwise.
-- AUTOINCREMENT, so every node gives the same row the same id.
CREATE TABLE notifications (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  post_id    INTEGER NOT NULL DEFAULT 0,
  ref_id     INTEGER NOT NULL DEFAULT 0,
  actor_id   INTEGER NOT NULL DEFAULT 0,
  read_at    INTEGER,
  emailed_at INTEGER,
  created_at INTEGER NOT NULL
);
CREATE INDEX notifications_user ON notifications(user_id, read_at);
`,
	// 8: M7 this file's outbox (see site migration 5), and when each
	// member last had this group's digest.
	`
CREATE TABLE outbox (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  target     INTEGER NOT NULL,
  command    BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
ALTER TABLE memberships ADD COLUMN digest_sent_at INTEGER NOT NULL DEFAULT 0;
`,
}
