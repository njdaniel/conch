#!/usr/bin/env bash
# conch-backup.sh <instance> [--local-only] [--pre-upgrade <tag>]
# — back up one conchd instance.
#
# Makes one dated archive of the instance's database and uploaded files, with
# a SHA-256 beside it, keeps it locally for CONCH_BACKUP_KEEP_DAYS days, and
# copies it off the host with rclone or s3cmd when one is configured.
#
# It is safe while conchd runs: the database is copied with SQLite's online
# backup (`sqlite3 .backup`), which takes a consistent snapshot of a database
# in WAL mode. The files come second, on purpose. A file's bytes are on disk
# before the row that names it, and files are never deleted, so every row in
# the snapshot has its file in the archive; the reverse order could archive a
# row whose file was missing.
#
# Who does what. Run as root (as conch-backup@.service does), the work is
# split so that no user holds more than its step needs:
#   conch-<instance>  takes the snapshot. conchd keeps the database and the
#                     uploads private to its user (mode 0600), so only that
#                     user can read them, and only its sqlite3 can open the
#                     live WAL database without leaving a -wal or -shm file
#                     conchd cannot use.
#   conch-backup      writes the archive, keeps it, and copies it off-site. It
#                     alone holds the off-site settings and credentials
#                     (backup.env, rclone.conf) and never reads the live data.
#   root              only hands the snapshot from one to the other, and reads
#                     no file either of them owns.
# Each step runs in its own session with no terminal and none of root's
# environment (only PATH, CONCH_DATA_ROOT, CONCH_BACKUP_DIR and
# CONCH_BACKUP_ENV are passed), and what it prints is stripped of control
# characters before it reaches root's terminal or the journal.
# Run as any other user, both steps run as that user (the tests do this).
#
# Without an off-site destination the archive is still made, but the script
# says that no off-site copy exists and exits 3, so a timer shows as failed.
# --local-only says that is intended (the tests use it).
#
# --pre-upgrade <tag> is for conch-upgrade.sh: a local-only archive named
# pre-upgrade-<tag>-<instance>-<time>.tar.gz, which the nightly retention
# (by age, conch-<instance>-*) never matches. Pre-upgrade archives are kept by
# count instead: the newest CONCH_BACKUP_KEEP_PRE_UPGRADE (default 3) of each
# instance, so the one a rollback needs is never aged out.
#
# Settings, read by the conch-backup step from /etc/conch/backup.env (mode
# 0600, owned by root or by the user running the step; see docs/deploy.md),
# or from the environment (run as root, only CONCH_BACKUP_DIR reaches it):
#   CONCH_BACKUP_DIR        default /var/backups/conch
#   CONCH_BACKUP_KEEP_DAYS  default 14
#   CONCH_BACKUP_KEEP_PRE_UPGRADE  default 3
#   CONCH_BACKUP_RCLONE_DEST  e.g. conch-crypt:conch   (rclone remote path)
#   CONCH_BACKUP_S3CMD_DEST   e.g. s3://my-bucket/conch (s3cmd)
# rclone and s3cmd read their own credentials (RCLONE_CONFIG, ~/.s3cfg); keep
# those in files of mode 0600 too, never in this repository.
# CONCH_DATA_ROOT (default /var/lib/conch) comes from the environment only.
set -euo pipefail

usage() { echo "usage: $0 <instance> [--local-only] [--pre-upgrade <tag>]" >&2; exit 2; }
die() { echo "conch-backup: $1" >&2; exit "${2:-1}"; }

instance=""
local_only=0
pre_upgrade=""
step=""
while [ $# -gt 0 ]; do
    case "$1" in
        --local-only) local_only=1 ;;
        --pre-upgrade) [ $# -ge 2 ] || usage; pre_upgrade=$2; shift ;;
        # Internal: one step, run by this script as the user it belongs to.
        --step=snapshot | --step=store) step=${1#--step=} ;;
        -*) usage ;;
        *) [ -z "$instance" ] || usage; instance=$1 ;;
    esac
    shift
done
[ -n "$instance" ] || usage
case "$instance" in
    *[!a-z0-9-]* | -*) die "instance must be lower-case letters, digits and '-'" 2 ;;
