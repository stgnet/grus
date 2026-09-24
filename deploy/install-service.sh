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
    echo "Edit /etc/grus/grus.conf, put the cluster certificates in /etc/grus/cluster"
    echo "(see docs/operations.md), then: systemctl enable --now grus"
    systemctl daemon-reload
    exit 0
fi

systemctl daemon-reload
systemctl restart grus
systemctl --no-pager status grus
