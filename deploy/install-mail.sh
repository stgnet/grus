#!/bin/sh
# Sets up outbound mail on a Linux VPS so grus can send sign-in and
# notification email itself, with no outside mail service: Postfix as a
# send-only server on loopback, OpenDKIM signing, and grus pointed at it.
# make install runs it (deploy/install.sh, step 5), as root:
#
#   deploy/install-mail.sh [domain] [helo-name]
#
# Each run sets up DKIM for one domain; run it once per domain on the
# admin page's list, since each domain's email comes from that domain.
# domain defaults to the first domain line in /etc/grus/grus.conf;
# helo-name (the name Postfix greets other servers with) defaults to
# `hostname -f` and should be this server's reverse-DNS name. It's safe to
# re-run: it keeps existing DKIM keys and prints the DNS records again.
# docs/mail.md has the full story, including the blocklist check.
set -eu

conf=/etc/grus/grus.conf
[ -f "$conf" ] || { echo "No $conf: run 'make install' first." >&2; exit 1; }

domain=${1:-$(sed -n -E 's/^(primary_)?domain *= *//p' "$conf" | head -n 1 | tr -d ' ')}
helo=${2:-$(hostname -f)}
[ -n "$domain" ] || { echo "No domain given and no domain line in $conf." >&2; exit 1; }

export DEBIAN_FRONTEND=noninteractive
echo "postfix postfix/main_mailer_type select Internet Site" | debconf-set-selections
echo "postfix postfix/mailname string $helo" | debconf-set-selections
apt-get install -y -qq postfix opendkim opendkim-tools >/dev/null

# DKIM: one key per domain, kept across re-runs. The selector carries the
# year it was made, so a later key can sit beside it in DNS during a swap.
keys=/etc/opendkim/keys/$domain
install -d -o opendkim -g opendkim -m 750 "$keys"
sel=$(ls "$keys" 2>/dev/null | sed -n 's/\.private$//p' | head -1)
if [ -z "$sel" ]; then
    sel=grus$(date +%Y)
    opendkim-genkey -b 2048 -d "$domain" -s "$sel" -D "$keys"
fi
chown opendkim:opendkim "$keys"/*
chmod 600 "$keys/$sel.private"

# One line per domain in the key and signing tables, so every domain this
# script has been run for is signed with its own key. A re-run for the same
# domain replaces its lines rather than adding more.
# Both tables name the key record, <selector>._domainkey.<domain>, which is
# what finds this domain's old lines.
re=$(printf '%s' "$domain" | sed 's/\./\\./g')
for t in KeyTable SigningTable; do
    touch /etc/opendkim/$t
    sed -i -E "/\._domainkey\.$re( |\$)/d" /etc/opendkim/$t
done
echo "$sel._domainkey.$domain $domain:$sel:$keys/$sel.private" >> /etc/opendkim/KeyTable
echo "*@$domain $sel._domainkey.$domain" >> /etc/opendkim/SigningTable

[ -f /etc/opendkim.conf.orig ] || cp /etc/opendkim.conf /etc/opendkim.conf.orig
cat > /etc/opendkim.conf <<EOF
# Signs outbound mail (grus sign-in and notification email) for every
# domain in the key and signing tables. Written by deploy/install-mail.sh.
Syslog          yes
UMask           007
Mode            s
Canonicalization relaxed/simple
OversignHeaders From
KeyTable        refile:/etc/opendkim/KeyTable
SigningTable    refile:/etc/opendkim/SigningTable
Socket          inet:8891@127.0.0.1
PidFile         /run/opendkim/opendkim.pid
UserID          opendkim
TrustAnchorFile /usr/share/dns/root.key
EOF

# Postfix: listens on loopback only, so it's never an open relay, and
# delivers straight to each recipient's MX. The loopback listener offers no
# STARTTLS: Go's smtp.SendMail would try it and reject the self-signed
# certificate. Outbound delivery uses TLS whenever the other side has it.
# Mail to the domain itself (bounces to mail_from, DMARC reports) lands in
# root's local mailbox rather than queueing for a server that isn't there.
[ -f /etc/postfix/main.cf.orig ] || cp /etc/postfix/main.cf /etc/postfix/main.cf.orig
postconf -e \
    "myhostname = $helo" \
    "myorigin = $domain" \
    "mydestination = localhost, $domain" \
    'inet_interfaces = loopback-only' \
    'inet_protocols = ipv4' \
    'mynetworks = 127.0.0.0/8' \
    'smtpd_tls_security_level = none' \
    'smtp_tls_security_level = may' \
    'smtp_tls_loglevel = 1' \
    'milter_default_action = accept' \
    'milter_protocol = 6' \
    'smtpd_milters = inet:127.0.0.1:8891' \
    'non_smtpd_milters = inet:127.0.0.1:8891'
for a in login dmarc postmaster; do
    grep -q "^$a:" /etc/aliases || echo "$a: root" >> /etc/aliases
done
newaliases

systemctl enable -q opendkim postfix
systemctl restart opendkim postfix

# grus sends to the local Postfix with no login (Postfix offers no AUTH).
# In grus.conf these are only the seed for a cluster's first start; on a
# running site the relay is a global setting on the admin page.
sed -i -E \
    -e 's/^#? *smtp_host *=.*/smtp_host = 127.0.0.1/' \
    -e 's/^#? *smtp_port *=.*/smtp_port = 25/' \
    -e 's/^(smtp_(user|pass) *=)/# \1/' "$conf"
grep -q '^smtp_host' "$conf" || printf 'smtp_host = 127.0.0.1\nsmtp_port = 25\n' >> "$conf"

# The DNS records, with the DKIM value on one line (also saved to a file,
# since a terminal can wrap or indent a long line when it's copied).
ip=$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
dkim=$(sed -n 's/.*"\(.*\)".*/\1/p' "$keys/$sel.txt" | tr -d '\n')
echo "$dkim" > "/root/dkim-$domain.txt"
# And where the admin page's DNS check looks for it, to compare with what
# DNS has (internal/web/dnscheck.go).
install -d -m 750 /var/lib/grus/dkim /var/lib/grus/dkim/"$domain"
echo "$dkim" > /var/lib/grus/dkim/"$domain"/"$sel".txt
id grus >/dev/null 2>&1 && chown -R grus:grus /var/lib/grus/dkim

cat <<EOF

Mail is set up: postfix and opendkim are running, and sign $domain's mail.
If grus is already running, set the SMTP relay on /admin to 127.0.0.1,
port 25, no user or password (global settings, or $domain's own row).
Add these TXT records for $domain (docs/mail.md):

  $domain                      v=spf1 ip4:$ip -all
  _dmarc.$domain               v=DMARC1; p=none
  $sel._domainkey.$domain      (the line in /root/dkim-$domain.txt)

Reverse DNS for $ip should be $helo, and $helo should resolve to $ip.
Then check the IP isn't on the Spamhaus PBL (https://check.spamhaus.org):
Gmail and most large providers refuse mail from listed IPs.
EOF
