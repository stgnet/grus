#!/bin/sh
# Daily consistent copy of every database into a dated directory on the
# NAS, keeping the last 30 days. Run on the Studio (which holds the full
# copy) from cron or launchd, e.g. at 03:30:
#
#   30 3 * * * /usr/local/bin/grus-backup.sh /Volumes/NAS/grus/backups
#
# This is the point-in-time backup. The Studio's live copy isn't one: a
# mistaken delete replicates to it within a second. Monthly copies and the
# encrypted off-site mirror come later (plan section 9).
set -eu

dest=${1:?usage: grus-backup.sh <backup-dir> [config]}
conf=${2:-/etc/grus/grus.conf}
today=$(date -u +%Y-%m-%d)

grus backup -config "$conf" -to "$dest/$today.tmp"
# Rename only when complete, so a half-written copy never looks like a backup.
mv "$dest/$today.tmp" "$dest/$today"

# Keep 30 dailies.
ls -1d "$dest"/????-??-?? 2>/dev/null | sort -r | tail -n +31 | while read -r old; do
    rm -rf "$old"
done
