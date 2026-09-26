#!/usr/bin/env bash
# make install: everything it takes to have this machine running as a Grus
# node, from a fresh Ubuntu (or Debian) or macOS machine with nothing on it,
# or over a node already running an older version. It's the only command
# an operator runs; everything else is on the site's admin page.
#
#   git clone https://github.com/stgnet/grus && cd grus && make install
#   (later)  git pull && make install
#
# What it does, each step skipped when it's already done:
#
#  1. installs the packages it needs, and Go (the official release: the
#     one in apt is often too old), under /usr/local/go;
#  2. builds grus (as you, not root);
#  3. the first time on this machine, sets it up: asks which existing
#     machine to copy the setup from (over your own ssh login: the site's
#     cluster certificate authority and where to join), or, with none, sets
#     up the first node of a new site (asks its domain and operator email);
#     then writes /etc/grus/grus.conf;
#  4. installs the binary and the service (systemd, or launchd on macOS);
#  5. offers to set this machine up to send the site's email itself;
#  6. restarts the service and waits until the node answers on its cluster
#     port as itself, then says where the site is.
#
# DNS and mail records are checked on the admin page (DNS and mail), which
# shows what each should be.
#
# Answers can come from the environment instead of prompts (for a run
# with no terminal): GRUS_FROM (the machine to copy from; "-" for a new
# site), GRUS_DOMAIN, GRUS_OPERATOR, GRUS_ADVERTISE (normally found by the
# node itself), GRUS_FULL (y/n),
# GRUS_WEB (y/n), GRUS_MAIL (y/n), GRUS_MAIL_DOMAIN.
#
# Security: nothing secret is in the repository. The cluster CA's key
# lives only on the site's nodes, and a new node gets it over ssh with the
# operator's own login to an existing one, so having the source gets
# nobody into the cluster.
set -euo pipefail

cd "$(dirname "$0")/.."
os=$(uname -s)
conf=/etc/grus/grus.conf

say() { printf '\n==> %s\n' "$*"; }
die() { printf '\nmake install: %s\n' "$*" >&2; exit 1; }

# Root for the steps that need it, whether or not this runs under sudo.
if [ "$(id -u)" = 0 ]; then
    as_root() { "$@"; }
    user=${SUDO_USER:-root}
else
    command -v sudo >/dev/null || die "needs sudo (or run it as root)"
    as_root() { sudo "$@"; }
    user=$(id -un)
fi
# The build runs as the person installing, so root never owns files in
# their Go caches.
as_user() {
    if [ "$(id -un)" = "$user" ]; then "$@"; else sudo -u "$user" -H "$@"; fi
}

# ask VAR "question" "default": the environment's GRUS_... value, or the
# answer typed, or the default.
ask() {
    local var=$1 q=$2 def=${3:-} env="GRUS_$1" ans
    if [ -n "${!env:-}" ]; then
        ans=${!env}
    elif [ -t 0 ]; then
        read -r -p "$q${def:+ [$def]}: " ans
    else
        ans=
    fi
    ans=${ans:-$def}
    [ "$ans" = "-" ] && ans=
    printf -v "$var" '%s' "$ans"
}
yes() { case "$1" in [yY]*) return 0 ;; *) return 1 ;; esac; }

case "$os" in
Linux)
    command -v apt-get >/dev/null || die "this Linux has no apt-get: make install knows Ubuntu and Debian"
    data=/var/lib/grus
    svc_user=grus
    ;;
Darwin)
    data=/usr/local/var/grus
    svc_user=$user
    [ "$svc_user" != root ] || die "on macOS, run it from your own account (the service runs as you)"
    ;;
*) die "make install knows Linux (Ubuntu, Debian) and macOS, not $os" ;;
esac

# --- 1. packages and Go ----------------------------------------------------

if [ "$os" = Linux ]; then
    say "Packages"
    missing=
    for p in ca-certificates curl git tar openssh-client; do
        dpkg -s "$p" >/dev/null 2>&1 || missing="$missing $p"
    done
    if [ -n "$missing" ]; then
        as_root apt-get update -qq
        as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq $missing >/dev/null
    fi
fi

# Go: any Go from 1.21 on fetches the exact version go.mod asks for by
# itself; without one (or with an older one), install the official release
# of that version.
want=$(sed -n 's/^go //p' go.mod)
export PATH=/usr/local/go/bin:$PATH
have=$(go env GOVERSION 2>/dev/null | sed 's/^go//' || true)
if [ -z "$have" ] || [ "$(printf '%s\n' 1.21 "$have" | sort -V | head -1)" != 1.21 ]; then
    say "Go $want"
    case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "no Go download for $(uname -m)" ;;
    esac
    goos=$(echo "$os" | tr '[:upper:]' '[:lower:]')
    tmp=$(mktemp -d)
    curl -fsSL "https://go.dev/dl/go$want.$goos-$arch.tar.gz" -o "$tmp/go.tgz"
    as_root rm -rf /usr/local/go
    as_root tar -C /usr/local -xzf "$tmp/go.tgz"
    rm -rf "$tmp"
