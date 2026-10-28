#!/usr/bin/env bash
# deploy-scripts-test.sh — the test of the files in ops/deploy (issue #435).
#
# It needs no droplet, no domain and no account. It builds conchd, runs it on
# loopback against a temporary data directory with a message and an uploaded
# file in it, and then runs the real scripts:
#
#   conch-backup.sh   an archive with a SHA-256; exit 3 and a plain message
#                     without an off-site destination; the off-site path with a
#                     stand-in rclone; --local-only and temporary mode (only
#                     an exact env-file line, no secrets exposed); a backup.env
#                     that is not 0600 is refused; retention by age for nightly archives
#                     and by count for pre-upgrade ones
#   conch-restore.sh  the drill: the message and the file are back and /healthz
#                     answers ok; the backup's operator token is refused by
#                     the restored data (conchd restore, issue #716); a second
#                     start does nothing more; a tampered archive is refused;
#                     a conchd that cannot run conchd restore fails the drill
#                     and leaves data that serve refuses
#   conch-upgrade.sh  upgrade, rollback without a migration (data kept) and
#                     rollback with one (pre-upgrade backup restored, the kept
#                     directory named, the backup's token refused; a previous
#                     release without conchd restore is refused before
#                     anything is stopped; one whose conchd restore fails
#                     leaves the instance stopped, and the rollback run again
#                     finishes it), with a stand-in systemctl that runs the
#                     instance as a child; a missing, corrupt or symlinked
#                     pre-upgrade backup stops the upgrade, and the rollback,
#                     before any instance is stopped; a crafted archive path
#                     from the backup step, and a rollback record with an
#                     injected line, are refused and nothing in them runs
#
# It runs as one user, so the split between root, conch-<instance> and
# conch-backup (docs/deploy.md) is not exercised here: run as a user other
# than root, the scripts do every step as that user.
#
# It also runs shellcheck on every script and systemd-analyze verify on the
# units. sqlite3 is a host prerequisite of the scripts and of this test; it
# fails, naming it, rather than skipping. shellcheck and systemd-analyze are
# skipped with a line when absent, and are a failure when CI is set.
# The ls calls name files this test made; "A && B || fail" is the test idiom.
# shellcheck disable=SC2012,SC2015
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
deploy=$PWD/ops/deploy

fail() { echo "deploy-scripts-test: FAIL: $*" >&2; exit 1; }
ok() { echo "deploy-scripts-test: ok: $*"; }
skip() {
    if [ -n "${CI:-}" ]; then fail "$1 is not installed (CI is set, so this is not a skip)"; fi
    echo "deploy-scripts-test: skipped: $1 is not installed"
}

for tool in sqlite3 curl tar sha256sum; do
    command -v "$tool" >/dev/null || fail "$tool is required (apt install sqlite3 curl)"
done

work=$(mktemp -d)
pids=()
cleanup() {
    local p
    for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
    for p in "$work"/pids/*; do
        [ -f "$p" ] || continue
        kill "$(cat "$p")" 2>/dev/null || true
    done
    rm -rf "${work:?}"
}
trap cleanup EXIT

# --- static checks ---------------------------------------------------------

if command -v shellcheck >/dev/null; then
    shellcheck "$deploy"/*.sh scripts/deploy-scripts-test.sh scripts/dev-stack.sh || fail "shellcheck"
    ok "shellcheck"
else
    skip shellcheck
fi

if command -v systemd-analyze >/dev/null; then
    units=$work/units
    mkdir "$units"
    for f in 'conchd@.service' 'conch-backup@.service' 'conch-backup@.timer'; do
        cp "$deploy/$f" "$units/${f/@/@alpha}"
    done
    cp "$deploy"/livekit.service "$deploy"/livekit-cert.service "$deploy"/livekit-cert.path "$units/"
    # verify loads the units ours name (livekit.service Requires=docker.service,
    # and orders after caddy.service). Loaded from this host, they bring the
    # host's own drop-ins with them: a workstation whose docker.service.d adds
    # RequiresMountsFor=/home fails here with "Unit home.mount not found",
    # which says nothing about Conch's units (#870). So verify reads only this
    # directory, which holds stand-ins for docker.service and caddy.service,
    # then the distribution's own units (for sysinit.target and the like),
    # never /etc/systemd/system, where a host's drop-ins live. (A drop-in the
    # distribution itself shipped for docker.service would still apply: stock
    # packages ship none.)
    printf '[Unit]\nDescription=stand-in for verify\n[Service]\nType=oneshot\nExecStart=/bin/true\n' >"$units/docker.service"
    cp "$units/docker.service" "$units/caddy.service"
    vendor=/usr/lib/systemd/system
    [ -d "$vendor" ] || vendor=/lib/systemd/system
    verify() {
        # The units name binaries that exist only on the droplet; that is the
        # one complaint allowed.
        SYSTEMD_UNIT_PATH="$1:$vendor" systemd-analyze verify "$1"/conch* "$1"/livekit* 2>&1 | grep -v 'is not executable' || true
    }
    out=$(verify "$units")
    [ -z "$out" ] || fail "systemd-analyze verify:
$out"
    # And it still sees a real error in one of ours: a misspelt key.
    cp -r "$units" "$work/units-broken"
    sed -i 's/^ExecStart=/ExecStrat=/' "$work/units-broken/conchd@alpha.service"
    out=$(verify "$work/units-broken")
    printf '%s\n' "$out" | grep -q 'conchd@alpha.service' || fail "systemd-analyze verify did not see a misspelt key in conchd@alpha.service"
    ok "systemd-analyze verify (and it sees a misspelt key)"
else
    skip systemd-analyze
fi

# No secret-looking value and no real host in the committed files.
if grep -rEn 'CONCHD_LIVEKIT_API_SECRET=[^r]|LIVEKIT_KEYS=' "$deploy"; then
    fail "a secret-looking value is in ops/deploy"
fi
ok "no secret in ops/deploy"

# --- helpers ---------------------------------------------------------------

# Two copies of this test can run at once (two worktrees, two workers), so no
# port is picked by looking for a free one and binding it later: another run can
# take it in between, and the curl then reaches that run's conchd, which does not
# know our token. conchd binds port 0 and prints the address it holds
# ("listening on 127.0.0.1:PORT"). That line is written by the process we
# started, after it bound, so the port read from it is ours.

# wait_listening <pid> <log>: print the port the conchd <pid> bound, read from
# its log. Return 1 if it exits first (a bind failure, a bad flag) or says
# nothing in 10 seconds.
wait_listening() {
    local pid=$1 log=$2 addr
    for _ in $(seq 1 100); do
        addr=$(sed -n 's/.* listening on \([^ ]*\) (data .*/\1/p' "$log" 2>/dev/null | head -n 1)
        if [ -n "$addr" ]; then echo "${addr##*:}"; return 0; fi
        kill -0 "$pid" 2>/dev/null || return 1
        sleep 0.1
    done
    return 1
}

