#!/usr/bin/env bash
# conch-upgrade.sh <tag> <instance>...        upgrade conchd to a release
# conch-upgrade.sh --rollback <instance>...   go back to the release before
#
# Upgrade: download conchd_<tag>_linux_<arch> from the GitHub release, check it
# against checksums.txt, unpack to /opt/conch/releases/<tag>/, take a local
# pre-upgrade backup of every instance and check each one (it exists and
# matches its .sha256) before anything is stopped, then repoint
# /opt/conch/current, restart the instances and check each /healthz reports
# <tag>. What a rollback needs is written to /opt/conch/state/<instance>.
# Pre-upgrade backups are named pre-upgrade-<tag>-<instance>-<time>.tar.gz and
# kept by count, not age (conch-backup.sh), so the nightly retention never
# removes the one a rollback needs.
#
# Rollback: repoint /opt/conch/current at the previous release and restart. If
# an instance's schema version (PRAGMA user_version) changed during the
# upgrade, the old binary would refuse its database ("database schema version
# N is newer than this binary"), so that instance's data directory is replaced
# by the pre-upgrade backup. What the instance wrote since the upgrade is lost
# with it; the replaced directory is kept beside it as <data>.rolled-back-*,
# and the script prints that path. An instance whose version did not change
# keeps its data. Every backup a rollback will need is checked before
# anything is stopped.
#
# As root, everything that touches an instance's data runs as that instance's
# user, conch-<instance> (docs/deploy.md), so no file in it ends up owned by
# root.
#
# All instances share one binary, so name every instance on the droplet.
# Run as root. For tests, every location can be moved:
#   CONCH_OPT (/opt/conch)  CONCH_ETC (/etc/conch)  CONCH_DATA_ROOT (/var/lib/conch)
#   CONCH_BACKUP_DIR (/var/backups/conch)  SYSTEMCTL (systemctl)
#   CONCH_RELEASE_URL (https://github.com/njdaniel/conch/releases/download)
#   CONCH_BACKUP_SCRIPT (conch-backup.sh beside this script)
set -euo pipefail

opt=${CONCH_OPT:-/opt/conch}
etc=${CONCH_ETC:-/etc/conch}
data_root=${CONCH_DATA_ROOT:-/var/lib/conch}
backup_dir=${CONCH_BACKUP_DIR:-/var/backups/conch}
systemctl=${SYSTEMCTL:-systemctl}
release_url=${CONCH_RELEASE_URL:-https://github.com/njdaniel/conch/releases/download}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
backup_script=${CONCH_BACKUP_SCRIPT:-$here/conch-backup.sh}
state=$opt/state
# The users below may not be able to enter the directory this was started in.
cd /

fail() { echo "conch-upgrade: FAIL: $*" >&2; exit 1; }
usage() { echo "usage: $0 <tag> <instance>...   |   $0 --rollback <instance>..." >&2; exit 2; }

[ $# -ge 2 ] || usage

# The loopback address an instance listens on, read from its env file.
listen_of() {
    local l
    l=$(sed -n 's/^CONCHD_LISTEN=//p' "$etc/$1.env" | tail -n 1)
    [ -n "$l" ] || fail "no CONCHD_LISTEN in $etc/$1.env"
    echo "$l"
}

# healthz_version <instance>: wait for /healthz to answer ok and print the
# version it reports.
healthz_version() {
    local listen out
    listen=$(listen_of "$1")
    for _ in $(seq 1 100); do
        if out=$(curl -fsS "http://$listen/healthz" 2>/dev/null); then
            case "$out" in
                *'"status":"ok"'*) printf '%s\n' "$out" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p'; return 0 ;;
            esac
        fi
        sleep 0.3
    done
    return 1
}

# as_instance <instance> <command>...: run it as the instance's user when
# root, and as whoever this is otherwise (the tests). Root without that user
# is refused rather than run as root.
as_instance() {
    local u=conch-$1
    shift
    if [ "$(id -u)" = 0 ]; then
        id "$u" >/dev/null 2>&1 || fail "the user $u does not exist (docs/deploy.md)"
        runuser -u "$u" -- "$@"
    else
        "$@"
    fi
}

# user_version <instance>: the database's schema version. As the service user,
# so reading a stopped WAL database never leaves a root-owned -wal or -shm
# file that conchd then cannot open.
user_version() {
    as_instance "$1" sqlite3 "$data_root/$1/conch.db" 'PRAGMA user_version'
}

# backup_ok <archive>: it exists and matches the .sha256 beside it.
backup_ok() {
    [ -n "$1" ] && [ -f "$1" ] && [ -f "$1.sha256" ] &&
        (cd "$(dirname "$1")" && sha256sum -c --status "$(basename "$1").sha256")
}

validate_names() {
    local i
    for i in "$@"; do
        case "$i" in *[!a-z0-9-]* | -*) fail "bad instance name $i" ;; esac
        [ -f "$etc/$i.env" ] || fail "no $etc/$i.env"
    done
}