fi

# --- 2. build --------------------------------------------------------------

say "Building"
version=$(git describe --always --dirty 2>/dev/null || echo dev)
as_user env PATH="$PATH" CGO_ENABLED=0 go build -ldflags "-X main.version=$version" -o grus ./cmd/grus

# --- 3. first-time setup ----------------------------------------------------

as_root install -d -m 755 /etc/grus
new_site=
first_time=
mail_domain=
if ! as_root test -f "$conf"; then
    first_time=1
    say "Setting up this machine"
    echo "Give the name of a machine already running this site (as you'd ssh to it)"
    echo "to copy the setup from, or leave it empty to start a new site here."
    ask FROM "Copy the setup from" ""
    node=$(hostname -s 2>/dev/null || hostname | cut -d. -f1)
    node=$(echo "$node" | tr '[:upper:]' '[:lower:]')
    web_default=y
    [ "$os" = Darwin ] && web_default=n

    tmpcerts=$(mktemp -d)
    trap 'rm -rf "$tmpcerts"' EXIT
    lines=()
    if [ -z "$FROM" ]; then
        new_site=1
        ask DOMAIN "The site's domain (its DNS goes to this machine)" ""
        [ -n "$DOMAIN" ] || die "a new site needs a domain"
        ask OPERATOR "Your email (you'll sign in with it to reach the admin page)" ""
        [ -n "$OPERATOR" ] || die "a new site needs an operator's email"
        lines+=("domain = $DOMAIN" "operator = $OPERATOR")
        mail_domain=$DOMAIN
        WEB=y
        FULL=n
        # The service makes the site's cluster CA and this node's
        # certificate the first time it starts.
    else
        # From the other machine: the cluster CA (the service makes this
        # node's own certificate from it), and its address to join through,
        # packed up there by deploy/remote-setup.sh.
        echo "Copying the setup from $FROM (over ssh; it may ask for your sudo password there)"
        scp -q deploy/remote-setup.sh "$FROM:/tmp/grus-remote-setup.sh"
        ssh -t "$FROM" 'sudo sh /tmp/grus-remote-setup.sh; status=$?; rm -f /tmp/grus-remote-setup.sh; exit $status' ||
            die "couldn't get the setup from $FROM"
        scp -q "$FROM:/tmp/grus-setup.tar" "$tmpcerts/setup.tar"
        ssh "$FROM" 'rm -f /tmp/grus-setup.tar'
        tar -C "$tmpcerts" -xf "$tmpcerts/setup.tar"
        while read -r j; do
            [ -n "$j" ] && lines+=("join = $j")
        done <"$tmpcerts/join"
        ask FULL "Hold a full copy of every group, like the Studio? (y/n)" n
        ask WEB "Serve the site's pages from this machine? (y/n)" "$web_default"
    fi

    lines+=("node_id = $node")
    # Where the other nodes reach this one is found by the node itself
    # (internal/cluster/addr.go); GRUS_ADVERTISE sets it instead.
    [ -n "${GRUS_ADVERTISE:-}" ] && lines+=("advertise = $GRUS_ADVERTISE")
    yes "$FULL" && lines+=("full = true")
    if yes "$WEB"; then
        lines+=("voter = true" "http_addr = :80" "https_addr = :443")
    fi
    # A model on this machine (Ollama): searches and background work go to
    # it. Which model is a global setting on the admin page.
    if curl -fs --noproxy "*" --max-time 2 http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then
        lines+=("ai_url = http://127.0.0.1:11434")
    fi

    {
        echo "# grus.conf, written by make install. Only what's particular to this"
        echo "# machine is here; everything else is on the site's admin page."
        echo "# See deploy/grus.conf.example for every key."
        printf '%s\n' "${lines[@]}"
    } >"$tmpcerts/grus.conf"
    as_root install -m 640 "$tmpcerts/grus.conf" "$conf"
    if [ -f "$tmpcerts/ca.key" ]; then
        as_root install -d -m 700 "$data" "$data/cluster"
        as_root install -m 644 "$tmpcerts/ca.crt" "$data/cluster/ca.crt"
        as_root install -m 600 "$tmpcerts/ca.key" "$data/cluster/ca.key"
    fi
fi