esac
if [ -n "$pre_upgrade" ]; then
    case "$pre_upgrade" in *[!A-Za-z0-9._-]* | -*) die "bad tag $pre_upgrade" 2 ;; esac
    local_only=1
fi

data=${CONCH_DATA_ROOT:-/var/lib/conch}/$instance
db=$data/conch.db

# snapshot: as the instance's user. A gzip'd tar of a consistent copy of the
# database and of files/, on stdout; everything said goes to stderr.
snapshot() {
    command -v sqlite3 >/dev/null || die "sqlite3 is required (apt install sqlite3)" 2
    [ -f "$db" ] || die "no database at $db" 2
    umask 077
    local stage check status
    stage=$(mktemp -d)
    # shellcheck disable=SC2064 # expand now: stage is local
    trap "rm -rf '$stage'" EXIT
    sqlite3 -cmd '.timeout 10000' "$db" ".backup '$stage/conch.db'" >&2
    check=$(sqlite3 "$stage/conch.db" 'PRAGMA integrity_check')
    [ "$check" = ok ] || die "the copied database failed its integrity check: $check"
    # Uploads are written to .upload-* first and renamed; a half-written one
    # is not part of any row.
    local tar_args=(--exclude='.upload-*' -C "$stage" conch.db)
    if [ -d "$data/files" ]; then
        tar_args+=(-C "$data" files)
    fi
    # tar exits 1 when a file changed while it was read; that is a warning,
    # not a failed archive (the result is read back by the store step).
    status=0
    tar -czf - "${tar_args[@]}" || status=$?
    [ "$status" -le 1 ] || die "tar failed (status $status)"
}

# store: as conch-backup. Reads the snapshot on stdin; writes, keeps and
# copies off-site.
store() {
    local env_file mode owner
    env_file=${CONCH_BACKUP_ENV:-/etc/conch/backup.env}
    if [ -e "$env_file" ]; then
        mode=$(stat -c %a "$env_file")
        owner=$(stat -c %u "$env_file")
        [ "$mode" = 600 ] || die "$env_file is mode $mode; it holds credentials, make it 0600" 2
        if [ "$owner" != 0 ] && [ "$owner" != "$(id -u)" ]; then
            die "$env_file is owned by uid $owner; it is sourced, so it must be owned by root or by $(id -un)" 2
        fi
        set -a
        # shellcheck disable=SC1090
        . "$env_file"
        set +a
    fi

    local backup_dir keep_days keep_pre out_dir stamp archive
    backup_dir=${CONCH_BACKUP_DIR:-/var/backups/conch}
    keep_days=${CONCH_BACKUP_KEEP_DAYS:-14}
    keep_pre=${CONCH_BACKUP_KEEP_PRE_UPGRADE:-3}
    case "$keep_days" in '' | *[!0-9]*) die "CONCH_BACKUP_KEEP_DAYS must be a number" 2 ;; esac
    case "$keep_pre" in '' | 0 | *[!0-9]*) die "CONCH_BACKUP_KEEP_PRE_UPGRADE must be a number above 0" 2 ;; esac

    umask 077
    out_dir=$backup_dir/$instance
    mkdir -p "$out_dir"
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    if [ -n "$pre_upgrade" ]; then
        archive=$out_dir/pre-upgrade-$pre_upgrade-$instance-$stamp.tar.gz
    else
        archive=$out_dir/conch-$instance-$stamp.tar.gz
    fi
    # shellcheck disable=SC2064 # expand now: archive is local
    trap "rm -f '$archive.part'" EXIT
    cat >"$archive.part"
    # grep without -q reads to the end, so tar never meets a closed pipe.
    tar -tzf "$archive.part" 2>/dev/null | grep -x conch.db >/dev/null ||
        die "the snapshot does not read back as an archive holding conch.db"
    mv "$archive.part" "$archive"
    (cd "$out_dir" && sha256sum "$(basename "$archive")" >"$(basename "$archive").sha256")
    echo "conch-backup: wrote $archive ($(stat -c %s "$archive") bytes)"

    # Local retention. Only this instance's archives, and only ones this
    # script made. Nightly ones by age; pre-upgrade ones by count, newest kept.
    find "$out_dir" -maxdepth 1 -type f \( -name "conch-$instance-*.tar.gz" -o -name "conch-$instance-*.tar.gz.sha256" \) \
        -mtime "+$keep_days" -print -delete | sed 's/^/conch-backup: removed old /'
    local pre=() old
    # shellcheck disable=SC2012 # names this script wrote: no newlines
    mapfile -t pre < <(ls -1t "$out_dir"/pre-upgrade-*-"$instance"-*.tar.gz 2>/dev/null || true)
    for old in "${pre[@]:keep_pre}"; do
        rm -f "$old" "$old.sha256"
        echo "conch-backup: removed old $old"
    done

    if [ -n "$pre_upgrade" ]; then
        echo "conch-backup: pre-upgrade backup, kept on this host only"
    elif [ -n "${CONCH_BACKUP_RCLONE_DEST:-}" ]; then
        command -v rclone >/dev/null || die "CONCH_BACKUP_RCLONE_DEST is set but rclone is not installed"
        rclone copy "$archive" "$CONCH_BACKUP_RCLONE_DEST/$instance/"
        rclone copy "$archive.sha256" "$CONCH_BACKUP_RCLONE_DEST/$instance/"
        echo "conch-backup: copied off-site with rclone to $CONCH_BACKUP_RCLONE_DEST/$instance/"
    elif [ -n "${CONCH_BACKUP_S3CMD_DEST:-}" ]; then
        command -v s3cmd >/dev/null || die "CONCH_BACKUP_S3CMD_DEST is set but s3cmd is not installed"
        s3cmd put "$archive" "$archive.sha256" "$CONCH_BACKUP_S3CMD_DEST/$instance/"
        echo "conch-backup: copied off-site with s3cmd to $CONCH_BACKUP_S3CMD_DEST/$instance/"
    elif [ "$local_only" = 1 ]; then
        echo "conch-backup: local only, as asked (--local-only); no off-site copy was made"
    else
        echo "conch-backup: NO OFF-SITE COPY WAS MADE: set CONCH_BACKUP_RCLONE_DEST or CONCH_BACKUP_S3CMD_DEST (docs/deploy.md). The archive is on this host only." >&2
        exit 3
    fi
}

