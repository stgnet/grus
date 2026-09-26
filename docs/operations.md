# Operations

The starting setup: one VPS serves every page, and the Studio holds a live,
complete copy of everything. Neither is in charge. Each applies every
write it takes at once and passes it to the other, and both end up with
the same files ([replication.md](replication.md) has how). Either can be
down, or unable to reach the other, and the one that's up carries on,
reads and writes, and they merge when they're back in touch.

```
VPS (n1)    takes new groups   site.db + every group file, HTTPS for every host
   │  mTLS :7946, operations both ways ↕
Studio      full copy          the same files, live
```

Each file has its own operations: site.db's, which every node holds, the
root FAQ's, which every node holds too, and one set per group, held only
by the nodes the group is placed on. The node map in site.db says which
nodes there are and which groups each holds.

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
make                               # builds ./grus (static; needs Go, see go.mod)
sudo make install                  # user, directories, systemd unit, example config
sudoedit /etc/grus/grus.conf       # from deploy/grus.conf.example
sudo systemctl enable --now grus
journalctl -u grus -f
```

Build as yourself and install with sudo, as two steps (`install` never
builds). To upgrade later: `git pull && make && sudo make install`, which
keeps the config and restarts the service. If the VPS has no Go, run
`make dist` elsewhere and copy `dist/grus-linux-amd64` to `./grus` in the
checkout before `sudo make install`.

Open ports 80 and 443 to everyone, and the cluster port (7946) to the
Studio. The first start (with `bootstrap = true`) creates the cluster and
seeds the global level from the config's seed lines: the first `domain`,
the `operator` emails, the SMTP relay and the rest (see
deploy/grus.conf.example). Sign in with an `operator` email to reach
`/admin`, where all of that is changed from then on; the seed lines are
never read again.

### The global level

grus.conf holds only what's particular to one node: its id, its files,
where it listens, the cluster, and `ai_url` if it has a model. Everything
else lives in site.db, is the same on every node, and is changed on
`/admin`:

- **Domains.** One list for the whole site, every domain equal. Any node
  answers any listed domain, the same way, and every request is answered
  in the domain it came in on: its links, sign-in, the group list, the
  FAQ. So each domain's DNS can point at different nodes, and a node can
  be tried out on its own by the domain that leads to it. Sign-in is per
  domain (a browser keeps each domain's cookie apart). Each domain can have
  its own SMTP relay and sender; without one it uses the global relay and
  `login@<domain>`.
- **Global settings.** Operators, the default SMTP relay, the Let's
  Encrypt contact, the AI model and context window, the daily question
  limit, and the FAQ and digest hours.
- **Groups and their settings.** Every group's settings (name,
  visibility, joining, AI and the rest) live in site.db beside the group
  list; the group's own file keeps a copy for its own commands, and holds
  the group's content: posts, members, the FAQ.

Email that isn't a reply to a request (notifications, the digest) goes out
in the domain the person last signed in on, through that domain's relay.

**Upgrading** a site from before the global level: its `primary_domain`,
`smtp_*`, `mail_from`, `operator`, `acme_email`, `ai_model`, `ai_context`,
`ask_daily_limit`, `faq_hour` and `digest_hour` lines are adopted as the
seed on the first start, so nothing needs editing first. Alternate
domains become full, equal domains (they no longer redirect). A group's
own domain and host aliases are dropped: add a domain to the list instead
if it should keep being answered. `worker` lines are ignored (nodes with a
model are found from the node map). Each group's settings are copied up
to site.db by the node on duty for it within moments of the start; settings
changes wait for that. Afterwards the seed lines can be deleted.

## 3. The Studio

In a checkout on the Studio, `make` then `sudo make install`. The first
run installs the binary, the directories and the launchd job, and puts
`deploy/studio.conf.example` at `/etc/grus/grus.conf`; edit that, add the
certificates, and run `sudo make install` again to start it. The job runs
as the account that ran sudo (the Studio's node binds no low ports, so it
needs no service account), and logs to `/usr/local/var/log/grus/grus.log`.
`data_dir` is just a directory: put it wherever the Studio keeps its
data, as long as it behaves like a local disk (a disk image on the NAS
does; a share mounted straight over SMB or NFS can corrupt SQLite files).

The Studio's config has `full = true` and `join = <VPS cluster address>`.
On its first start it copies site.db from the VPS, registers itself in
the node map, and, being a full node, is placed on every group and copies
each one. After that the two pass operations back and forth; a Studio
that was offline catches up by itself when it's back.

The admin page's Cluster section shows, for this node, each file it
holds and how many of its operations aren't stable yet, when it last
heard from each other node, and how many rewinds it has done.

## 4. No backups, no restores

There's nothing to back up and nothing to restore. Every full node (the
Studio, and any other) is a live, complete copy of everything, and every
write is on the node that took it before the page says it worked. If
nodes are lost, start new ones with a `join` line pointing at any node
still running: they copy what they need and carry on. The copy of last
resort is the Studio's.

## 5. When nodes are lost

Nothing needs doing for the site to keep working: the nodes still up
carry on, including one left entirely alone.

- **A node that will come back** (a reboot, the Studio's home connection
  dropping): do nothing. It catches up when it's back.
- **The VPS is gone for good:** start a new VPS with a config that has
  `voter = true` and `join = <the Studio's cluster address>`, and point
  DNS at it. It copies everything from the Studio and serves from then on.
  Then remove the old VPS on the admin page (below), giving its groups to
  the new one.
