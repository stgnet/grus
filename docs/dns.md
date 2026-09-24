# DNS for the primary domain

Everything below is for `nfb.group`; substitute the domain if it changes
(see "Changing the primary" in operations.md). `VPS_IP` is the VPS's public
address.

## Web

| Name | Type | Value | Why |
|---|---|---|---|
| `nfb.group` | A | `VPS_IP` | home page and sign-in |
| `*.nfb.group` | A | `VPS_IP` | every group; a new group is live as soon as it's created |
| `www.nfb.group` | (covered by the wildcard) | | redirects to `nfb.group` |

Certificates are fetched per host the first time it's used, and only for
hosts that exist (groups, aliases, the primary). A random name like
`typo.nfb.group` resolves but fails to connect, which is intended.

The Studio needs a name of its own for the cluster port (any domain works;
it doesn't have to be under `nfb.group`). Use a short TTL (300s) so a new
home IP takes effect quickly.

## Mail: SPF, DKIM, DMARC

Sign-in only works if the email reaches the inbox, so set these up before
the first real user. Grus sends through an SMTP relay (your mail provider,
`smtp_host` in grus.conf); the relay signs with DKIM. To send from the VPS
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

## Alternate domains

An alternate domain (the old primary after a change, a short or typo
domain) needs the same two A records (bare and wildcard) pointing at the
VPS. Add it on the admin page first; it then redirects every link to the
same page on the primary, with its own certificates.