# listen_or_fail <pid> <log>: wait_listening, or fail saying the bind failed and
# where the log is.
listen_or_fail() {
    wait_listening "$1" "$2" || { cat "$2" >&2; fail "conchd did not bind a port; see $2"; }
}

wait_health() { # <port>
    for _ in $(seq 1 100); do
        if curl -fsS "http://127.0.0.1:$1/healthz" >/dev/null 2>&1; then return 0; fi
        sleep 0.2
    done
    return 1
}

arch=amd64
case "$(uname -m)" in aarch64 | arm64) arch=arm64 ;; esac

build() { # <version> <out>
    go build -ldflags "-X main.version=$1" -o "$2" ./cmd/conchd
}
mkdir -p "$work/bin"
build v0.0.1 "$work/bin/conchd-v0.0.1"
build v0.0.2 "$work/bin/conchd-v0.0.2"
ok "built conchd v0.0.1 and v0.0.2"

api() { # <port> <token> <method> <path> [curl args...]
    local port=$1 token=$2 method=$3 path=$4
    shift 4
    curl -fsS -X "$method" -H "Authorization: Bearer $token" "$@" "http://127.0.0.1:$port$path"
}

# --- an instance with a message and a file ----------------------------------

data_root=$work/var
backups=$work/backups
mkdir -p "$data_root/alpha" "$backups"
umask 077
token=$("$work/bin/conchd-v0.0.1" bootstrap-operator --data "$data_root/alpha" --name tester)
[ -n "$token" ] || fail "no operator token"
"$work/bin/conchd-v0.0.1" serve --data "$data_root/alpha" --listen 127.0.0.1:0 --no-web >"$work/serve.log" 2>&1 &
pids+=($!)
port=$(listen_or_fail "${pids[0]}" "$work/serve.log")
wait_health "$port" || { cat "$work/serve.log" >&2; fail "conchd did not start"; }

api "$port" "$token" POST /v0/channels -H 'Content-Type: application/json' -d '{"name":"ops"}' >/dev/null
api "$port" "$token" POST /v2/channels/ops/messages -H 'Content-Type: application/json' \
    -d '{"body":"hello before the backup"}' >/dev/null
head -c 3000 /dev/urandom >"$work/upload.bin"
file_id=$(api "$port" "$token" POST '/v2/channels/ops/files?filename=upload.bin' \
    -H 'Content-Type: application/octet-stream' --data-binary @"$work/upload.bin" |
    sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
[ -n "$file_id" ] || fail "the upload returned no id"
ok "instance running with a message and file $file_id"

# --- backup -----------------------------------------------------------------

export CONCH_DATA_ROOT=$data_root CONCH_BACKUP_DIR=$backups CONCH_BACKUP_ENV=$work/absent.env
export CONCH_ETC=$work/instance-env
mkdir "$CONCH_ETC"
printf 'CONCHD_MODE=normal\n' >"$CONCH_ETC/alpha.env"

status=0
out=$("$deploy/conch-backup.sh" alpha 2>&1) || status=$?
[ "$status" -eq 3 ] || fail "backup without an off-site destination exited $status, want 3: $out"
case "$out" in *"NO OFF-SITE COPY WAS MADE"*) ;; *) fail "backup did not say there is no off-site copy: $out" ;; esac
archive=$(ls -1 "$backups"/alpha/conch-alpha-*.tar.gz | head -n 1)
[ -f "$archive" ] && [ -f "$archive.sha256" ] || fail "no archive and checksum after the first backup"
(cd "$backups/alpha" && sha256sum -c "$(basename "$archive").sha256" >/dev/null) || fail "the checksum file does not verify"
ok "normal instance, no off-site destination: said so, exit 3, archive still written"
# Every archive carries an empty restore-pending (issue #716), so whatever
# unpacks it gets data conchd serve refuses until conchd restore has run.
tar -tzf "$archive" | grep -qx restore-pending || fail "the archive holds no restore-pending"
[ ! -e "$data_root/alpha/restore-pending" ] || fail "the backup left restore-pending in the live data directory"
ok "the archive carries restore-pending; the live data directory does not"

sleep 1 # the archive name carries the second
"$deploy/conch-backup.sh" alpha --local-only >"$work/b2.out" 2>&1 || { cat "$work/b2.out" >&2; fail "--local-only exited non-zero"; }
grep -q 'no off-site copy was made' "$work/b2.out" || fail "--local-only did not say what it did"
ok "--local-only exits 0"