if [ "$1" = --rollback ]; then
    shift
    validate_names "$@"
    prev=""
    for i in "$@"; do
        [ -f "$state/$i" ] || fail "no upgrade recorded for $i ($state/$i)"
        previous_release="" previous_tag=""
        # shellcheck disable=SC1090
        . "$state/$i"
        if [ -z "$previous_release" ] || [ -z "$previous_tag" ]; then fail "$state/$i is incomplete"; fi
        if [ -n "$prev" ] && [ "$prev" != "$previous_release" ]; then
            fail "$i was upgraded from a different release than the instances before it; they share one binary, so they cannot be rolled back apart"
        fi
        prev=$previous_release
    done
    [ -d "$prev" ] || fail "the previous release $prev is gone"

    # Before anything is stopped: which instances need their pre-upgrade
    # backup, and is each of those there and intact? conchd migrates only at
    # start, so the version read from the running instance is the one the
    # old binary would meet.
    declare -A restore
    for i in "$@"; do
        backup_before="" schema_version_before=""
        # shellcheck disable=SC1090
        . "$state/$i"
        now=$(user_version "$i")
        if [ "$now" != "$schema_version_before" ]; then
            backup_ok "$backup_before" ||
                fail "$i needs its pre-upgrade backup ($backup_before), which is missing or does not match its .sha256. Nothing was stopped. Find that archive (off-site copies are nightly ones, not this one) or restore a nightly backup by hand (docs/deploy.md, \"Restoring for real\")."
            restore[$i]="$schema_version_before -> $now"
        fi
    done

    for i in "$@"; do "$systemctl" stop "conchd@$i"; done
    ln -sfn "$prev" "$opt/current.new" && mv -T "$opt/current.new" "$opt/current"
    for i in "$@"; do
        backup_before="" previous_tag=""
        # shellcheck disable=SC1090
        . "$state/$i"
        if [ -n "${restore[$i]:-}" ]; then
            echo "conch-upgrade: $i: schema version went ${restore[$i]}; restoring the pre-upgrade backup $backup_before"
            kept=$data_root/$i.rolled-back-$(date -u +%Y%m%dT%H%M%SZ)
            mv "${data_root:?}/$i" "$kept"
            echo "conch-upgrade: $i: the replaced data directory is kept at $kept"
            if [ "$(id -u)" = 0 ]; then
                install -d -m 0700 -o "conch-$i" -g "conch-$i" "$data_root/$i"
            else
                mkdir -m 0700 "$data_root/$i"
            fi
            # Unpacked by the service user from a stream root opens, so every
            # file is the service user's and no owner or mode in the archive
            # is taken from it.
            as_instance "$i" tar --no-same-owner --no-same-permissions -xzf - -C "$data_root/$i" <"$backup_before"
        else
            echo "conch-upgrade: $i: schema version unchanged ($(user_version "$i")); data kept"
        fi
        "$systemctl" start "conchd@$i"
        got=$(healthz_version "$i") || fail "$i did not answer /healthz after the rollback"
        [ "$got" = "$previous_tag" ] || fail "$i reports version $got, expected $previous_tag"
        echo "conch-upgrade: $i is back on $previous_tag"
        rm -f "${state:?}/${i:?}"
    done
    exit 0
fi