# --- 4. binary and service ---------------------------------------------------

say "Installing the service"
as_root install -d -m 755 /usr/local/bin
as_root install -m 755 ./grus /usr/local/bin/grus
if [ "$os" = Linux ]; then
    id grus >/dev/null 2>&1 || as_root useradd --system --home-dir "$data" --shell /usr/sbin/nologin grus
    as_root install -d -o grus -g grus -m 750 "$data"
    as_root chown -R grus:grus "$data"
    as_root chown root:grus /etc/grus "$conf"
    as_root chmod 750 /etc/grus
    as_root chmod 640 "$conf"
    as_root install -m 644 deploy/grus.service /etc/systemd/system/grus.service
    as_root systemctl daemon-reload
    as_root systemctl enable -q grus
else
    group=$(id -gn "$svc_user")
    label=com.stgnet.grus
    plist=/Library/LaunchDaemons/$label.plist
    as_root install -d -o "$svc_user" -g "$group" -m 750 "$data"
    as_root chown -R "$svc_user:$group" "$data"
    as_root install -d -o "$svc_user" -g "$group" -m 755 /usr/local/var/log/grus
    as_root chown "root:$group" /etc/grus "$conf"
    as_root chmod 750 /etc/grus
    as_root chmod 640 "$conf"
    sed "s|<string>_grus</string>|<string>$svc_user</string>|" deploy/$label.plist | as_root tee "$plist" >/dev/null
    as_root chown root:wheel "$plist"
    as_root chmod 644 "$plist"
fi

# --- 5. mail -------------------------------------------------------------------

# This machine can send the site's email itself: Postfix with DKIM for a
# domain (deploy/install-mail.sh). A new site's first node is asked for its
# domain; another node that serves pages is asked which domain, if any.
# On a machine that's set up already, GRUS_MAIL_DOMAIN=<domain> make
# install sets it up for one more.
if [ "$os" = Linux ]; then
    if [ -n "${GRUS_MAIL_DOMAIN:-}" ]; then
        mail_domain=$GRUS_MAIL_DOMAIN
    elif [ -n "$new_site" ]; then
        ask MAIL "Send the site's email from this machine (Postfix with DKIM)? (y/n)" y
        yes "$MAIL" || mail_domain=
    elif [ -n "$first_time" ] && yes "${WEB:-n}"; then
        ask MAIL_DOMAIN "Send email from this machine for which domain? (empty: don't)" ""
        mail_domain=$MAIL_DOMAIN
    fi
    if [ -n "$mail_domain" ]; then
        as_root sh deploy/install-mail.sh "$mail_domain"
    fi
fi

# --- 6. start, and check it's running --------------------------------------

say "Starting"
if [ "$os" = Linux ]; then
    as_root systemctl restart grus
else
    as_root launchctl bootout system/com.stgnet.grus 2>/dev/null || true
    as_root launchctl bootstrap system /Library/LaunchDaemons/com.stgnet.grus.plist
fi

# The node is up when it answers on its cluster port as itself. Ask with
# its own certificate (the service makes it on first start, so wait for it).
setting() { as_root sed -n "s/^$1 *= *//p" "$conf" | tr -d ' ' | tail -1; }
node=$(setting node_id)
[ -n "$node" ] || node=$(hostname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')
ca=$(setting tls_ca); ca=${ca:-$data/cluster/ca.crt}
crt=$(setting tls_cert); crt=${crt:-$data/cluster/node.crt}
key=$(setting tls_key); key=${key:-$data/cluster/node.key}
port=$(setting cluster_addr | sed 's/.*://'); port=${port:-7946}
ok=
for _ in $(seq 60); do
    if as_root test -f "$crt" &&
        as_root curl -fs --noproxy "*" --max-time 3 --cacert "$ca" --cert "$crt" --key "$key" \
            --resolve "grus-node:$port:127.0.0.1" "https://grus-node:$port/sync/report" 2>/dev/null |
        grep -q "\"id\":\"$node\""; then
        ok=1
        break
    fi
    sleep 1
done
if [ -z "$ok" ]; then
    echo "The node didn't come up. Its log:" >&2
    if [ "$os" = Linux ]; then
        as_root journalctl -u grus -n 30 --no-pager >&2 || true
    else
        tail -n 30 /usr/local/var/log/grus/grus.log >&2 || true
    fi
    die "the service isn't answering yet (see the log above)"
fi

say "Grus $version is running on this machine as node $node"
domains=$(setting domain)
if [ -n "$domains" ]; then
    echo "The site: https://$domains/ (sign in with the operator email to reach /admin)."
fi
echo "The admin page's DNS and mail section shows any record that still needs setting."
