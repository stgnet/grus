#!/bin/sh
# Part of make install (deploy/install.sh), run with sudo on the machine a
# new node copies its setup from: it packs up what the new node needs, for
# make install to fetch over the same ssh login. That's the site's cluster
# CA (so the new node can make its own certificate) and this machine's
# cluster address (for the new node to join through). It never sends this
# machine's own certificate or anything else.
set -eu

out=/tmp/grus-setup.tar
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT

# The CA is kept in different places depending on how the machine was set
# up: the data directory (make install), or /root/grus-ca and
# /etc/grus/cluster (older installs).
for d in /var/lib/grus/cluster /usr/local/var/grus/cluster /root/grus-ca /etc/grus/cluster; do
    if [ -f "$d/ca.crt" ] && [ -f "$d/ca.key" ]; then
        cp "$d/ca.crt" "$d/ca.key" "$dir/"
        break
    fi
done
[ -f "$dir/ca.key" ] || { echo "no cluster CA on this machine: is it a node of the site?" >&2; exit 1; }

# Its cluster address: the advertise line, or its default (host name, port 7946).
join=$(sed -n 's/^advertise *= *//p' /etc/grus/grus.conf 2>/dev/null | tr -d ' ' | tail -1)
[ -n "$join" ] || join="$(hostname -f 2>/dev/null || hostname):7946"
echo "$join" >"$dir/join"

tar -C "$dir" -cf "$out" ca.crt ca.key join
chmod 600 "$out"
# Readable by the account that ssh'd in, which fetches and deletes it.
if [ -n "${SUDO_USER:-}" ]; then
    chown "$SUDO_USER" "$out"
fi