# Only an exact mode assignment enables temporary backups. Each row gets a
# fresh archive directory, so an earlier backup cannot hide a missing one.
# The secret canaries and shell expression must never be printed or sourced.
for mode_line in 'CONCHD_MODE=temporary' missing unreadable \
    'CONCHD_MODE=normal' 'CONCHD_MODE=Temporary' 'CONCHD_MODE=temporary # comment' \
    'CONCHD_MODE="temporary"' 'CONCHD_MODE=temporary ' $'CONCHD_MODE=temporary\r' \
    '# CONCHD_MODE=temporary' 'OTHER_CONCHD_MODE=temporary' 'CONCHD_OPERATOR_TOTP=optional'; do
    printf '%s\n' "$mode_line" \
        'CONCHD_LIVEKIT_API_SECRET=instance-env-secret-livekit' \
        'CONCHD_TOTP_KEY=instance-env-secret-totp' \
        "UNUSED=\$(touch '$work/env-sourced')" >"$CONCH_ETC/alpha.env"
    want=3
    case "$mode_line" in
        CONCHD_MODE=temporary) want=0 ;;
        missing) rm "$CONCH_ETC/alpha.env" ;;
        unreadable)
            printf 'CONCHD_MODE=temporary\n' >>"$CONCH_ETC/alpha.env"
            chmod 000 "$CONCH_ETC/alpha.env" ;;
    esac
    status=0
    out=$(CONCH_BACKUP_DIR=$work/mode-backups "$deploy/conch-backup.sh" alpha 2>&1) || status=$?
    chmod 0600 "$CONCH_ETC/alpha.env" 2>/dev/null || true
    case "$out" in *instance-env-secret-*) fail "instance env secret appeared in backup output" ;; esac
    [ ! -e "$work/env-sourced" ] || fail "the instance env file was sourced"
    [ "$status" -eq "$want" ] || fail "$mode_line backup exited $status, want $want: $out"
    mode_archive=$(ls -1 "$work/mode-backups/alpha"/conch-alpha-*.tar.gz | head -n 1)
    [ -f "$mode_archive" ] && [ -f "$mode_archive.sha256" ] || fail "$mode_line backup wrote no archive and checksum"
    if [ "$want" -eq 0 ]; then
        [ "$(printf '%s\n' "$out" | grep -Fc 'temporary instance: local-only backup, by ADR-011 item 2.2')" -eq 1 ] ||
            fail "temporary backup did not explain its mode exactly once: $out"
    else
        case "$out" in *'NO OFF-SITE COPY WAS MADE'*) ;; *) fail "$mode_line backup lost the off-site warning: $out" ;; esac
        case "$out" in *'temporary instance:'*) fail "$mode_line was treated as temporary: $out" ;; esac
    fi
    ok "$mode_line: archive and checksum written, exit $want, no env secrets printed or sourced"
    rm -rf "$work/mode-backups"
done
rm "$CONCH_ETC/alpha.env"

printf 'CONCH_BACKUP_KEEP_DAYS=14\n' >"$work/loose.env"
chmod 0644 "$work/loose.env"
status=0
out=$(CONCH_BACKUP_ENV=$work/loose.env "$deploy/conch-backup.sh" alpha --local-only 2>&1) || status=$?
[ "$status" -eq 2 ] || fail "a backup.env of mode 0644 was accepted (exit $status): $out"
case "$out" in *"make it 0600"*) ;; *) fail "loose backup.env: wrong reason: $out" ;; esac
ok "a backup.env that is not 0600 is refused"

mkdir "$work/fakebin" "$work/offsite"
cat >"$work/fakebin/rclone" <<EOF
#!/bin/sh
# stand-in: rclone copy <src> <dest>; dest is a directory path here
[ "\$1" = copy ] || exit 2
mkdir -p "\$3" && cp "\$2" "\$3"
EOF
chmod +x "$work/fakebin/rclone"
sleep 1
PATH=$work/fakebin:$PATH CONCH_BACKUP_RCLONE_DEST=$work/offsite "$deploy/conch-backup.sh" alpha >"$work/b3.out" 2>&1 ||
    { cat "$work/b3.out" >&2; fail "backup with an off-site destination exited non-zero"; }
[ "$(ls "$work/offsite/alpha" | wc -l)" -eq 2 ] || fail "off-site copy did not receive the archive and its checksum"
ok "off-site destination: archive and checksum copied, exit 0"

# Local-only must skip even configured destinations, including the shared
# backup.env used by normal instances. Neither off-site tool may be called.
cat >"$work/fakebin/s3cmd" <<EOF
#!/bin/sh
touch "$work/s3cmd-called"
exit 1
EOF
chmod +x "$work/fakebin/s3cmd"
printf 'CONCH_BACKUP_RCLONE_DEST=%s\nCONCH_BACKUP_S3CMD_DEST=%s\n' \
    "$work/local-only-offsite" s3://unused >"$work/local-only.env"
for backup_kind in explicit temporary; do
    backup_args=(alpha)
    if [ "$backup_kind" = temporary ]; then
        printf 'CONCHD_MODE=temporary\n' >"$CONCH_ETC/alpha.env"
    else
        backup_args+=(--local-only)
    fi
    status=0
    out=$(PATH=$work/fakebin:$PATH CONCH_BACKUP_ENV=$work/local-only.env \
        "$deploy/conch-backup.sh" "${backup_args[@]}" 2>&1) || status=$?
    [ "$status" -eq 0 ] || fail "$backup_kind local-only with configured destinations exited $status: $out"
    [ ! -e "$work/local-only-offsite" ] && [ ! -e "$work/s3cmd-called" ] ||
        fail "$backup_kind local-only backup called an off-site tool"
    ok "$backup_kind local-only skips configured off-site destinations, exit 0"
done
rm "$CONCH_ETC/alpha.env"

# Retention removes only old archives of this instance.
old=$backups/alpha/conch-alpha-19990101T000000Z.tar.gz
other=$backups/alpha/conch-other-19990101T000000Z.tar.gz
touch -d '2000-01-01' "$old" "$other"
sleep 1
"$deploy/conch-backup.sh" alpha --local-only >/dev/null 2>&1
[ ! -e "$old" ] || fail "an archive older than the retention was kept"
[ -e "$other" ] || fail "retention removed another instance's archive"
ok "retention"

# --- restore drill -----------------------------------------------------------

latest=$(ls -1t "$backups"/alpha/conch-alpha-2*.tar.gz | head -n 1)
CONCHD=$work/bin/conchd-v0.0.1 "$deploy/conch-restore.sh" "$latest" "$work/scratch" >"$work/restore.out" 2>&1 ||
    { cat "$work/restore.out" >&2; fail "the restore drill failed"; }
