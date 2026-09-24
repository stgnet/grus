#!/bin/sh
# Installs grus as a systemd service on a Linux node. Run as root from the
# repository after `go build -o grus ./cmd/grus`:
#
#   sudo deploy/install-service.sh
#
# It's safe to re-run: it upgrades the binary and unit and restarts.
set -eu

here=$(dirname "$0")

id grus >/dev/null 2>&1 || useradd --system --home-dir /var/lib/grus --shell /usr/sbin/nologin grus
install -d -o grus -g grus -m 750 /var/lib/grus
install -d -o root -g grus -m 750 /etc/grus /etc/grus/cluster

install -m 755 ./grus /usr/local/bin/grus
install -m 644 "$here/grus.service" /etc/systemd/system/grus.service

if [ ! -f /etc/grus/grus.conf ]; then
    # The config holds the SMTP password: readable by the grus user only.
    install -o root -g grus -m 640 "$here/grus.conf.example" /etc/grus/grus.conf
    echo "Installed /etc/grus/grus.conf from the example; review it (domain, mail relay)."
fi

# The first node of a new cluster makes the cluster CA and its own
# certificate. The CA key stays in /root/grus-ca, off the node's config
# dir: it's what admits new members (docs/operations.md, section 1). A node
# joining an existing cluster finds ca.crt already copied in and needs its
# certificate issued by that CA, so it's never given a CA of its own.
node=$(sed -n 's/^node_id *= *//p' /etc/grus/grus.conf | tr -d ' ')
cluster=/etc/grus/cluster
if [ ! -f "$cluster/$node.crt" ]; then
    if [ -f "$cluster/ca.crt" ] || grep -q '^join *=' /etc/grus/grus.conf; then
        echo "No $cluster/$node.crt: issue it with the cluster's CA (grus ca issue $node)" >&2
        echo "and copy $node.crt and $node.key to $cluster, then re-run." >&2
        exit 1
    fi
    ca=/root/grus-ca
    install -d -m 700 "$ca"
    [ -f "$ca/ca.key" ] || /usr/local/bin/grus ca init -dir "$ca"
    [ -f "$ca/$node.crt" ] || /usr/local/bin/grus ca issue -dir "$ca" "$node"
    install -o root -g grus -m 644 "$ca/ca.crt" "$ca/$node.crt" "$cluster/"
    install -o root -g grus -m 640 "$ca/$node.key" "$cluster/"
    echo "Made a cluster CA in $ca (keep ca.key safe; it admits new nodes)."
fi

systemctl daemon-reload
systemctl enable grus
systemctl restart grus
systemctl --no-pager status grus
