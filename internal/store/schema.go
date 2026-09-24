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
}