tag=$1
shift
case "$tag" in *[!A-Za-z0-9._-]* | '') fail "bad tag $tag" ;; esac
validate_names "$@"
for tool in curl sha256sum sqlite3; do command -v "$tool" >/dev/null || fail "$tool is required"; done

case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) fail "no release for $(uname -m)" ;;
esac
asset=conchd_${tag}_linux_$arch

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl -fsSL -o "$work/$asset" "$release_url/$tag/$asset" || fail "could not download $asset"
curl -fsSL -o "$work/checksums.txt" "$release_url/$tag/checksums.txt" || fail "could not download checksums.txt"
# checksums.txt lists "<sha256>  ./<asset>".
want=$(awk -v a="./$asset" -v b="$asset" '$2 == a || $2 == b { print $1 }' "$work/checksums.txt")
[ -n "$want" ] || fail "$asset is not in checksums.txt"
have=$(sha256sum "$work/$asset" | cut -d' ' -f1)
[ "$want" = "$have" ] || fail "$asset does not match checksums.txt"
echo "conch-upgrade: $asset matches checksums.txt"

release_dir=$opt/releases/$tag
previous_release=$(readlink -f "$opt/current" 2>/dev/null || true)
if [ -z "$previous_release" ] || [ ! -d "$previous_release" ]; then fail "$opt/current does not point at an installed release"; fi
previous_tag=$(basename "$previous_release")

mkdir -p "$opt/releases" "$state"
if [ -e "$release_dir" ]; then
    # Left by an earlier upgrade that was rolled back. Reuse it only if it is
    # the same binary.
    [ "$(sha256sum "$release_dir/conchd" 2>/dev/null | cut -d' ' -f1)" = "$want" ] ||
        fail "$release_dir exists and is not this release; remove it if you are sure"
    [ "$(readlink -f "$opt/current")" != "$(readlink -f "$release_dir")" ] || fail "$tag is already the current release"
else
    mkdir "$release_dir"
    install -m 0755 "$work/$asset" "$release_dir/conchd"
fi

# The pre-upgrade backups, every one made and checked before any instance is
# stopped. The path is the one the backup script says it wrote, so an older
# archive can never stand in for a failed backup.
declare -A before
for i in "$@"; do
    out=$(CONCH_DATA_ROOT=$data_root CONCH_BACKUP_DIR=$backup_dir "$backup_script" "$i" --pre-upgrade "$tag") ||
        fail "the pre-upgrade backup of $i failed. Nothing was stopped; $tag is unpacked in $release_dir but not in use. Fix the backup (run conch-backup.sh $i --local-only as root to see why), then run this upgrade again."
    printf '%s\n' "$out"
    b=$(printf '%s\n' "$out" | sed -n 's/^conch-backup: wrote \(.*\) ([0-9]* bytes)$/\1/p' | tail -n 1)
    backup_ok "$b" ||
        fail "the pre-upgrade backup of $i is missing or does not match its .sha256 (${b:-no archive named}). Nothing was stopped; $tag is unpacked in $release_dir but not in use. Check $backup_dir/$i and the backup (run conch-backup.sh $i --local-only as root), then run this upgrade again."
    before[$i]=$b
done
echo "conch-upgrade: every pre-upgrade backup is there and matches its .sha256"

for i in "$@"; do
    {
        echo "previous_release=$previous_release"
        echo "previous_tag=$previous_tag"
        echo "backup_before=${before[$i]}"
        echo "schema_version_before=$(user_version "$i")"
    } > "$state/$i"
done

for i in "$@"; do "$systemctl" stop "conchd@$i"; done
ln -sfn "$release_dir" "$opt/current.new" && mv -T "$opt/current.new" "$opt/current"
for i in "$@"; do
    "$systemctl" start "conchd@$i"
    got=$(healthz_version "$i") || fail "$i did not answer /healthz on $tag; roll back with: $0 --rollback $*"
    [ "$got" = "$tag" ] || fail "$i reports version $got, expected $tag; roll back with: $0 --rollback $*"
    echo "conch-upgrade: $i runs $tag (schema version $(user_version "$i"))"
done
echo "conch-upgrade: done. To go back: $0 --rollback $*"
