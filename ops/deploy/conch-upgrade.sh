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
# root. Root never opens a file conch-backup owns either: the pre-upgrade
# archives are checked, and read for a restore, as conch-backup. Every step
# run as another user runs in its own session, with no terminal and none of
# root's environment, and what it prints is stripped of control characters.
#
# The rollback record /opt/conch/state/<instance> is never sourced. It is
# written with printf %q and read back by a parser that accepts only the keys
# this script writes, each value checked as the upgrade checked it: the
# archive must be exactly this instance's pre-upgrade backup for that tag, in
# the backup directory.
#
# All instances share one binary, so name every instance on the droplet.
# Run as root. For tests, every location can be moved:
#   CONCH_OPT (/opt/conch)  CONCH_ETC (/etc/conch)  CONCH_DATA_ROOT (/var/lib/conch)
#   CONCH_BACKUP_DIR (/var/backups/conch)  SYSTEMCTL (systemctl)
#   CONCH_RELEASE_URL (https://github.com/njdaniel/conch/releases/download)
#   CONCH_BACKUP_SCRIPT (conch-backup.sh beside this script)
set -euo pipefail
# Character classes such as [0-9] mean ASCII only in the C locale.
export LC_ALL=C

opt=${CONCH_OPT:-/opt/conch}
etc=${CONCH_ETC:-/etc/conch}
data_root=${CONCH_DATA_ROOT:-/var/lib/conch}
backup_dir=${CONCH_BACKUP_DIR:-/var/backups/conch}
systemctl=${SYSTEMCTL:-systemctl}
release_url=${CONCH_RELEASE_URL:-https://github.com/njdaniel/conch/releases/download}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
backup_script=${CONCH_BACKUP_SCRIPT:-$here/conch-backup.sh}
state=$opt/state
safe_path=/usr/sbin:/usr/bin:/sbin:/bin
# The users below may not be able to enter the directory this was started in.
cd /

# clean: only printable ASCII, tab and newline, so nothing another user wrote
# can send escape sequences to root's terminal.
clean() { LC_ALL=C tr -cd '\011\012\040-\176'; }

fail() { printf 'conch-upgrade: FAIL: %s\n' "$*" | clean >&2; exit 1; }
usage() { echo "usage: $0 <tag> <instance>...   |   $0 --rollback <instance>..." >&2; exit 2; }

[ $# -ge 2 ] || usage

# These end up in the rollback record, so they are kept to characters that
# need no quoting.
for d in "$opt" "$backup_dir"; do
    case "$d" in
        /*) ;;
        *) fail "$d is not an absolute path" ;;
    esac
    case "$d" in *[!A-Za-z0-9._/-]*) fail "$d: only letters, digits, '.', '_', '-' and '/' are allowed" ;; esac
done

valid_tag() {
    case "$1" in '' | . | .. | -* | *[!A-Za-z0-9._-]*) return 1 ;; esac
}
valid_int() { [[ $1 =~ ^-?[0-9]+$ ]]; }

# pre_upgrade_path <archive> <instance> <tag>: it is exactly the name
# conch-backup.sh gives a pre-upgrade backup, in this instance's backup
# directory: <backup_dir>/<instance>/pre-upgrade-<tag>-<instance>-<UTC time>.tar.gz.
pre_upgrade_path() {
    local prefix=$backup_dir/$2/pre-upgrade-$3-$2- stamp='^[0-9]{8}T[0-9]{6}Z\.tar\.gz$'
    [ "${1:0:${#prefix}}" = "$prefix" ] && [[ ${1:${#prefix}} =~ $stamp ]]
}

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
                *'"status":"ok"'*) printf '%s\n' "$out" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' | clean; return 0 ;;
            esac
        fi
        sleep 0.3
    done
    return 1
}

# guarded <command>...: run it holding no terminal. Its own session (setsid),
# so root's terminal is not its controlling one; stdin from /dev/null if it is
# a terminal; stdout through clean if it is a terminal; stderr always through
# clean. A file or a pipe on stdin or stdout is passed as it is.
guarded() {
    if [ -t 0 ]; then
        guarded "$@" </dev/null
        return
    fi
    if [ -t 1 ]; then
        guarded "$@" | clean
        return
    fi
    { setsid -w "$@" 2>&1 1>&3 3>&- | clean >&2; } 3>&1
}

# as_user <user> <command>...: as root, run it as that user with none of
# root's environment; otherwise (the tests) as whoever this is. Root without
# that user is refused rather than run as root.
as_user() {
    local u=$1
    shift
    if [ "$(id -u)" = 0 ]; then
        id "$u" >/dev/null 2>&1 || fail "the user $u does not exist (docs/deploy.md)"
        guarded runuser -u "$u" -- env -i "PATH=$safe_path" "$@"
    else
        guarded "$@"
    fi
}
as_instance() {
    local i=$1
    shift
    as_user "conch-$i" "$@"
}
as_backup() { as_user conch-backup "$@"; }

# user_version <instance>: the database's schema version. As the service user,
# so reading a stopped WAL database never leaves a root-owned -wal or -shm
# file that conchd then cannot open.
user_version() {
    local v
    v=$(as_instance "$1" sqlite3 "$data_root/$1/conch.db" 'PRAGMA user_version')
    valid_int "$v" || fail "could not read the schema version of $1"
    echo "$v"
}

# backup_ok <archive>: checked as conch-backup, which owns the archives, so
# root opens neither file. Both are regular files and not symlinks, and the
# .sha256 is exactly one line naming this archive with the SHA-256 it has.
backup_ok() {
    [ -n "$1" ] || return 1
    # shellcheck disable=SC2016 # expanded by the inner bash
    as_backup bash -c '
        a=$1 s=$1.sha256
        if [ ! -f "$a" ] || [ -L "$a" ] || [ ! -f "$s" ] || [ -L "$s" ]; then exit 1; fi
        want=$(cat -- "$s") && have=$(sha256sum <"$a") || exit 1
        [ "$want" = "${have%% *}  ${a##*/}" ]
    ' backup-check "$1"
}

