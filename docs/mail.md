# Outbound mail

Sign-in only works if the email arrives. Grus hands every message to an SMTP
server: the global SMTP settings on `/admin`, or a domain's own there. Each
domain's email comes from that domain (`login@<domain>` unless the domain
has its own sender), so each domain needs the records below. With no relay
set, grus prints each email to the log instead, which is fine on a laptop
and a dead end in production.

There are two ways to give it one:

1. **Send from the VPS itself** (`deploy/install-mail.sh`, below): Postfix
   on loopback, DKIM signing for each domain, no outside account.
   Free and quick, but whether big providers accept the mail depends on the
   VPS's IP (see "Blocklists").
2. **A transactional mail service** (Postmark, Amazon SES, Mailgun,
   Brevo): set the SMTP relay, port, user and password on `/admin` to
   what the service gives you, and publish the SPF and DKIM records it
   gives you instead of the ones below. Their IPs have a sending
   reputation a new VPS doesn't, so this is the dependable choice once
   real users sign in. You can also keep option 1 and relay it through a
   service ("Relaying through a service", below).

## Sending from the VPS

make install does this (deploy/install-mail.sh): on a new site's first
node it offers it for the site's domain, and on another node that serves
pages it asks which domain, if any. To add a domain on a machine that's
set up already:

```sh
GRUS_MAIL_DOMAIN=example.org make install
```

Postfix greets other servers with the machine's `hostname -f`, which
should be its reverse-DNS name. The script:

- installs Postfix and OpenDKIM;
- makes a 2048-bit DKIM key for the domain under
  `/etc/opendkim/keys/<domain>/`, with selector `grus<year>` (kept on
  re-runs);
- configures Postfix to listen on 127.0.0.1 only (never an open relay),
  deliver straight to each recipient's mail server with TLS when offered,
  and pass every message through OpenDKIM;
- delivers mail addressed to the domain itself (bounces to `mail_from`,
  DMARC reports, `postmaster@`) to root's mailbox, `/var/mail/root`;
- adds the domain to OpenDKIM's key and signing tables, so running it
  again for each other domain signs each with its own key;
- sets `smtp_host = 127.0.0.1` and `smtp_port = 25` in
  `/etc/grus/grus.conf`, the seed for a new cluster's first start. On a
  site that's already running, set the relay on `/admin` instead
  (127.0.0.1, port 25, no user or password);
- prints the DNS records, and saves the DKIM value to
  `/root/dkim-<domain>.txt` on one line.

The original `/etc/postfix/main.cf` and `/etc/opendkim.conf` are kept
beside them with `.orig` on the end.

## DNS records

Three TXT records on each domain. Most DNS panels add the domain to
the name for you, so type only the short part.

| Name | Type | Value |
|---|---|---|
| `@` | TXT | `v=spf1 ip4:VPS_IP -all` |
| `grus2026._domainkey` | TXT | the line in `/root/dkim-<domain>.txt` |

(`grus2026` is the selector the script printed; a key made in another
year has that year.)
| `_dmarc` | TXT | `v=DMARC1; p=none` |

- **SPF** says the VPS may send as the domain. A domain has one SPF
  record; if there's already one, add `ip4:VPS_IP` to it.
- **DKIM** is the public half of the signing key. Paste the value as one
  line: the spaces after the semicolons belong, but the key after `p=` has
  none. A panel that limits TXT strings to 255 characters splits it for
  you, which is fine.
- **DMARC** tells receivers what to do with mail that fails both. Start at
  `p=none`; after a few weeks of clean sending, move to `p=quarantine`.

Also check **reverse DNS**: the VPS's IP should map to the name Postfix
greets with, and that name back to the IP. Set it at the VPS provider (on
DigitalOcean, the droplet's name is its reverse DNS).

```sh
dig +short -x VPS_IP                       # the greeting name
dig +short TXT grus2026._domainkey.nfb.group
opendkim-testkey -d nfb.group -s grus2026 -vvv   # "key OK" (not "secure"
                                                 # only means no DNSSEC)
```

## Blocklists

Many providers' IP ranges are on the Spamhaus **PBL**, a list of addresses
not expected to send mail directly, and Gmail, Google Workspace and most
large providers refuse mail from them however well it's signed. The bounce
reads: "The IP you're using to send mail is not authorized to send email
directly to our servers."

Check the VPS's IP (a result of `127.0.0.10` or `127.0.0.11` is the PBL):

```sh
ip=VPS_IP; r=$(echo $ip | awk -F. '{print $4"."$3"."$2"."$1}')
dig +short $r.zen.spamhaus.org @$(dig +short NS zen.spamhaus.org | head -1)
```

If it's listed, look the IP up at <https://check.spamhaus.org> and follow
the PBL removal steps (a captcha and an email confirmation). A
Spamhaus-maintained listing (`127.0.0.11`) can be removed by whoever runs
the server; it takes effect within about an hour. If mail is still
refused after that, the IP has no sending reputation yet: relay through a
service.

## Relaying through a service

To keep the local Postfix and DKIM but hand delivery to a service, add its
relay to Postfix (the service's SMTP host, port 587, and login):

```sh
sudo postconf -e 'relayhost = [smtp.example-service.com]:587' \
    'smtp_sasl_auth_enable = yes' 'smtp_sasl_security_options = noanonymous' \
    'smtp_sasl_password_maps = hash:/etc/postfix/sasl_passwd' \
    'smtp_tls_security_level = encrypt'
echo '[smtp.example-service.com]:587 USER:PASSWORD' | sudo tee /etc/postfix/sasl_passwd >/dev/null
sudo chmod 600 /etc/postfix/sasl_passwd && sudo postmap /etc/postfix/sasl_passwd
sudo systemctl reload postfix
```

Then add the include the service documents to the SPF record, beside
`ip4:VPS_IP`. The DKIM record stays: the mail is still signed
here.

## Testing and troubleshooting

```sh
printf 'Subject: test\n\nhello\n' | sendmail -f login@nfb.group you@example.com
journalctl -u postfix --since -5min | grep status=   # sent, deferred, bounced
mailq                                                # anything stuck
```

In the received message's headers, look for `spf=pass`, `dkim=pass` and
`dmarc=pass`.

While mail isn't arriving, sign-in still works for an operator with shell
access: a refused sign-in email bounces into `/var/mail/root` with the
link and the 6-digit code in it.

```sh
grep -E '/link/|^ +[0-9]{6}$' /var/mail/root | tail -2
```