grep -q 'conch-restore: OK' "$work/restore.out" || fail "the drill did not report OK"
grep -q '"db":"ok"' "$work/restore.out" || fail "the drill did not report /healthz db ok"
grep -q '^restore: signed out [1-9][0-9]* principal' "$work/restore.out" || fail "the drill did not run conchd restore: $(cat "$work/restore.out")"
if grep -qF "$token" "$work/restore.out" "$work/scratch/conchd.log"; then fail "the drill printed the operator's token"; fi
[ ! -e "$work/scratch/data/restore-pending" ] || fail "the drill left restore-pending behind"
ok "restore drill, conchd restore included"
# The drill restores a copy: the live instance and its sign-ins are as they were.
api "$port" "$token" GET /v2/channels/ops/messages | grep -q 'hello before the backup' || fail "the drill touched the live instance's sign-ins"
ok "the drill leaves the live instance alone"

# Served again, the restored data refuses the token the backup holds, and a
# second start finds no restore to finish.
"$work/bin/conchd-v0.0.1" serve --data "$work/scratch/data" --listen 127.0.0.1:0 --no-web >"$work/restored.log" 2>&1 &
pids+=($!)
rport=$(listen_or_fail "${pids[1]}" "$work/restored.log")
wait_health "$rport" || fail "the restored data did not start"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "http://127.0.0.1:$rport/v2/channels/ops/messages")
[ "$code" = 401 ] || fail "the backup's operator token = $code on the restored data, want 401"
if grep -q 'first start on restored data' "$work/restored.log"; then fail "a second start on restored data acted as the first"; fi
ok "restored data refuses the backup's token; a second start does nothing more"
kill "${pids[1]}" 2>/dev/null || true
wait "${pids[1]}" 2>/dev/null || true

# It holds the message and the file. Nothing in the backup signs anyone in
# any more, so the message is read from a conchd with authentication off,
# and the file, which no route serves without authentication, is compared
# where the database says it is stored.
"$work/bin/conchd-v0.0.1" serve --data "$work/scratch/data" --listen 127.0.0.1:0 --no-web --auth off >"$work/restored-open.log" 2>&1 &
pids+=($!)
rport=$(listen_or_fail "${pids[2]}" "$work/restored-open.log")
wait_health "$rport" || fail "the restored data did not start with authentication off"
curl -fsS "http://127.0.0.1:$rport/v2/channels/ops/messages" | grep -q 'hello before the backup' || fail "the message is not in the restored server"
sum=$(sqlite3 "$work/scratch/data/conch.db" "SELECT sha256 FROM files WHERE id = $file_id")
[ -n "$sum" ] || fail "file $file_id is not in the restored database"
cmp "$work/upload.bin" "$work/scratch/data/files/${sum:0:2}/$sum" || fail "the restored file differs from the upload"
ok "restored server holds the message and the file"
kill "${pids[2]}" 2>/dev/null || true
wait "${pids[2]}" 2>/dev/null || true

# serve refuses data that holds restore-pending: a restore that conchd
# restore has not finished.
: >"$work/scratch/data/restore-pending"
if timeout 20 "$work/bin/conchd-v0.0.1" serve --data "$work/scratch/data" --listen 127.0.0.1:0 --no-web >"$work/pending.log" 2>&1; then
    fail "serve started on data with restore-pending"
fi
grep -q 'run: conchd restore --data' "$work/pending.log" || fail "serve on restore-pending: wrong reason: $(cat "$work/pending.log")"
rm -f "$work/scratch/data/restore-pending"
ok "serve refuses data whose restore is unfinished"

# A conchd that cannot run conchd restore (one from before it, say) fails the
# drill before anything serves the data, and the data stays unservable.
norestore() { # <real conchd> <out>: a stand-in for a conchd from before conchd restore
    # shellcheck disable=SC2016 # expanded by the stand-in, not here
    printf '#!/bin/sh\nif [ "$1" = restore ]; then echo %s >&2; exit 1; fi\nexec %s "$@"\n' \
        "'conchd: unknown command \"restore\"'" "'$1'" >"$2"
    chmod 0755 "$2"
}
norestore "$work/bin/conchd-v0.0.1" "$work/bin/conchd-norestore"
if CONCHD=$work/bin/conchd-norestore "$deploy/conch-restore.sh" "$latest" "$work/scratch3" >"$work/norestore.out" 2>&1; then
    fail "the drill passed with a conchd that cannot run conchd restore"
fi
grep -q 'conchd restore failed' "$work/norestore.out" || fail "drill without conchd restore: wrong reason: $(cat "$work/norestore.out")"
[ -e "$work/scratch3/data/restore-pending" ] && [ ! -e "$work/scratch3/conchd.log" ] ||
    fail "a drill whose conchd restore failed served the data or removed restore-pending"
if timeout 20 "$work/bin/conchd-v0.0.1" serve --data "$work/scratch3/data" --listen 127.0.0.1:0 --no-web >"$work/pending3.log" 2>&1; then
    fail "serve started on the data of a failed drill"
fi
ok "a drill whose conchd restore fails serves nothing and leaves data that serve refuses"

# A tampered archive, a missing checksum and a used scratch directory are refused.
cp "$latest" "$work/tampered.tar.gz"
cp "$latest.sha256" "$work/tampered.tar.gz.sha256"
printf 'x' >>"$work/tampered.tar.gz"
if CONCHD=$work/bin/conchd-v0.0.1 "$deploy/conch-restore.sh" "$work/tampered.tar.gz" "$work/scratch2" >"$work/t.out" 2>&1; then
    fail "a tampered archive was restored"
fi
grep -q 'SHA-256 mismatch' "$work/t.out" || fail "tampered archive: wrong reason: $(cat "$work/t.out")"
if CONCHD=$work/bin/conchd-v0.0.1 "$deploy/conch-restore.sh" "$latest" "$work/scratch" >"$work/u.out" 2>&1; then
    fail "a non-empty scratch directory was accepted"
fi
ok "tampered archive and used scratch directory refused"

kill "${pids[@]}" 2>/dev/null || true
wait "${pids[@]}" 2>/dev/null || true
pids=()

