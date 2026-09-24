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
(`site.db`, `groups/`) before the first start. Photos (from M1) are
content-addressed files and are backed up by copying the blob directory.

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

## Retention

The leader submits a `Purge` command once a day. It removes expired sign-in
links and sessions, the personal details of accounts deleted more than 30
days ago, and posts, comments and old versions past their `purge_after`,
except anything under legal hold. Every node applies the same purge.
