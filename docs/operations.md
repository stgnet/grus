# Operations

The starting setup (plan section 8): one VPS serves everything and is the
cluster's only voter, so it's always the leader. The Studio is a non-voting
member with a live full copy, reached over mutual TLS on one port. Losing
the Studio changes nothing for users; losing the VPS is handled by
"Recovering from a lost VPS" below.

```
VPS (n1)    voter, leader     site.db + every group file, HTTPS for every host
   │  mTLS :7946, replicated log ↓
Studio      non-voter         the same files, live; daily backups to the NAS
```

## 1. The cluster CA (once, on your own machine)

```sh
grus ca init  -dir ./cluster
grus ca issue -dir ./cluster n1
grus ca issue -dir ./cluster studio
```

Copy `ca.crt` plus each node's `<id>.crt` and `<id>.key` to that node's
`/etc/grus/cluster/`. Keep `ca.key` off the nodes, somewhere safe: it's what
admits new members. Any certificate it signed is a cluster member, so if a
node's key leaks, make a new CA and reissue every node's certificate.

## 2. The VPS

```sh
go build -o grus ./cmd/grus        # or CGO_ENABLED=0 for a fully static binary
sudo deploy/install-service.sh     # user, directories, unit, example config
sudoedit /etc/grus/grus.conf       # from deploy/grus.conf.example
sudo systemctl enable --now grus
journalctl -u grus -f
```

Open ports 80 and 443 to everyone, and the cluster port (7946) to the
Studio. The first start (with `bootstrap = true`) creates the cluster and
seeds the primary domain from `primary_domain`. Sign in with an `operator`
email to reach `/admin`.

## 3. The Studio

Build for macOS (`GOOS=darwin GOARCH=arm64 go build -o grus ./cmd/grus`),
install to `/usr/local/bin`, write `/etc/grus/grus.conf` from
`deploy/studio.conf.example`, and load `deploy/com.stgnet.grus.plist` with
launchd. Keep its `data_dir` on the Studio's own disk: SQLite needs local
file locking, which network shares don't reliably provide. The NAS gets
the backups.

