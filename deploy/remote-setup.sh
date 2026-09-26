#!/bin/sh
# Part of make install (deploy/install.sh), run with sudo on the machine a
# new node copies its setup from: it packs up what the new node needs, for
# make install to fetch over the same ssh login. That's the site's cluster
# CA (so the new node can make its own certificate) and the ways into the
# site for the new node to join through. It never sends this
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

# The ways into the site, for the new node's join lines: asked of the node
# running here (every node that can be reached, and every domain), using
# its own certificate. If it isn't running, its advertise line, or its
# host name.
conf=/etc/grus/grus.conf
setting() { sed -n "s/^$1 *= *//p" "$conf" 2>/dev/null | tr -d ' ' | tail -1; }
data=$(setting data_dir)
[ -n "$data" ] || for d in /var/lib/grus /usr/local/var/grus; do [ -d "$d" ] && data=$d && break; done
ca=$(setting tls_ca); ca=${ca:-$data/cluster/ca.crt}
crt=$(setting tls_cert); crt=${crt:-$data/cluster/node.crt}
key=$(setting tls_key); key=${key:-$data/cluster/node.key}
port=$(setting cluster_addr | sed 's/.*://'); port=${port:-7946}
curl -fs --noproxy '*' --max-time 5 --cacert "$ca" --cert "$crt" --key "$key" \
    --resolve "grus-node:$port:127.0.0.1" "https://grus-node:$port/sync/join" >"$dir/join" 2>/dev/null || true
if [ ! -s "$dir/join" ]; then
    join=$(setting advertise)
    [ -n "$join" ] || join="$(hostname -f 2>/dev/null || hostname):$port"
    echo "$join" >"$dir/join"
fi

tar -C "$dir" -cf "$out" ca.crt ca.key join
chmod 600 "$out"
# Readable by the account that ssh'd in, which fetches and deletes it.
if [ -n "${SUDO_USER:-}" ]; then
    chown "$SUDO_USER" "$out"
fi