# --- upgrade and rollback -----------------------------------------------------

opt=$work/opt etc=$work/etc rel=$work/release up_data=$work/updata
mkdir -p "$opt/releases/v0.0.1" "$etc" "$rel/v0.0.2" "$work/pids" "$up_data/beta"
install -m 0755 "$work/bin/conchd-v0.0.1" "$opt/releases/v0.0.1/conchd"
ln -s "$opt/releases/v0.0.1" "$opt/current"
cp "$work/bin/conchd-v0.0.2" "$rel/v0.0.2/conchd_v0.0.2_linux_$arch"
(cd "$rel/v0.0.2" && sha256sum "./conchd_v0.0.2_linux_$arch" >checksums.txt)

# A stand-in for systemctl that runs the instance as a child of this test.
# Every call is logged, so a test can say nothing was stopped.
cat >"$work/systemctl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
echo "\$*" >>"$work/systemctl.calls"
unit=\${2#conchd@}
pidfile=$work/pids/\$unit
case "\$1" in
stop)
    if [ -f "\$pidfile" ]; then
        pid=\$(cat "\$pidfile")
        kill "\$pid" 2>/dev/null || true
        for _ in \$(seq 1 100); do kill -0 "\$pid" 2>/dev/null || break; sleep 0.1; done
        rm -f "\$pidfile"
    fi ;;
start)
    set -a; . "$etc/\$unit.env"; set +a
    "$opt/current/conchd" serve --no-web >>"$work/\$unit.log" 2>&1 &
    echo \$! >"\$pidfile" ;;
*) exit 2 ;;
esac
EOF
chmod +x "$work/systemctl"

btoken=$("$opt/current/conchd" bootstrap-operator --data "$up_data/beta" --name tester)
# The instance's port is in its env file, because the upgrade restarts it on the
# same address, so it cannot be port 0. Try a port from a range each run picks
# at random. A conchd that fails to bind exits without printing its address and
# the next port is tried; the port is ours once our own conchd printed it.
bport=""
for _ in 1 2 3 4 5 6 7 8; do
    cand=$((20000 + RANDOM % 20000))
    printf 'CONCHD_DATA=%s\nCONCHD_LISTEN=127.0.0.1:%s\n' "$up_data/beta" "$cand" >"$etc/beta.env"
    : >"$work/beta.log"
    "$work/systemctl" start conchd@beta
    if wait_listening "$(cat "$work/pids/beta")" "$work/beta.log" >/dev/null; then bport=$cand; break; fi
    "$work/systemctl" stop conchd@beta
done
[ -n "$bport" ] || { cat "$work/beta.log" >&2; fail "beta did not bind a port in 8 tries; see $work/beta.log"; }
wait_health "$bport" || fail "beta did not start; see $work/beta.log"
api "$bport" "$btoken" POST /v0/channels -H 'Content-Type: application/json' -d '{"name":"ops"}' >/dev/null
post() { api "$bport" "$btoken" POST /v2/channels/ops/messages -H 'Content-Type: application/json' -d "{\"body\":\"$1\"}" >/dev/null; }
has() { api "$bport" "$btoken" GET /v2/channels/ops/messages | grep -q "$1"; }
version() { curl -fsS "http://127.0.0.1:$bport/healthz" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p'; }
post "before upgrade"

up() { # args to conch-upgrade.sh
    SYSTEMCTL=$work/systemctl CONCH_OPT=$opt CONCH_ETC=$etc CONCH_DATA_ROOT=$up_data CONCH_BACKUP_DIR=$work/upbackups \
        CONCH_RELEASE_URL=file://$rel "$deploy/conch-upgrade.sh" "$@"
}

# A release that does not match checksums.txt is refused before anything changes.
mkdir -p "$rel/v0.0.9"
printf 'not a binary' >"$rel/v0.0.9/conchd_v0.0.9_linux_$arch"
(cd "$rel/v0.0.9" && printf '%s  ./conchd_v0.0.9_linux_%s\n' "$(printf 'something else' | sha256sum | cut -d' ' -f1)" "$arch" >checksums.txt)
if up v0.0.9 beta >"$work/bad.out" 2>&1; then fail "a release that fails its checksum was installed"; fi
grep -q 'does not match checksums.txt' "$work/bad.out" || fail "bad checksum: wrong reason: $(cat "$work/bad.out")"
[ "$(readlink "$opt/current")" = "$opt/releases/v0.0.1" ] && [ "$(version)" = v0.0.1 ] || fail "a refused upgrade changed the install"
ok "upgrade: checksum mismatch refused, nothing changed"

# A pre-upgrade backup that is missing, or does not match its .sha256, stops
# the upgrade before any instance is stopped, and the error says what to do.
cat >"$work/nobackup.sh" <<'STANDIN'
#!/bin/sh
# stand-in for conch-backup.sh that says it backed up and wrote nothing
echo "conch-backup: pre-upgrade backup, kept on this host only"
STANDIN
badsum_archive=$work/upbackups/beta/pre-upgrade-v0.0.2-beta-20000101T000000Z.tar.gz
cat >"$work/badsum.sh" <<STANDIN
#!/bin/sh
# stand-in for conch-backup.sh whose archive does not match its .sha256
mkdir -p "$work/upbackups/beta"
printf 'not an archive' >"$badsum_archive"
echo "0000000000000000000000000000000000000000000000000000000000000000  \$(basename "$badsum_archive")" >"$badsum_archive.sha256"
echo "conch-backup: wrote $badsum_archive (14 bytes)"
STANDIN
chmod +x "$work/nobackup.sh" "$work/badsum.sh"
bpid=$(cat "$work/pids/beta")
for stand_in in nobackup badsum; do
    calls=$(wc -l <"$work/systemctl.calls")
    if CONCH_BACKUP_SCRIPT=$work/$stand_in.sh up v0.0.2 beta >"$work/$stand_in.out" 2>&1; then
        fail "$stand_in: an upgrade without a good pre-upgrade backup went ahead"
    fi
    case $stand_in in
        nobackup) reason='named no archive, which is not' ;;
        badsum) reason='does not match its .sha256' ;;
    esac
    grep -q "$reason" "$work/$stand_in.out" || fail "$stand_in: wrong reason: $(cat "$work/$stand_in.out")"
    grep -q 'Nothing was stopped' "$work/$stand_in.out" && grep -q 'run this upgrade again' "$work/$stand_in.out" ||
        fail "$stand_in: the error does not say what to do: $(cat "$work/$stand_in.out")"
    [ "$(wc -l <"$work/systemctl.calls")" = "$calls" ] || fail "$stand_in: systemctl was called: $(tail -n 2 "$work/systemctl.calls")"
    [ "$(cat "$work/pids/beta")" = "$bpid" ] && kill -0 "$bpid" || fail "$stand_in: the instance was stopped"
    [ "$(readlink "$opt/current")" = "$opt/releases/v0.0.1" ] && [ "$(version)" = v0.0.1 ] || fail "$stand_in: the install changed"
    [ ! -e "$opt/state/beta" ] || fail "$stand_in: a rollback state was written"