- **A node gone for good**, whatever it was: remove it on the admin page's
  Nodes section. Until it's removed, the others wait for it before
  treating any operation as final, and keep every operation for it: the
  site works, but a little more slowly and with more on disk. If a node you
  removed turns out not to be gone, it finds out when it's back in touch,
  sends again anything the others hadn't got (as a new node), and carries
  on. Nothing it took is lost.

`TestRebuildFromSurvivor` in `internal/cluster` runs the "everything but
the Studio is gone" case on every CI run, and `TestRandomSplits` splits
three nodes at random while they take writes and checks every copy ends
up the same.

## 6. Adding and removing domains

1. Set up the new domain's DNS (docs/dns.md), including mail records,
   pointing at whichever nodes should answer it.
2. On `/admin`, add it. It's live on every node at once, with the same
   groups and content, and pages asked for on it link within it.
3. If mail for it should go through a different relay, or come from a
   different sender, set that on the domain's row.

Removing a domain stops every node answering it. Keep renewing a domain
for as long as links to it are out there. The last domain can't be
removed.

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
3. Set `ai_url` in the Studio's grus.conf and restart it; it records in
   the node map that it has a model, and every other node sends it
   searches from then on. Set `ai_model` (and `ai_context`) on `/admin`:
   every node with a model runs that one, and a change applies at once.
4. `/admin` shows, per day, how many model calls each kind of work made,
   tokens in and out, seconds, soft fails by cause, and the job queue.

Group owners turn AI off for their group on the group's Settings page.
With it off, no job runs for that group and search never asks the model
about it. Nothing is ever sent to an outside AI service.

## 8. The FAQ and outside sources

Each night at `faq_hour` (a global setting; UTC, default 8) the node on duty queues the FAQ batch:
entries whose threads changed are rewritten, and threads that grew into a
well-answered cluster get a new entry (at most 20 a night per group). On
Sundays the batch also tidies the topic outline and re-reads outside pages
not checked in the past week. The work runs on the worker like any other
job, so with no worker it simply waits.

Mods (or anyone they trust) can edit any entry; a locked entry keeps its
wording and the model only leaves a suggestion beside it. Every change is
kept in the entry's history and can be rolled back.

The FAQ on the bare domain (any of them) is the site-wide one. Its list of groups
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

Email is off for everyone until they turn it on at `/profile`. Each
group's email is sent by the node on duty for it (the lowest-id node
holding it that the others have heard from in the last minute): every 5
minutes it sends each person one email with whatever has waited 10 minutes
unread, and the daily digest (the day's top new posts in each of their
groups) goes at `digest_hour` UTC, default 12. When one node is on duty
for every group, as in the starting setup, that's one email per person
for all their groups. Each person's email goes out in the domain they
last signed in on, through that domain's relay. If duty moves between
sending and recording, or nodes are split and each side has one on duty,
a batch can go twice; nothing is lost.

## Retention

The node on duty submits a `Purge` command once a day, which sends each
group's part to that group's file. It removes expired sign-in
links and sessions, the personal details of accounts deleted more than 30
days ago, and posts, comments and old versions past their `purge_after`,
except anything under legal hold. Every node applies the same purge.

## 12. More nodes

A second or third VPS: issue it a certificate, and give it a config with
`voter = true` and `join = <any existing node's cluster address>`. It
copies site.db on first start, registers itself, and new groups are
placed on it from then on: on the first three nodes with `voter = true`
(by node id), and on every full node.

A small VPS that holds only a few groups: neither `voter` nor `full`, and
a `join` line. It holds nothing until groups are placed on it, from the
admin page's Nodes section ("Place a group"). A node a group is placed on
copies it from a node that has it, then keeps in step; a node it's taken
off deletes its copy once another node has everything it wrote there.

Every node answers for every host name: a request for a group this node
doesn't hold is passed over the cluster port to one that does, and the
pages that gather from every group (the home page, notifications) go to a
full node when this one isn't. So DNS can point everything at any node, and
moving a group needs no DNS change. Writes work the same from anywhere:
one for a group this node doesn't hold is made on a node that does.

Photos follow their groups: the node that receives one pushes it to the
other nodes holding the group, and a node fetches any it's missing, on a
page view or within a minute otherwise.

Every node's `node_num` must be different: it goes into every id the node
makes. A node that finds its number in use by another stops taking writes
and says so in its log and on the admin page.

### An off-site copy

The Studio's copy is live but it's in one house. For a copy somewhere
else, run another full node: a cheap VPS in another region with `full =
true` and a `join` line. It holds every group, takes writes like any other
node, and is a complete copy to rebuild from if the house and the VPS are
both lost.

## 13. Before opening a group: load test

`grus loadtest -url https://travato.nfb.group -c 20 -d 60s` reads the
group's public pages (its front page, FAQ, and every post the front page
links to) as 20 signed-out visitors at once, and reports pages a second
and how long pages took. It only reads. Run it from another machine, and
try stopping a node partway through: the other nodes should carry on
without a pause.

Signed-out views of a public group come from the render cache: each page
is rendered once and kept until the group or site.db changes, so a busy
public thread costs one render per change, not one per view.

## 14. Group export

A group's owner can download everything the group is from the bottom of
its settings page: its database file (posts, comments and their history,
the FAQ, members, the mod log), a list of handles for the accounts it
mentions (no emails), and every photo, as one `.tar.gz`.
