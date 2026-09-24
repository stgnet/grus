#!/bin/sh
# Installs grus as a launchd service on macOS (the Studio). Run from the
# repository after `make`:
#
#   sudo make install        (or: sudo deploy/install-launchd.sh)
#
# It's safe to re-run: it upgrades the binary and the job and restarts.
#
# The job runs as the user who ran sudo, not a dedicated account. Making a
# hidden service user on macOS takes a page of dscl commands that change
# between releases, and the Studio's node needs no privileges anyway: it
# serves no web ports, only the cluster port (7946), which is above 1024.
# That user owns the data directory and can read the config.
set -eu

here=$(dirname "$0")
label=com.stgnet.grus
plist=/Library/LaunchDaemons/$label.plist
user=${SUDO_USER:-}

if [ "$(id -u)" != 0 ] || [ -z "$user" ] || [ "$user" = root ]; then
    echo "Run this with sudo from your own account: sudo make install" >&2
    exit 1
fi
group=$(id -gn "$user")

# Directories: the config (and cluster certificates) readable by the
# service's user only; the data directory on the Studio's own disk, not the
# NAS, because SQLite needs local file locking.
install -d -o root -g "$group" -m 750 /etc/grus /etc/grus/cluster
install -d -o "$user" -g "$group" -m 750 /usr/local/var/grus
install -d -o "$user" -g "$group" -m 755 /usr/local/var/log/grus
install -d -m 755 /usr/local/bin

install -m 755 ./grus /usr/local/bin/grus

# The job file, with its UserName switched from the placeholder to the
# account that ran sudo.
sed "s|<string>_grus</string>|<string>$user</string>|" "$here/$label.plist" > "$plist"
chown root:wheel "$plist"
chmod 644 "$plist"

if [ ! -f /etc/grus/grus.conf ]; then
    # The config can hold the SMTP password: readable by the service's user only.
    install -o root -g "$group" -m 640 "$here/studio.conf.example" /etc/grus/grus.conf
    echo "Edit /etc/grus/grus.conf, put the cluster certificates in /etc/grus/cluster"
    echo "(see docs/operations.md), then run 'sudo make install' again to start it."
    exit 0
fi

# Restart: unload the old job if one is loaded (ignore "not loaded"), then
# load the new one. launchd starts it at once (RunAtLoad) and restarts it
# if it exits (KeepAlive).
launchctl bootout system/$label 2>/dev/null || true
launchctl bootstrap system "$plist"
echo "Started. Log: /usr/local/var/log/grus/grus.log"
