# DNS for a domain

Every domain on the admin page's list is the whole site, answered the same
way by every node. Everything below is for `nfb.group`; each other listed
domain needs the same records with its own name. `VPS_IP` is the public
address of a node that serves web traffic.

## Web

| Name | Type | Value | Why |
|---|---|---|---|
| `nfb.group` | A | `VPS_IP` | home page and sign-in |
| `*.nfb.group` | A | `VPS_IP` | every group; a new group is live as soon as it's created |
| `www.nfb.group` | (covered by the wildcard) | | redirects to `nfb.group` |

Certificates are fetched per host the first time it's used, and only for
hosts that exist (a listed domain, its `www`, and its groups). A random
name like `typo.nfb.group` resolves but fails to connect, which is
intended.

Since any node answers any listed domain, the domains can point at
different nodes: `nfb.group` at one VPS and `eu.example.org` at another,
say, or a new node given a domain of its own to try it out on before
anything else points at it. The site is the same on each; only the
address differs, and every link on a page stays in the domain it was
asked for on.

The Studio needs a name of its own for the cluster port (any domain works;
it doesn't have to be under `nfb.group`). Use a short TTL (300s) so a new
home IP takes effect quickly.

## Mail: SPF, DKIM, DMARC

Sign-in only works if the email reaches the inbox, so set these up before
the first real user, for each domain. Email about a domain comes from that
domain (`login@<domain>`, or the domain's own sender on the admin page), so
each domain needs its own SPF, DKIM and DMARC. Grus sends through an SMTP
relay (your mail provider; the global SMTP settings on the admin page, or
a domain's own); the relay signs with DKIM. To send from the VPS
itself instead, with no mail provider, see [mail.md](mail.md): it has the
records for that setup.

**SPF** says which servers may send as `@nfb.group`. Use the include your
provider documents, for example:

| Name | Type | Value |
|---|---|---|
| `nfb.group` | TXT | `v=spf1 include:_spf.example-provider.com -all` |

**DKIM** is a public key your provider gives you, under a selector name:

| Name | Type | Value |
|---|---|---|
| `<selector>._domainkey.nfb.group` | TXT (or CNAME, per provider) | from the provider |

**DMARC** tells receivers what to do with mail that fails both. Start in
monitoring mode, then tighten once reports look clean:

| Name | Type | Value |
|---|---|---|
| `_dmarc.nfb.group` | TXT | `v=DMARC1; p=none; rua=mailto:dmarc@nfb.group` |

After a couple of weeks of clean reports, change `p=none` to
`p=quarantine`, then `p=reject`.

These names start with `_`, which a group slug can never contain, so no
group can collide with them. (The wildcard `*.nfb.group` A record doesn't
interfere with TXT lookups.)

**Check:** send yourself a sign-in email and look at the headers for
`spf=pass`, `dkim=pass` and `dmarc=pass`.

## Adding a domain

Point the new domain's two A records (bare and wildcard) at the node or
nodes that should answer it, add its mail records, then add it on the admin
page. It's live on every node at once; certificates are fetched as its
hosts are first used. Taking a domain off the list stops it being
answered anywhere.