# read_archive <archive>: its bytes on stdout, read by conch-backup.
read_archive() {
    # shellcheck disable=SC2016 # expanded by the inner bash
    as_backup bash -c '[ ! -L "$1" ] && exec cat -- "$1"' read-archive "$1"
}

validate_names() {
    local i
    for i in "$@"; do
        case "$i" in *[!a-z0-9-]* | -*) fail "bad instance name $i" ;; esac
        [ -f "$etc/$i.env" ] || fail "no $etc/$i.env"
    done
}

# read_state <instance>: the rollback record, parsed, never sourced. Sets
# upgrade_tag, previous_release, previous_tag, backup_before and
# schema_version_before, or fails. Every line must be key=value with one of
# those keys, each once, and every value must be what the upgrade would have
# written; anything else and the record is refused.
read_state() {
    local i=$1 f=$state/$1 line key n=0 releases_real
    if [ ! -f "$f" ] || [ -L "$f" ]; then fail "no upgrade recorded for $i ($f)"; fi
    [ "$(stat -c %u "$f")" = "$(id -u)" ] || fail "$f is not owned by $(id -un); refusing it. Nothing was stopped."
    upgrade_tag="" previous_release="" previous_tag="" backup_before="" schema_version_before=""
    while IFS= read -r line || [ -n "$line" ]; do
        key=${line%%=*}
        case "$key" in
            upgrade_tag | previous_release | previous_tag | backup_before | schema_version_before) ;;
            *) fail "$f holds a line this script does not write; refusing it. Nothing was stopped." ;;
        esac
        if [ "$key" = "$line" ] || [ -n "${!key}" ]; then
            fail "$f holds a key twice or a line without a value; refusing it. Nothing was stopped."
        fi
        printf -v "$key" '%s' "${line#*=}"
        n=$((n + 1))
    done <"$f"
    releases_real=$(readlink -f "$opt/releases")
    if [ "$n" != 5 ] ||
        ! valid_tag "$upgrade_tag" || ! valid_tag "$previous_tag" ||
        [ "$previous_release" != "$releases_real/$previous_tag" ] ||
        ! pre_upgrade_path "$backup_before" "$i" "$upgrade_tag" ||
        ! valid_int "$schema_version_before"; then
        fail "$f is not a rollback record this script wrote; refusing it. Nothing was stopped."
    fi
}