The Studio needs no join step. The VPS's config lists it (`peer = studio
host:port`), and whenever the VPS is leader it adds any listed peer that's
missing. A brand-new or long-offline Studio receives a snapshot of every
database, then streams changes.

The admin page's Cluster section shows `latest_configuration`, which
should list the Studio as a Nonvoter, and `applied_index`.

## 4. Backups

The Studio's copy is live, not a backup: a mistaken delete reaches it
within a second. Point-in-time copies come from `grus backup`, which writes
a consistent copy of every database while the server keeps running:

```sh
# crontab on the Studio
30 3 * * * /usr/local/bin/grus-backup.sh /Volumes/NAS/grus/backups
```

`deploy/grus-backup.sh` writes `<dir>/YYYY-MM-DD/` and keeps 30 days. To
restore one onto a fresh node, copy its files into the node's `data_dir`
(`site.db`, `groups/`) before the first start, and copy `<dir>/blobs/`
to `data_dir/blobs/`. Photos are content-addressed files that never change,
so the script keeps one shared copy of them with rsync rather than one per
day. A node that's missing photos also fetches them from the others by
itself within a minute.

To load the Travato knowledge base, see [archive-format.md](archive-format.md).

## 5. Recovering from a lost VPS

This was drilled once for M0: `TestReplicateAndRecover` in
`internal/cluster` runs it on every CI run, and it was also run by hand
with two real nodes.

1. Stop grus on the Studio (so its files are still).
2. Copy the Studio's whole `data_dir` (including `raft/`) to the new VPS.
3. Issue the new VPS a certificate if it has a new id
   (`grus ca issue -dir ./cluster n2`), and write its `grus.conf`: its own
   `node_id`, `advertise`, certificate, and `peer = studio ...`.
   `bootstrap` doesn't matter; the copied state already exists.
4. On the new VPS: `grus recover -config /etc/grus/grus.conf`. This rewrites
   the cluster membership so the new VPS is the only voter, keeping every
   database and the log.
5. Start grus on the new VPS, point DNS (`nfb.group`, `*.nfb.group`) at it,
   and start the Studio again. The new leader re-adds the Studio as a
   non-voter and it catches up.

Nothing is lost but writes the Studio hadn't received when the VPS died
(normally under a second's worth).

## 6. Changing the primary domain

1. Set up the new domain's DNS (docs/dns.md), including mail records.
2. On `/admin`, add it as an alternate, then click **Make primary**.
3. Every group's address, the home page and sign-in move at once. The old
   primary becomes an alternate that 301-redirects every old link to the
   same page on the new one.
4. Update `mail_from` in grus.conf and restart, and keep renewing the old
   domain for as long as links to it are out there.

Sessions on the old domain don't carry over yet (people sign in again once);
the invisible cross-domain bounce is planned with custom-domain sign-in in
M7.

## 7. The model (AI)

Everything the model does is optional: with no worker reachable, search
shows plain results with one line saying quick answers aren't available,
and summaries and link notes wait in the job queue until a worker is back.

1. On the Studio, install Ollama and pull two or three candidate models.
2. Pick one on real content (plan section 9, "Choosing the model"):

   ```sh
   grus bench-llm -model <name> -n 20 -questions questions.json travato.json
   ```

   It loads the archive into a throwaway database, runs digests, link
   checks and link notes through the same code the site uses, and reports
   runs per hour. With a questions file (`[{"q": "...", "expect":
   ["ref"]}]`, refs from the archive) it also runs searches and reports how
   often an expected thread made the top three and how long 90% of
   searches took. The site gives up on a search after 8 seconds, so that
   number needs to be comfortably under it.
3. Set `ai_url`, `ai_model` and `ai_context` in the Studio's grus.conf and
   `worker = <studio cluster address>` in the VPS's, then restart both.
4. `/admin` shows, per day, how many model calls each kind of work made,
   tokens in and out, seconds, soft fails by cause, and the job queue.

Group owners turn AI off for their group on the group's Settings page.
With it off, no job runs for that group and search never asks the model
about it. Nothing is ever sent to an outside AI service.

## 8. The FAQ and outside sources

Each night at `faq_hour` (UTC, default 8) the leader queues the FAQ batch:
entries whose threads changed are rewritten, and threads that grew into a
well-answered cluster get a new entry (at most 20 a night per group). On
Sundays the batch also tidies the topic outline and re-reads outside pages
not checked in the past week. The work runs on the worker like any other
job, so with no worker it simply waits.

Mods (or anyone they trust) can edit any entry; a locked entry keeps its
wording and the model only leaves a suggestion beside it. Every change is
kept in the entry's history and can be rolled back.

The FAQ on the bare primary domain is the site-wide one. Its list of groups
and their top topics is automatic; its entries are written by operators on
that page.

Outside pages are read by the worker through a restricted fetcher: public
addresses only (checked again after every redirect), ports 80 and 443,
robots.txt obeyed, and only the page's summary, title and date are kept.
A member's link is read only if its site is on the group's allowed list
(`/mod/sources`); everything else waits for a mod. Facebook links are never
read: they need a description instead. Anyone can ask for a page (or an
imported archive thread) to be taken down, and it is, at once; the mods can
restore it if the request was mistaken.

## 9. Private groups, invites and sister groups

A group's owners set who can read it (public, private, hidden) and how
people join (open, approval with questions, invite only) on its Settings
page. Invite links are made on `/mod/members`, where mods also answer join
requests; a hidden group can only be reached through one. A copy of each
group's name, visibility and "Use AI" setting is kept in `site.db`, so any
node can list groups and apply the sister-group rule without the group's
own file.

Sister groups are paired by their owners (one proposes, the other
accepts). The worker then links related posts across the pair. A note is
only placed where everyone who can read it could also read the post it was
written from: a public group's posts can be cited anywhere, a private
group's nowhere else. The rule is checked when a link is made, again when
a note is shown, and again when a search card is shown, so changing a
group's visibility takes effect at once. Every 15 minutes the worker
checks whether threads that sister notes were written from have changed,
and queues those notes to be rewritten.

## 10. Moderation

Every new or edited post and comment gets an automatic check by the model,
with the group's rules and its mods' last 20 decisions as examples. "clear"
does nothing; "borderline" flags the item; "violation" hides it until a
mod decides. Photos aren't sent to the check yet. When no worker is
running, items go up unchecked and the checks run when one returns.

A flagged item stays readable (comments are folded) and members vote Keep
or Hide. Only members of 30 days or more who have something shown in the
group can vote, and not the author or someone replying to them in that
thread. The group's "votes to settle" setting (default 5) decides: that
many Hide votes, more than Keep, hides it; that many Keep votes clears it
for good. A member's report flags an item the same way.

Mods work from `/mod/queue` (held, hidden, flagged and reported items) and
`/mod/log` (every action, the check's included as "auto"). Owners change
roles and mods ban members on `/mod/members`; owners can't be banned and
only owners can ban a mod. The operator can suspend an account everywhere
from `/admin`, which signs it out at once on every node.

Accounts under three days old, or with nothing shown in the group yet,
can't post links and can post five times an hour. Those limits are counted
in memory on the node serving the request, like the daily question limit.

## 11. Notifications and email

The bell counts unread notifications: replies to your comments, new
comments on posts you follow (you follow your own), newer posts linked to
them, join approvals, and what mods did with your posts. Authors are told
when something of theirs is hidden or removed unless the group turns off
"notify_hidden" (quiet hiding).

Email is off for everyone until they turn it on at `/profile`. The leader
checks every 5 minutes and sends each person one email with whatever has
waited 10 minutes unread; the daily digest (the day's top new posts in
each of their groups) goes at `digest_hour` UTC, default 12. Both go
through the same SMTP relay as sign-in links. If the leader changes
between sending and recording, a batch can go twice; nothing is lost.

## Retention

The leader submits a `Purge` command once a day. It removes expired sign-in
links and sessions, the personal details of accounts deleted more than 30
days ago, and posts, comments and old versions past their `purge_after`,
except anything under legal hold. Every node applies the same purge.