done
rm -f "$badsum_archive" "$badsum_archive.sha256"
ok "upgrade: a missing or corrupt pre-upgrade backup stops it before anything is stopped"

# The archive path comes from the conch-backup user, who can write anything
# there and a matching .sha256 beside it. A path that is not exactly this
# instance's pre-upgrade archive for this tag, in the backup directory, stops
# the upgrade before anything is stopped, and nothing in it ever runs: the
# rollback record was once sourced by root, so a name holding $(...) or ';'
# ran as root. So does a symlink in place of the archive. PWN is a file the
# payloads would create.
export PWN=$work/pwned
cat >"$work/crafted.sh" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for conch-backup.sh: writes CRAFTED (a symlink to a real file when
# CRAFTED_TARGET is set) with a matching .sha256, and says it wrote it.
set -eu
dir=$(dirname "$CRAFTED") name=$(basename "$CRAFTED")
mkdir -p "$dir"
if [ -n "${CRAFTED_TARGET:-}" ]; then
    mkdir -p "$(dirname "$CRAFTED_TARGET")"
    printf 'an archive' >"$CRAFTED_TARGET"
    ln -sfn "$CRAFTED_TARGET" "$CRAFTED"
else
    printf 'an archive' >"$CRAFTED"
fi
(cd "$dir" && printf '%s  %s\n' "$(sha256sum <"$name" | cut -d' ' -f1)" "$name" >"$name.sha256")
# Escape sequences (clear the screen, set the title), which must not reach
# root's terminal.
printf 'conch-backup: \033[2J\033]0;title\007 on the terminal\n'
echo "conch-backup: wrote $CRAFTED (10 bytes)"
STANDIN
chmod +x "$work/crafted.sh"
good_name="pre-upgrade-v0.0.2-beta-20260101T000000Z.tar.gz"
crafted_cases=(
    "subst|$work/upbackups/beta/pre-upgrade-v0.0.2-beta-x\$(touch \"\$PWN\").tar.gz|"
    "semicolon|$work/upbackups/beta/$good_name;touch \"\$PWN\"|"
    "outside|$work/elsewhere/$good_name|"
    "symlink|$work/upbackups/beta/$good_name|$work/elsewhere/real.tar.gz"
)
for c in "${crafted_cases[@]}"; do
    IFS='|' read -r name path target <<<"$c"
    calls=$(wc -l <"$work/systemctl.calls")
    if CRAFTED=$path CRAFTED_TARGET=$target CONCH_BACKUP_SCRIPT=$work/crafted.sh up v0.0.2 beta >"$work/crafted-$name.out" 2>&1; then
        fail "crafted $name: the upgrade accepted $path"
    fi
    case $name in
        symlink) reason='is a symlink' ;;
        *) reason='which is not' ;;
    esac
    grep -q "$reason" "$work/crafted-$name.out" || fail "crafted $name: wrong reason: $(cat "$work/crafted-$name.out")"
    [ ! -e "$PWN" ] || fail "crafted $name: the payload in the archive name ran"
    grep -q 'on the terminal' "$work/crafted-$name.out" || fail "crafted $name: the backup step's output was not shown"
    if LC_ALL=C grep -q $'[\x01-\x08\x0b-\x1f\x7f]' "$work/crafted-$name.out"; then
        fail "crafted $name: a control character from the backup step reached the terminal"
    fi
    [ "$(wc -l <"$work/systemctl.calls")" = "$calls" ] || fail "crafted $name: systemctl was called"
    [ "$(cat "$work/pids/beta")" = "$bpid" ] && kill -0 "$bpid" || fail "crafted $name: the instance was stopped"
    [ ! -e "$opt/state/beta" ] || fail "crafted $name: a rollback state was written"
    rm -f "$path" "$path.sha256"
