#!/usr/bin/env bash
# conch-backup.sh <instance> [--local-only] — back up one conchd instance.
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
# Without an off-site destination the archive is still made, but the script
# says that no off-site copy exists and exits 3, so a timer shows as failed.
# --local-only says that is intended (the upgrade script and tests use it).
#
# Settings, from the environment or from /etc/conch/backup.env (mode 0600,
# read by this script; see docs/deploy.md):
#   CONCH_DATA_ROOT         default /var/lib/conch
#   CONCH_BACKUP_DIR        default /var/backups/conch
#   CONCH_BACKUP_KEEP_DAYS  default 14
#   CONCH_BACKUP_RCLONE_DEST  e.g. spaces:my-bucket/conch   (rclone remote path)
#   CONCH_BACKUP_S3CMD_DEST   e.g. s3://my-bucket/conch     (s3cmd)
# rclone and s3cmd read their own credentials (RCLONE_CONFIG, ~/.s3cfg); keep
# those in files of mode 0600 too, never in this repository.
set -euo pipefail

usage() { echo "usage: $0 <instance> [--local-only]" >&2; exit 2; }

instance=""
local_only=0
for arg in "$@"; do
    case "$arg" in
        --local-only) local_only=1 ;;
        -*) usage ;;
        *) [ -z "$instance" ] || usage; instance=$arg ;;
    esac
done
[ -n "$instance" ] || usage
case "$instance" in
    *[!a-z0-9-]* | -*) echo "conch-backup: instance must be lower-case letters, digits and '-'" >&2; exit 2 ;;
esac

# Run as the service user, so a database opened here never leaves a -wal or
# -shm file that conchd then cannot use.
if [ "$(id -u)" = 0 ] && [ -z "${CONCH_BACKUP_AS_ROOT:-}" ] && id conch >/dev/null 2>&1; then
    exec runuser -u conch -- "$0" "$@"
fi

env_file=${CONCH_BACKUP_ENV:-/etc/conch/backup.env}
if [ -e "$env_file" ]; then
    mode=$(stat -c %a "$env_file")
    if [ "$mode" != 600 ]; then
        echo "conch-backup: $env_file is mode $mode; it holds credentials, make it 0600" >&2
        exit 2
    fi
    set -a
    # shellcheck disable=SC1090
    . "$env_file"
    set +a
fi

data_root=${CONCH_DATA_ROOT:-/var/lib/conch}
backup_dir=${CONCH_BACKUP_DIR:-/var/backups/conch}
keep_days=${CONCH_BACKUP_KEEP_DAYS:-14}
data=$data_root/$instance
db=$data/conch.db

command -v sqlite3 >/dev/null || { echo "conch-backup: sqlite3 is required (apt install sqlite3)" >&2; exit 2; }
[ -f "$db" ] || { echo "conch-backup: no database at $db" >&2; exit 2; }
case "$keep_days" in '' | *[!0-9]*) echo "conch-backup: CONCH_BACKUP_KEEP_DAYS must be a number" >&2; exit 2 ;; esac

umask 077
out_dir=$backup_dir/$instance
mkdir -p "$out_dir"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
archive=$out_dir/conch-$instance-$stamp.tar.gz
stage=$(mktemp -d "$out_dir/.stage.XXXXXX")
trap 'rm -rf "$stage"' EXIT

sqlite3 -cmd '.timeout 10000' "$db" ".backup '$stage/conch.db'"
check=$(sqlite3 "$stage/conch.db" 'PRAGMA integrity_check')
[ "$check" = ok ] || { echo "conch-backup: the copied database failed its integrity check: $check" >&2; exit 1; }

# Uploads are written to .upload-* first and renamed; a half-written one is
# not part of any row.
tar_args=(--exclude='.upload-*' -C "$stage" conch.db)
if [ -d "$data/files" ]; then
    tar_args+=(-C "$data" files)
fi
# tar exits 1 when a file changed while it was read; that is a warning, not a
# failed archive (the integrity of the result is checked below).
status=0
tar -czf "$archive.part" "${tar_args[@]}" || status=$?
[ "$status" -le 1 ] || { rm -f "$archive.part"; echo "conch-backup: tar failed (status $status)" >&2; exit 1; }
tar -tzf "$archive.part" >/dev/null || { rm -f "$archive.part"; echo "conch-backup: the archive does not read back" >&2; exit 1; }
mv "$archive.part" "$archive"
(cd "$out_dir" && sha256sum "$(basename "$archive")" > "$(basename "$archive").sha256")
echo "conch-backup: wrote $archive ($(stat -c %s "$archive") bytes)"

# Local retention. Only this instance's archives, and only ones this script made.
find "$out_dir" -maxdepth 1 -type f \( -name "conch-$instance-*.tar.gz" -o -name "conch-$instance-*.tar.gz.sha256" \) \
    -mtime "+$keep_days" -print -delete | sed 's/^/conch-backup: removed old /'

if [ -n "${CONCH_BACKUP_RCLONE_DEST:-}" ]; then
    command -v rclone >/dev/null || { echo "conch-backup: CONCH_BACKUP_RCLONE_DEST is set but rclone is not installed" >&2; exit 1; }
    rclone copy "$archive" "$CONCH_BACKUP_RCLONE_DEST/$instance/"
    rclone copy "$archive.sha256" "$CONCH_BACKUP_RCLONE_DEST/$instance/"
    echo "conch-backup: copied off-site with rclone to $CONCH_BACKUP_RCLONE_DEST/$instance/"
elif [ -n "${CONCH_BACKUP_S3CMD_DEST:-}" ]; then
    command -v s3cmd >/dev/null || { echo "conch-backup: CONCH_BACKUP_S3CMD_DEST is set but s3cmd is not installed" >&2; exit 1; }
    s3cmd put "$archive" "$archive.sha256" "$CONCH_BACKUP_S3CMD_DEST/$instance/"
    echo "conch-backup: copied off-site with s3cmd to $CONCH_BACKUP_S3CMD_DEST/$instance/"
elif [ "$local_only" = 1 ]; then
    echo "conch-backup: local only, as asked (--local-only); no off-site copy was made"
else
    echo "conch-backup: NO OFF-SITE COPY WAS MADE: set CONCH_BACKUP_RCLONE_DEST or CONCH_BACKUP_S3CMD_DEST (docs/deploy.md). The archive is on this host only." >&2
    exit 3
fi
