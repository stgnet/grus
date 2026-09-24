#!/bin/sh
# Sets up outbound mail on a Linux VPS so grus can send sign-in and
# notification email itself, with no outside mail service: Postfix as a
# send-only server on loopback, OpenDKIM signing for the primary domain,
# and grus.conf pointed at it. Run as root after `make install`:
#
#   sudo deploy/install-mail.sh [domain] [helo-name]
#
# domain defaults to primary_domain in /etc/grus/grus.conf; helo-name (the
# name Postfix greets other servers with) defaults to `hostname -f` and
# should be this server's reverse-DNS name. It's safe to re-run: it keeps
# an existing DKIM key and prints the DNS records again. docs/mail.md has
# the full story, including the blocklist check.
set -eu

conf=/etc/grus/grus.conf
[ -f "$conf" ] || { echo "No $conf: run 'sudo make install' first." >&2; exit 1; }

domain=${1:-$(sed -n 's/^primary_domain *= *//p' "$conf" | tr -d ' ')}
helo=${2:-$(hostname -f)}
[ -n "$domain" ] || { echo "No domain given and no primary_domain in $conf." >&2; exit 1; }

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

[ -f /etc/opendkim.conf.orig ] || cp /etc/opendkim.conf /etc/opendkim.conf.orig
cat > /etc/opendkim.conf <<EOF
# Signs outbound mail for $domain (grus sign-in and notification email).
# Written by deploy/install-mail.sh.
Syslog          yes
UMask           007
Mode            s
Canonicalization relaxed/simple
OversignHeaders From
Domain          $domain
Selector        $sel
KeyFile         $keys/$sel.private
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
sed -i -E \
    -e 's/^#? *smtp_host *=.*/smtp_host = 127.0.0.1/' \
    -e 's/^#? *smtp_port *=.*/smtp_port = 25/' \
    -e 's/^(smtp_(user|pass) *=)/# \1/' "$conf"
grep -q '^smtp_host' "$conf" || printf 'smtp_host = 127.0.0.1\nsmtp_port = 25\n' >> "$conf"
if systemctl is-active -q grus; then systemctl restart grus; fi

# The DNS records, with the DKIM value on one line (also saved to a file,
# since a terminal can wrap or indent a long line when it's copied).
ip=$(ip -4 route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
dkim=$(sed -n 's/.*"\(.*\)".*/\1/p' "$keys/$sel.txt" | tr -d '\n')
echo "$dkim" > "/root/dkim-$domain.txt"

cat <<EOF

Mail is set up: postfix and opendkim are running, and grus sends through
127.0.0.1:25. Add these TXT records for $domain (docs/mail.md):

  $domain                      v=spf1 ip4:$ip -all
  _dmarc.$domain               v=DMARC1; p=none
  $sel._domainkey.$domain      (the line in /root/dkim-$domain.txt)

Reverse DNS for $ip should be $helo, and $helo should resolve to $ip.
Then check the IP isn't on the Spamhaus PBL (https://check.spamhaus.org):
Gmail and most large providers refuse mail from listed IPs.
EOF