case "$step" in
    snapshot) snapshot; exit 0 ;;
    store) store; exit 0 ;;
esac

# The whole backup: the snapshot into a file only this process can read, then
# the store step reading it on stdin.
self=$(readlink -f "$0")
store_args=("$instance" --step=store)
[ "$local_only" = 0 ] || store_args+=(--local-only)
[ -z "$pre_upgrade" ] || store_args+=(--pre-upgrade "$pre_upgrade")

umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
snap=$work/snapshot.tar.gz

# clean: only printable ASCII, tab and newline, so a step cannot send escape
# sequences to whoever reads this output.
clean() { LC_ALL=C tr -cd '\011\012\040-\176'; }

# guarded <command>...: run it holding no terminal. Its own session (setsid),
# so the terminal is not its controlling one; stdin from /dev/null if it is a
# terminal; stdout through clean if it is a terminal; stderr always through
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

if [ "$(id -u)" = 0 ]; then
    for u in "conch-$instance" conch-backup; do
        id "$u" >/dev/null 2>&1 || die "run as root, this needs the users conch-$instance and conch-backup; $u does not exist (docs/deploy.md)" 2
    done
    # Only these reach a step from root's environment.
    step_env=(PATH=/usr/sbin:/usr/bin:/sbin:/bin)
    for v in CONCH_DATA_ROOT CONCH_BACKUP_DIR CONCH_BACKUP_ENV; do
        [ -z "${!v:-}" ] || step_env+=("$v=${!v}")
    done
    # as <user> <command>...: drop to that user, its groups and no capability,
    # with none of root's environment and no terminal. setpriv, not runuser:
    # no PAM session, which a sandboxed unit may not allow.
    as() {
        local u=$1
        shift
        guarded setpriv --reuid="$u" --regid="$(id -g "$u")" --init-groups --inh-caps=-all -- \
            env -i "${step_env[@]}" "$@"
    }
    cd /
    as "conch-$instance" "$self" "$instance" --step=snapshot >"$snap" </dev/null
    as conch-backup "$self" "${store_args[@]}" <"$snap"
else
    guarded "$self" "$instance" --step=snapshot >"$snap" </dev/null
    guarded "$self" "${store_args[@]}" <"$snap"
fi