done
rm -rf "${work:?}/elsewhere"
for f in "$work"/upbackups/beta/* "$work"/upbackups/beta/.[!.]*; do
    [ ! -e "$f" ] && [ ! -L "$f" ] || fail "the crafted cases left $f behind"
done
ok "upgrade: a crafted archive path (\$(...), ';', outside the backup directory) or a symlinked archive is refused, nothing in it runs, and the step's escape sequences are stripped"

# Pre-upgrade backups are kept by count (3), whatever their age, and the
# nightly retention by age does not touch them.
mkdir -p "$work/upbackups/beta"
for d in 1 2 3; do
    f=$work/upbackups/beta/pre-upgrade-v0.0.0-beta-1999010${d}T000000Z.tar.gz
    touch -d "2000-01-0$d" "$f" "$f.sha256"
done
up v0.0.2 beta >"$work/up1.out" 2>&1 || { cat "$work/up1.out" >&2; fail "upgrade failed"; }
[ "$(version)" = v0.0.2 ] || fail "after the upgrade /healthz says $(version)"
has 'before upgrade' || fail "the upgrade lost a message"
ok "upgrade: /healthz reports v0.0.2, data intact"

pre=$(ls -1 "$work"/upbackups/beta/pre-upgrade-v0.0.2-beta-*.tar.gz)
[ -f "$pre" ] && [ -f "$pre.sha256" ] || fail "no pre-upgrade-v0.0.2-beta-* archive"
grep -qx "backup_before=$pre" "$opt/state/beta" || fail "the rollback state does not name $pre"
[ ! -e "$work/upbackups/beta/pre-upgrade-v0.0.0-beta-19990101T000000Z.tar.gz" ] || fail "the oldest of four pre-upgrade backups was kept"
[ -e "$work/upbackups/beta/pre-upgrade-v0.0.0-beta-19990102T000000Z.tar.gz" ] || fail "a pre-upgrade backup within the newest three was removed"
CONCH_DATA_ROOT=$up_data CONCH_BACKUP_DIR=$work/upbackups "$deploy/conch-backup.sh" beta --local-only >/dev/null 2>&1 ||
    fail "a nightly backup of beta failed"
[ -e "$work/upbackups/beta/pre-upgrade-v0.0.0-beta-19990102T000000Z.tar.gz" ] || fail "the nightly retention removed a pre-upgrade backup"
ok "pre-upgrade backup: its own name, kept by count, untouched by the nightly retention"

# The rollback record is parsed, never sourced: a line it does not write, a
# value that is not what the upgrade wrote, or a key twice is refused before
# anything is stopped, and nothing in it runs.
cp "$opt/state/beta" "$work/state.good"
grep -qx 'upgrade_tag=v0.0.2' "$work/state.good" || fail "the rollback state does not name the tag: $(cat "$work/state.good")"
# shellcheck disable=SC2016 # the payloads are literal
inject() { # <case>: write an altered rollback record for beta
    case $1 in
        extra-line) { cat "$work/state.good"; printf '%s\n' 'x=$(touch "$PWN")'; } ;;
        bare-command) { cat "$work/state.good"; printf '%s\n' 'touch "$PWN"'; } ;;
        value-subst) sed 's/^backup_before=\(.*\)$/backup_before=\1$(touch "$PWN")/' "$work/state.good" ;;
        value-semicolon) sed 's/^previous_tag=\(.*\)$/previous_tag=\1;touch "$PWN"/' "$work/state.good" ;;
        other-archive) sed "s|^backup_before=.*|backup_before=$work/elsewhere/pre-upgrade-v0.0.2-beta-20260101T000000Z.tar.gz|" "$work/state.good" ;;
        twice) { cat "$work/state.good"; grep '^previous_tag=' "$work/state.good"; } ;;
    esac >"$opt/state/beta"
}
for c in extra-line bare-command value-subst value-semicolon other-archive twice; do
    inject "$c"
    calls=$(wc -l <"$work/systemctl.calls")
    if up --rollback beta >"$work/inject-$c.out" 2>&1; then fail "injected state ($c): the rollback went ahead"; fi
    grep -q 'refusing it. Nothing was stopped.' "$work/inject-$c.out" || fail "injected state ($c): wrong reason: $(cat "$work/inject-$c.out")"
    [ ! -e "$PWN" ] || fail "injected state ($c): a command in the rollback record ran"
    [ "$(wc -l <"$work/systemctl.calls")" = "$calls" ] && [ "$(version)" = v0.0.2 ] || fail "injected state ($c): the instance was stopped or changed"
done
cp "$work/state.good" "$opt/state/beta"
ok "rollback: a rollback record with an injected line or value is refused, and nothing in it runs"

post "after upgrade, no migration"
up --rollback beta >"$work/rb1.out" 2>&1 || { cat "$work/rb1.out" >&2; fail "rollback failed"; }
[ "$(version)" = v0.0.1 ] || fail "after the rollback /healthz says $(version)"
grep -q 'schema version unchanged' "$work/rb1.out" || fail "rollback did not say the data was kept"
has 'after upgrade, no migration' || fail "a rollback without a migration lost data"
ok "rollback without a migration: v0.0.1 again, data kept"

up v0.0.2 beta >"$work/up2.out" 2>&1 || { cat "$work/up2.out" >&2; fail "second upgrade failed"; }
# Stand in for a migration: the new binary raised the schema version.
sqlite3 "$up_data/beta/conch.db" 'PRAGMA user_version = 999' >/dev/null
post "after upgrade, with migration"

# The rollback that needs the pre-upgrade backup checks it before stopping
# anything.
pre=$(sed -n 's/^backup_before=//p' "$opt/state/beta")
mv "$pre" "$work/held.tar.gz"
calls=$(wc -l <"$work/systemctl.calls")
if up --rollback beta >"$work/rb-missing.out" 2>&1; then fail "a rollback went ahead without the backup it needs"; fi
grep -q 'Nothing was stopped' "$work/rb-missing.out" || fail "rollback without its backup: wrong reason: $(cat "$work/rb-missing.out")"
[ "$(wc -l <"$work/systemctl.calls")" = "$calls" ] && [ "$(version)" = v0.0.2 ] || fail "a refused rollback stopped or changed the instance"
# A symlink in place of the archive, even to the archive itself, is refused.
ln -s "$work/held.tar.gz" "$pre"
calls=$(wc -l <"$work/systemctl.calls")
if up --rollback beta >"$work/rb-symlink.out" 2>&1; then fail "a rollback went ahead from a symlinked archive"; fi
grep -q 'is a symlink' "$work/rb-symlink.out" && grep -q 'Nothing was stopped' "$work/rb-symlink.out" ||
    fail "rollback from a symlinked archive: wrong reason: $(cat "$work/rb-symlink.out")"
[ "$(wc -l <"$work/systemctl.calls")" = "$calls" ] && [ "$(version)" = v0.0.2 ] || fail "a refused rollback stopped or changed the instance"
rm -f "$pre"
mv "$work/held.tar.gz" "$pre"
ok "rollback: a missing or symlinked pre-upgrade backup is found before anything is stopped"

up --rollback beta >"$work/rb2.out" 2>&1 || { cat "$work/rb2.out" >&2; fail "rollback after a migration failed"; }
[ "$(version)" = v0.0.1 ] || fail "after the second rollback /healthz says $(version)"
grep -q 'restoring the pre-upgrade backup' "$work/rb2.out" || fail "rollback did not restore the backup"
# The restored backup is made safe before it is served (issue #716): the
# token it holds is refused, so its messages are read from the database.
grep -q '^restore: signed out [1-9][0-9]* principal' "$work/rb2.out" || fail "the rollback did not run conchd restore: $(cat "$work/rb2.out")"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $btoken" "http://127.0.0.1:$bport/v2/channels/ops/messages")
[ "$code" = 401 ] || fail "the pre-upgrade backup's token = $code after the rollback restored it, want 401"
db_has() { [ "$(sqlite3 "$up_data/beta/conch.db" "SELECT count(*) FROM messages WHERE body = '$1'")" = 1 ]; }
db_has 'after upgrade, no migration' || fail "the pre-upgrade backup lost an older message"
if db_has 'after upgrade, with migration'; then fail "a message written after the migration survived the restore"; fi
kept=$(sed -n 's/.*the replaced data directory is kept at //p' "$work/rb2.out")
case "$kept" in "$up_data"/beta.rolled-back-*) ;; *) fail "the rollback did not print the kept directory: $(cat "$work/rb2.out")" ;; esac
[ -f "$kept/conch.db" ] || fail "the kept directory $kept does not hold the replaced data"
ok "rollback with a migration: pre-upgrade backup restored and made safe, old binary starts, kept directory named"

# A previous release from before conchd restore: its usage does not name
# the command, the command does not exist, and its serve does not know
# restore-pending, so it would serve the backup's sign-ins if it were ever
# started on them. The rollback that would need it is refused before
# anything is stopped.
up v0.0.2 beta >"$work/up3.out" 2>&1 || { cat "$work/up3.out" >&2; fail "third upgrade failed"; }
sqlite3 "$up_data/beta/conch.db" 'PRAGMA user_version = 999' >/dev/null
mv "$opt/releases/v0.0.1/conchd" "$work/real-v0.0.1"
# shellcheck disable=SC2016 # expanded by the stand-in, not here
printf '#!/bin/sh\n# stand-in for a conchd from before conchd restore\ncase "$1" in\nrestore) echo %s >&2; exit 1 ;;\nhelp | -h | --help) echo %s; exit 0 ;;\nserve) [ -z "$CONCHD_DATA" ] || rm -f "$CONCHD_DATA/restore-pending" ;;\nesac\nexec %s "$@"\n' \
    "'conchd: unknown command \"restore\"'" "'conchd - the Conch server (no restore command)'" "'$work/real-v0.0.1'" >"$opt/releases/v0.0.1/conchd"
chmod 0755 "$opt/releases/v0.0.1/conchd"
calls=$(wc -l <"$work/systemctl.calls")
bpid=$(cat "$work/pids/beta")
rolled=$(ls -d "$up_data"/beta.rolled-back-* | wc -l)
if up --rollback beta >"$work/rb-old.out" 2>&1; then fail "a rollback to a release without conchd restore went ahead"; fi
grep -q 'has no conchd restore' "$work/rb-old.out" && grep -q 'Nothing was stopped' "$work/rb-old.out" ||
    fail "rollback to a release without conchd restore: wrong reason: $(cat "$work/rb-old.out")"
[ "$(wc -l <"$work/systemctl.calls")" = "$calls" ] || fail "rollback to a release without conchd restore: systemctl was called: $(tail -n 2 "$work/systemctl.calls")"
[ "$(cat "$work/pids/beta")" = "$bpid" ] && kill -0 "$bpid" && [ "$(version)" = v0.0.2 ] ||
    fail "rollback to a release without conchd restore: beta was stopped or changed"
[ "$(readlink "$opt/current")" = "$opt/releases/v0.0.2" ] || fail "rollback to a release without conchd restore: current moved"
[ "$(ls -d "$up_data"/beta.rolled-back-* | wc -l)" = "$rolled" ] && [ ! -e "$up_data/beta/restore-pending" ] ||
    fail "rollback to a release without conchd restore: beta's data was replaced"
ok "rollback: a previous release without conchd restore is refused before anything is stopped"

# A previous release that has conchd restore, which fails: the rollback
# restores the backup and stops there, with the instance stopped and its
# data unservable. Run again once conchd restore works, it finishes.
norestore "$work/real-v0.0.1" "$opt/releases/v0.0.1/conchd"
if up --rollback beta >"$work/rb3.out" 2>&1; then fail "a rollback finished although conchd restore failed"; fi
grep -q 'conchd restore failed on the restored backup' "$work/rb3.out" && grep -q 'run this rollback again' "$work/rb3.out" ||
    fail "rollback whose conchd restore fails: wrong reason: $(cat "$work/rb3.out")"
[ -e "$up_data/beta/restore-pending" ] || fail "a rollback whose conchd restore failed removed restore-pending"
if curl -fsS "http://127.0.0.1:$bport/healthz" >/dev/null 2>&1; then fail "beta serves after a rollback whose conchd restore failed"; fi
if timeout 20 "$work/real-v0.0.1" serve --data "$up_data/beta" --listen 127.0.0.1:0 --no-web >"$work/pending-beta.log" 2>&1; then
    fail "serve started on beta's unfinished restore"
fi
install -m 0755 "$work/real-v0.0.1" "$opt/releases/v0.0.1/conchd"
up --rollback beta >"$work/rb4.out" 2>&1 || { cat "$work/rb4.out" >&2; fail "the rollback run again failed"; }
grep -q '^restore: signed out' "$work/rb4.out" || fail "the rollback run again did not finish the restore: $(cat "$work/rb4.out")"
[ "$(version)" = v0.0.1 ] && [ ! -e "$up_data/beta/restore-pending" ] || fail "the rollback run again did not bring beta back on v0.0.1"
ok "rollback: a conchd restore that fails leaves beta stopped and unservable; run again, the rollback finishes"

echo "deploy-scripts-test: PASS"