if [ "$1" = --rollback ]; then
    command -v setsid >/dev/null || fail "setsid is required"
    shift
    validate_names "$@"
    prev=""
    for i in "$@"; do
        read_state "$i"
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
        read_state "$i"
        now=$(user_version "$i")
        if [ "$now" != "$schema_version_before" ]; then
            backup_ok "$backup_before" ||
                fail "$i needs its pre-upgrade backup ($backup_before), which is missing, is a symlink, or does not match its .sha256. Nothing was stopped. Find that archive (off-site copies are nightly ones, not this one) or restore a nightly backup by hand (docs/deploy.md, \"Restoring for real\")."
            restore[$i]="$schema_version_before -> $now"
        fi
    done

    for i in "$@"; do "$systemctl" stop "conchd@$i"; done
    ln -sfn "$prev" "$opt/current.new" && mv -T "$opt/current.new" "$opt/current"
    for i in "$@"; do
        read_state "$i"
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
            # Read by conch-backup and unpacked by the service user from that
            # stream: root never opens the archive, every file is the service
            # user's, and no owner or mode in the archive is taken from it.
            read_archive "$backup_before" |
                as_instance "$i" tar --no-same-owner --no-same-permissions -xzf - -C "$data_root/$i" ||
                fail "$i: restoring $backup_before failed; the replaced data is kept (see above)"
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
valid_tag "$tag" || fail "bad tag $tag"
validate_names "$@"
for tool in curl sha256sum sqlite3 setsid; do command -v "$tool" >/dev/null || fail "$tool is required"; done

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
if ! valid_tag "$previous_tag" || [ "$previous_release" != "$(readlink -f "$opt/releases")/$previous_tag" ]; then
    fail "$opt/current points at $previous_release, which is not a release directory in $opt/releases"
fi
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
# archive can never stand in for a failed backup. That output comes from the
# conch-backup user, so the path is accepted only if it is exactly the name a
# pre-upgrade backup of this instance for this tag has, in the backup
# directory; anything else, whatever it holds, stops the upgrade.
declare -A before
for i in "$@"; do
    out=$(CONCH_DATA_ROOT=$data_root CONCH_BACKUP_DIR=$backup_dir "$backup_script" "$i" --pre-upgrade "$tag") ||
        fail "the pre-upgrade backup of $i failed. Nothing was stopped; $tag is unpacked in $release_dir but not in use. Fix the backup (run conch-backup.sh $i --local-only as root to see why), then run this upgrade again."
    printf '%s\n' "$out" | clean
    b=$(printf '%s\n' "$out" | sed -n 's/^conch-backup: wrote \(.*\) ([0-9]* bytes)$/\1/p' | tail -n 1)
    pre_upgrade_path "$b" "$i" "$tag" ||
        fail "the pre-upgrade backup of $i named ${b:-no archive}, which is not $backup_dir/$i/pre-upgrade-$tag-$i-<time>.tar.gz. Nothing was stopped; $tag is unpacked in $release_dir but not in use. Check $backup_dir/$i and the backup (run conch-backup.sh $i --local-only as root), then run this upgrade again."
    backup_ok "$b" ||
        fail "the pre-upgrade backup of $i ($b) is missing, is a symlink, or does not match its .sha256. Nothing was stopped; $tag is unpacked in $release_dir but not in use. Check $backup_dir/$i and the backup (run conch-backup.sh $i --local-only as root), then run this upgrade again."
    before[$i]=$b
done
echo "conch-upgrade: every pre-upgrade backup is there and matches its .sha256"

# The rollback record: written with %q (every value is already one that
# needs no quoting), read back by read_state, never sourced.
for i in "$@"; do
    sv=$(user_version "$i")
    {
        printf 'upgrade_tag=%q\n' "$tag"
        printf 'previous_release=%q\n' "$previous_release"
        printf 'previous_tag=%q\n' "$previous_tag"
        printf 'backup_before=%q\n' "${before[$i]}"
        printf 'schema_version_before=%q\n' "$sv"
    } >"$state/$i.new"
    mv "$state/$i.new" "$state/$i"
    read_state "$i"
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
