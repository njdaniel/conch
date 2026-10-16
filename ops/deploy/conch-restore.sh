#!/usr/bin/env bash
# conch-restore.sh <archive> <scratch-dir> — the restore drill.
#
# Unpacks a conch-backup.sh archive into <scratch-dir>/data and proves it is a
# working Conch database: the archive matches its SHA-256, the database passes
# PRAGMA integrity_check, every uploaded file the database names is there with
# the bytes it was stored with, and a conchd started on it (loopback, a spare
# port, no web client) answers /healthz with db ok. Then it stops that conchd
# and prints what it found. Nothing outside <scratch-dir> is touched, and the
# live instance is not involved, so it is safe to run on the droplet.
#
# <scratch-dir> must not exist or must be empty. To restore for real, see
# docs/deploy.md: the drill's unpacked data is the same thing you would put in
# /var/lib/conch/<instance>.
#
# CONCHD is the binary to run (default /opt/conch/current/conchd);
# CONCH_RESTORE_PORT is the first port to try (default 18080).
set -euo pipefail
# Character classes such as [0-9] mean ASCII only in the C locale.
export LC_ALL=C

[ $# -eq 2 ] || { echo "usage: $0 <archive> <scratch-dir>" >&2; exit 2; }
archive=$1
scratch=$2
conchd=${CONCHD:-/opt/conch/current/conchd}

fail() { echo "conch-restore: FAIL: $*" >&2; exit 1; }
for tool in sqlite3 tar sha256sum curl; do
    command -v "$tool" >/dev/null || fail "$tool is required"
done
[ -f "$archive" ] || fail "no archive at $archive"
[ -x "$conchd" ] || fail "no conchd at $conchd (set CONCHD)"
sumfile=$archive.sha256
[ -f "$sumfile" ] || fail "no checksum at $sumfile; a backup without its SHA-256 is not trusted"
if [ -e "$scratch" ] && [ -n "$(ls -A "$scratch")" ]; then
    fail "$scratch is not empty"
fi

umask 077
mkdir -p "$scratch"
data=$scratch/data
mkdir "$data"

# 1. The archive is the one that was written.
want=$(cut -d' ' -f1 "$sumfile")
have=$(sha256sum "$archive" | cut -d' ' -f1)
[ "$want" = "$have" ] || fail "SHA-256 mismatch: the archive is not the one that was backed up"
echo "sha256:        ok ($have)"

# 2. Unpack. Refuse a member that would land outside the data directory.
if tar -tzf "$archive" | grep -Eq '(^/|(^|/)\.\.(/|$))'; then
    fail "the archive holds an absolute or parent path"
fi
# The files become the user running the drill's, with modes from its umask,
# whatever the archive says (and whoever runs it, root included).
tar --no-same-owner --no-same-permissions -xzf "$archive" -C "$data"
[ -f "$data/conch.db" ] || fail "the archive has no conch.db"

# 3. The database is intact.
check=$(sqlite3 "$data/conch.db" 'PRAGMA integrity_check')
[ "$check" = ok ] || fail "integrity_check: $check"
echo "integrity:     ok"
user_version=$(sqlite3 "$data/conch.db" 'PRAGMA user_version')
messages=$(sqlite3 "$data/conch.db" 'SELECT count(*) FROM messages')
echo "user_version:  $user_version"
echo "messages:      $messages"

# 4. Every file the database names is on disk with the bytes it was stored with.
files=0
while IFS= read -r sum; do
    [ -n "$sum" ] || continue
    blob=$data/files/${sum:0:2}/$sum
    [ -f "$blob" ] || fail "file $sum is in the database but not in the archive"
    [ "$(sha256sum "$blob" | cut -d' ' -f1)" = "$sum" ] || fail "file $sum does not match its SHA-256"
    files=$((files + 1))
done < <(sqlite3 "$data/conch.db" 'SELECT DISTINCT sha256 FROM files')
echo "files:         $files, each with its stored SHA-256"

# 5. conchd opens it. A spare loopback port, an empty environment (no LiveKit,
# no ntfy), and no web client.
port=${CONCH_RESTORE_PORT:-18080}
while (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; do
    port=$((port + 1))
    [ "$port" -lt 65000 ] || fail "no spare port"
done
log=$scratch/conchd.log
env -i PATH="$PATH" "$conchd" serve --data "$data" --listen "127.0.0.1:$port" --no-web >"$log" 2>&1 &
pid=$!
stop() {
    if kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    fi
}
trap stop EXIT

health=""
for _ in $(seq 1 100); do
    kill -0 "$pid" 2>/dev/null || { tail -n 5 "$log" >&2; fail "conchd exited on the restored data"; }
    if health=$(curl -fsS "http://127.0.0.1:$port/healthz" 2>/dev/null); then break; fi
    health=""
    sleep 0.2
done
[ -n "$health" ] || { tail -n 5 "$log" >&2; fail "conchd did not answer /healthz"; }
case "$health" in
    *'"status":"ok"'*'"db":"ok"'*) ;;
    *) fail "/healthz said $health" ;;
esac
echo "healthz:       $health"
stop
trap - EXIT

echo "conch-restore: OK: $archive restores to a working database in $data"
