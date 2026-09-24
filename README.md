# Grus

A self-hosted home for discussion groups: picture posts and comment threads,
organized so the good answers don't scroll away. One engine runs many groups,
each on its own subdomain (`travato.nfb.group`) or its own domain, across one
or more servers, with one passwordless login for all of them.

Grus is a single Go binary with SQLite files and a Raft-replicated command
log. No database server, no JavaScript framework, no third-party anything.

**Status: milestone M0 (the skeleton).** Sign-in, groups on subdomains,
domains and aliases, replication and recovery work. Posts arrive in M1.

## What's here in M0

- One static binary (`CGO_ENABLED=0`), with templates and CSS embedded.
- SQLite schemas with migrations: `site.db` for accounts, sessions, domains
  and the group list, and one `groups/<id>/group.db` per group. Soft delete,
  revisions and the daily purge are in the schema from the start.
- Every write is a command (`internal/cmd`) replicated through Raft
  (`hashicorp/raft`) over mutual TLS with a private cluster CA. The starting
  setup is one VPS as the only voter plus the Studio as a non-voting full
  copy.
- Routing by Host header: the primary domain, `<slug>.<primary>`, custom
  domains, single-host aliases, and alternate domains that redirect every
  old link. The primary can be changed live.
- Let's Encrypt certificates per host, only for hosts that exist, cached in
  `site.db` so every node has them.
- Passwordless sign-in: an emailed magic link (tap Continue, so email
  scanners can't use it up) plus a 6-digit code for the other-device case,
  returning to the page you started from. One session cookie covers every
  group. Send limits per address, per IP and overall.
- CSRF protection (Go's `CrossOriginProtection`), a strict CSP, and one
  read-access rule (`internal/auth/access.go`) for every read path.
- `grus backup` (consistent copies for NAS snapshots) and `grus recover`
  (the "VPS is gone" runbook), both tested.

## Try it on a laptop

```sh
go build -o grus ./cmd/grus
./grus ca init -dir certs && ./grus ca issue -dir certs n1
cat > dev.conf <<EOF
node_id = n1
data_dir = ./data
primary_domain = grus.localhost
dev = true
http_addr = 127.0.0.1:8080
cluster_addr = 127.0.0.1:7946
advertise = 127.0.0.1:7946
bootstrap = true
tls_ca = certs/ca.crt
tls_cert = certs/n1.crt
tls_key = certs/n1.key
operator = you@example.com
EOF
./grus serve -config dev.conf
```

Open <http://grus.localhost:8080/> (browsers send any `*.localhost` name to
your machine), sign in as the operator email, and pick the link out of the
server's output (with no `smtp_host`, emails are printed rather than sent).
Create a group on the admin page and it's live at
`http://<name>.grus.localhost:8080/`.

## Layout

```
cmd/grus/           the binary: serve, ca, backup, recover
internal/cmd/       every write, as a command struct + Apply (the only code that writes SQL)
internal/cluster/   the Log interface; Raft over mutual TLS; snapshots; recover
internal/store/     SQLite files, schemas and migrations, read queries
internal/web/       host routing, pages, sign-in, admin, certificates
internal/auth/      tokens and codes, send limits, the read-access rule
internal/mail/      SMTP (or print, on a laptop)
internal/ids/       time-ordered 64-bit ids
internal/config/    grus.conf
web/                templates and static files (embedded)
deploy/             example configs, systemd and launchd units, backup script
docs/               operations runbook, DNS records for mail
```

## Docs

- [docs/operations.md](docs/operations.md): setting up the VPS and the Studio,
  backups, and recovering from a lost VPS.
- [docs/dns.md](docs/dns.md): DNS for the primary domain, including SPF,
  DKIM and DMARC so sign-in emails reach the inbox.

## License

Apache 2.0. See [LICENSE](LICENSE).
