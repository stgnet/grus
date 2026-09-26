# Grus

A self-hosted home for discussion groups: picture posts and comment threads,
organized so the good answers don't scroll away. One engine runs many groups,
each on its own subdomain (`travato.nfb.group`), across one or more servers,
with one passwordless login for all of them. The site can have several
domains, all equal: any server answers any of them, the same way.

Grus is a single Go binary with SQLite files, replicated between servers
with no leader: every server takes writes, even alone, and they merge
([docs/replication.md](docs/replication.md)). No database server, no
JavaScript framework, no third-party anything.

**Status: milestone M8, ready to open to the Travato groups.** Sign-in, groups, replication, posts and photos,
the archive import, search with quick answers, link notes, each group's
FAQ, thread summaries, topics and outside sources, private and hidden
groups, join approval and invites, anonymous posting, sister groups,
moderation (the automatic check, reports, member votes, the mod queue and
log, bans, roles, lock and pin, held first posts, new-account limits),
"helpful" votes and the Top sort, following posts, notifications, optional
notification email and a daily digest, and public profiles all work.
Since M7 the site runs across several servers: each group lives on the
nodes it's placed on, and any node answers for any group. M8 added the
public-page render cache, group export, node and placement tools on the
admin page, mobile polish, and load tests. M9 made the global level (every
setting in site.db, one list of equal domains) and replaced Raft with
replication that has no leader: any node keeps working alone and merges
when it's back in touch, and there are no backups or restores to do.

## What's here in M0

- One static binary (`CGO_ENABLED=0`), with templates and CSS embedded.
- SQLite schemas with migrations: `site.db` for accounts, sessions, domains
  and the group list, and one `groups/<id>/group.db` per group. Soft delete,
  revisions and the daily purge are in the schema from the start.
- Every write is a command (`internal/cmd`), applied on the node that
  takes it as an operation and passed to the other nodes over mutual TLS
  with a private cluster CA. Each node applies every operation in the same
  order (by a hybrid logical clock), rewinding a file when one arrives
  late, so every copy ends up the same (docs/replication.md). Each file
  has its own operations (site.db's, and one set per group), so a node
  holds only the groups placed on it. The starting setup is one VPS plus
  the Studio as a full copy.
- Routing by Host header against one global list of domains, all equal:
  `<domain>` is the home site and `<slug>.<domain>` a group, on every
  listed domain, and each request is answered in the domain it came in on.
  Domains are added and removed live.
- The global level: site.db holds everything needed to run the system
  except the groups' content (the domains, each domain's mail settings,
  operators, SMTP, AI and schedule settings, and every group's settings),
  changed on the admin page. grus.conf holds only what's particular to a
  node.
- Let's Encrypt certificates per host, only for hosts that exist, cached in
  `site.db` so every node has them.
- Passwordless sign-in: an emailed magic link (tap Continue, so email
  scanners can't use it up) plus a 6-digit code for the other-device case,
  returning to the page you started from. One session cookie covers every
  group. Send limits per address, per IP and overall.
- CSRF protection (Go's `CrossOriginProtection`), a strict CSP, and one
  read-access rule (`internal/auth/access.go`) for every read path.
- No backups and no restores: every full node is a live, complete copy,
  and a lost node is replaced by starting a new one pointed at any other.

## Try it on a laptop

```sh
make                    # or: go build -o grus ./cmd/grus
cat > dev.conf <<EOF
node_id = n1
data_dir = ./data
domain = grus.localhost
dev = true
http_addr = 127.0.0.1:8080
cluster_addr = 127.0.0.1:7946
advertise = 127.0.0.1:7946
operator = you@example.com
EOF
./grus -config dev.conf
```

Open <http://grus.localhost:8080/> (browsers send any `*.localhost` name to
your machine), sign in as the operator email, and pick the link out of the
server's output (with no SMTP relay set, emails are printed rather than
sent). Create a group on the admin page and it's live at
`http://<name>.grus.localhost:8080/`. The first start makes the cluster's
certificates in `./data/cluster`.

`http://localhost:8080/` works too, on this or any node (every node
answers `localhost` from its own machine), with groups at
`http://localhost:8080/g/<name>/`; and any domain answers
`<domain>/g/<name>/` for a group. See [docs/dns.md](docs/dns.md), "Before
DNS".

## Installing for real

`make install`, on a fresh or running Ubuntu or macOS machine: it
installs what's missing, builds, sets the machine up (asking which
existing machine to copy the setup from, if any) and starts it.
[docs/operations.md](docs/operations.md) has the rest. There are no other
commands: everything an operator adjusts is on the admin page.

## Layout

```
cmd/grus/           the binary: the service, and nothing else
internal/tools/     the admin page's tools: archive import, model measurement, load test
internal/cmd/       every write, as a command struct + Apply (the only code that writes SQL)
internal/cluster/   the Log interface; replication without a leader, over mutual TLS; the node map
internal/store/     SQLite files, schemas and migrations, read queries
internal/web/       host routing, pages, sign-in, admin, certificates
internal/auth/      tokens and codes, send limits, the read-access rule
internal/mail/      SMTP (or print, on a laptop)
internal/ids/       time-ordered 64-bit ids
internal/config/    grus.conf
web/                templates and static files (embedded)
deploy/             make install (install.sh), example configs, systemd and launchd units, mail setup
docs/               operations runbook, DNS records for mail
```

## Docs

- [docs/operations.md](docs/operations.md): setting up the VPS and the Studio,
  what to do when nodes are lost, and adding more nodes.
- [docs/replication.md](docs/replication.md): how the nodes keep their
  copies the same with no leader.
- [docs/dns.md](docs/dns.md): DNS for each domain, including SPF,
  DKIM and DMARC so sign-in emails reach the inbox.
- [docs/mail.md](docs/mail.md): sending that email from the VPS itself
  (`deploy/install-mail.sh`), blocklists, and relaying through a service.

## License

Apache 2.0. See [LICENSE](LICENSE).
