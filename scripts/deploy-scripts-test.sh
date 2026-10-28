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
#                     injected line, are refused and nothing in them runs;
#                     and two instances upgraded and rolled back together
#   add-instance.sh   (issue #861) every refusal (bad, reserved or taken name,
#                     taken port, unresolved or wrong DNS, a wrong domain,
#                     --temporary with --operator-totp required, no terminal
#                     and no --yes) changes nothing; --dry-run changes
#                     nothing; a failure partway is undone to the same bytes;
#                     success makes exactly the user, data, key, env file,
#                     marked Caddy block and tmpfiles rule, starts a real
#                     conchd, prints the setup link once, and no generated
#                     secret is printed, logged or written elsewhere
#   remove-instance.sh  refuses a normal server without --force-normal, a
#                     wrongly typed name, no terminal, a symlinked data
#                     directory or env file, and an unmarked (hand-made)
#                     Caddy block; removal leaves the droplet as it was
#                     before the add, apart from the archive in removed/
#   list-instances.sh, ops/conch-server  against stand-ins for ssh (which
#                     runs the remote side here, so the quoting is real),
#                     doctl and DNS-over-HTTPS: new, list, upgrade, rm, sync;
#                     no secret in their logs
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

for tool in sqlite3 curl tar sha256sum script setsid; do
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
    shellcheck "$deploy"/*.sh ops/conch-server scripts/deploy-scripts-test.sh scripts/dev-stack.sh || fail "shellcheck"
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
    # The units name binaries that exist only on the droplet; that is the one
    # complaint allowed.
    out=$(systemd-analyze verify "$units"/* 2>&1 || true)
    out=$(printf '%s\n' "$out" | grep -v 'is not executable' || true)
    [ -z "$out" ] || fail "systemd-analyze verify:
$out"
    ok "systemd-analyze verify"
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
# Every call is logged, so a test can say nothing was stopped. Units other
# than conchd@<instance> are only logged; reload or restart of one fails
# once if $work/fail-reload-<unit> or $work/fail-restart-<unit> exists. Once
# each: $work/fail-start-conchd@<i> starts a process that never answers, and
# $work/fail-stop-conchd@<i> makes a stop fail and leave it running. Starting
# conch-backup@<i>.timer makes $work/upbackups/<i>, as a backup at once would.
cat >"$work/systemctl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
echo "\$*" >>"$work/systemctl.calls"
verb=\$1
shift
[ "\${1:-}" != --quiet ] || shift
unit=\${1:-}
inst=""
case "\$unit" in conchd@*) inst=\${unit#conchd@} ;; esac
pidfile=$work/pids/\$inst
case "\$verb" in
stop)
    if [ -e "$work/fail-stop-\$unit" ]; then rm -f "$work/fail-stop-\$unit"; exit 1; fi
    if [ -n "\$inst" ] && [ -f "\$pidfile" ]; then
        pid=\$(cat "\$pidfile")
        kill "\$pid" 2>/dev/null || true
        for _ in \$(seq 1 100); do kill -0 "\$pid" 2>/dev/null || break; sleep 0.1; done
        rm -f "\$pidfile"
    fi ;;
start)
    if [ -n "\$inst" ] && [ -e "$work/fail-start-\$unit" ]; then
        rm -f "$work/fail-start-\$unit"
        nohup sleep 300 </dev/null >/dev/null 2>&1 &
        echo \$! >"\$pidfile"
    elif [ -n "\$inst" ]; then
        set -a; . "$etc/\$inst.env"; set +a
        # nohup: a stand-in for a unit, so the end of a test terminal (script) does not hang it up
        nohup "$opt/current/conchd" serve --no-web </dev/null >>"$work/\$inst.log" 2>&1 &
        echo \$! >"\$pidfile"
    else
        case "\$unit" in
        conch-backup@*.timer)
            b=\${unit#conch-backup@}
            mkdir -p "$work/upbackups/\${b%.timer}" ;;
        esac
    fi ;;
is-active) [ -n "\$inst" ] && [ -f "\$pidfile" ] && kill -0 "\$(cat "\$pidfile")" 2>/dev/null ;;
reload) if [ -e "$work/fail-reload-\$unit" ]; then rm -f "$work/fail-reload-\$unit"; exit 1; fi ;;
restart) if [ -e "$work/fail-restart-\$unit" ]; then rm -f "$work/fail-restart-\$unit"; exit 1; fi ;;
enable | disable) ;;
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

# --- add-instance, remove-instance, list-instances (issue #861) --------------
#
# The droplet as #567 left it, in miniature, beside beta from the upgrade test:
# a hand-made alpha whose Caddy block has no markers, LiveKit's key file, a
# stray alpha.env.bak-alpha2 and a backup.env (neither is an instance), and
# the shared users. The stand-ins below log every call. They find this test's
# directory in CONCH_TEST_WORK, so their bodies need no escaping.

export CONCH_TEST_WORK=$work
sb=$work/standins
mkdir -p "$sb" "$work/caddy" "$work/tmpfiles" "$work/metadata/interfaces/public/0/ipv4" \
    "$work/metadata/interfaces/public/0/ipv6"
caddyfile=$work/caddy/Caddyfile
sed -e 's/example\.com/example.test/g' -e '/^\temail /d' "$deploy/Caddyfile" >"$caddyfile"
cp -p "$caddyfile" "$work/Caddyfile.as-deployed"
# The metadata service, its IPv6 written out in full and upper case, as the
# resolver will not write it.
printf '203.0.113.7' >"$work/metadata/interfaces/public/0/ipv4/address"
printf '2001:0DB8:0000:0000:0000:0000:0000:0007' >"$work/metadata/interfaces/public/0/ipv6/address"
# DNS for example.test: "<id> <type> <name> <data>", kept by the doctl stand-in.
printf '1 A alpha 203.0.113.7\n2 AAAA alpha 2001:db8::7\n' >"$work/dns-records"
printf '%s\n' 'conch-alpha:x:990:990::/nonexistent:/usr/sbin/nologin' 'conch-beta:x:991:991::/nonexistent:/usr/sbin/nologin' \
    'conch-backup:x:992:992::/nonexistent:/usr/sbin/nologin' 'conch-drill:x:993:993::/nonexistent:/usr/sbin/nologin' >"$work/passwd"
printf 'alpha-a9ea7ac7: %s\nbeta-0123abcd: %s\n' "$(printf '%064d' 1)" "$(printf '%064d' 2)" >"$etc/livekit-keys"
chmod 0600 "$etc/livekit-keys"
mkdir -m 0700 "$up_data/alpha"
printf 'CONCHD_DATA=%s\nCONCHD_LISTEN=127.0.0.1:8080\nCONCHD_LIVEKIT_URL=wss://livekit.example.test\nCONCHD_LIVEKIT_API_KEY=alpha-a9ea7ac7\nCONCHD_OPERATOR_TOTP=optional\n' \
    "$up_data/alpha" >"$etc/alpha.env"
# If *.env matched these, port 8082 would look taken and "backup" would be an
# instance.
printf 'CONCHD_LISTEN=127.0.0.1:8082\n' >"$etc/alpha.env.bak-alpha2"
printf 'CONCH_BACKUP_KEEP_DAYS=14\nCONCHD_LISTEN=127.0.0.1:8082\n' >"$etc/backup.env"
chmod 0600 "$etc"/*.env "$etc/alpha.env.bak-alpha2"
: >"$work/ss-taken"
echo 8081 >>"$work/ss-taken"

cat >"$sb/caddy" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for caddy validate: fails while fail-caddy exists, and on unbalanced braces
w=$CONCH_TEST_WORK
printf '%s\n' "$*" >>"$w/caddy.calls"
[ "$1" = validate ] && [ "$2" = --config ] && [ "$4 $5" = "--adapter caddyfile" ] || exit 2
if [ -e "$w/lose-keys-orig" ]; then rm -f "$CONCH_ETC"/.livekit-keys.orig.*; fi
if [ -e "$w/fail-caddy" ]; then echo "Error: adapting config using caddyfile: stand-in failure" >&2; exit 1; fi
o=$(grep -v '^[[:space:]]*#' "$3" | tr -cd '{' | wc -c) c=$(grep -v '^[[:space:]]*#' "$3" | tr -cd '}' | wc -c)
[ "$o" = "$c" ] || { echo "Error: unbalanced braces" >&2; exit 1; }
echo "Valid configuration"
STANDIN
cat >"$sb/useradd" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for useradd, on the test's passwd
w=$CONCH_TEST_WORK
printf '%s\n' "$*" >>"$w/useradd.calls"
[ "$*" = "--system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ${!#}" ] || exit 2
! grep -q "^${!#}:" "$w/passwd" || exit 9
echo "${!#}:x:999:999::/nonexistent:/usr/sbin/nologin" >>"$w/passwd"
STANDIN
cat >"$sb/userdel" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for userdel, on the test's passwd; the shared users must never be named
w=$CONCH_TEST_WORK
printf '%s\n' "$*" >>"$w/useradd.calls"
case "$1" in conch-backup | conch-drill) touch "$w/shared-user-touched"; exit 9 ;; esac
[ ! -e "$w/fail-userdel" ] || { echo "userdel: stand-in failure" >&2; exit 1; }
grep -q "^$1:" "$w/passwd" || exit 6
sed -i "/^$1:/d" "$w/passwd"
STANDIN
cat >"$sb/getent" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for getent: passwd from the test's file; ahostsv4/ahostsv6 from the
# test's DNS records, an A-only name answering ahostsv6 with mapped addresses
# as glibc does
w=$CONCH_TEST_WORK
printf '%s\n' "$*" >>"$w/getent.calls"
case "$1" in
    passwd) grep "^$2:" "$w/passwd" ;;
    ahostsv4 | ahostsv6)
        n=${2%.example.test}
        [ "$n" != "$2" ] || exit 2
        t=A
        [ "$1" = ahostsv4 ] || t=AAAA
        out=$(awk -v t="$t" -v n="$n" '$2 == t && $3 == n { print $4 }' "$w/dns-records")
        if [ "$t" = AAAA ]; then out+=$(awk -v n="$n" '$2 == "A" && $3 == n { printf "\n::ffff:%s", $4 }' "$w/dns-records"); fi
        [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ] || exit 2
        printf '%s\n' "$out" | sed '/^$/d' | while read -r a; do printf '%s STREAM %s\n%s DGRAM\n' "$a" "$2" "$a"; done ;;
    *) exit 1 ;;
esac
STANDIN
cat >"$sb/ss" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for ss -Hltn 'sport = :N': a port in ss-taken listens; otherwise the real ss answers
w=$CONCH_TEST_WORK
printf '%s\n' "$*" >>"$w/ss.calls"
p=${!#}
p=${p##*:}
if grep -qx "$p" "$w/ss-taken"; then echo "LISTEN 0 4096 127.0.0.1:$p 0.0.0.0:*"; exit 0; fi
# The ports this test picks at random (20000 and up) may really be taken.
if [ "$p" -ge 20000 ] && command -v ss >/dev/null; then exec ss "$@"; fi
STANDIN
cat >"$sb/public-curl" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for the check through Caddy: https://<name>.example.test/healthz is
# asked of that instance's loopback port; fails while fail-public exists
w=$CONCH_TEST_WORK
printf '%s\n' "$*" >>"$w/public-curl.calls"
[ ! -e "$w/fail-public" ] || exit 7
u=${!#}
h=${u#https://}
h=${h%%/*}
l=$(sed -n 's/^CONCHD_LISTEN=//p' "$w/etc/${h%%.*}.env" | tail -n 1)
exec curl -fsS --max-time 5 "http://$l/healthz"
STANDIN
chmod 0755 "$sb"/*

# The droplet's locations and commands, for add-instance, remove-instance and
# list-instances here, and for the ssh stand-in below.
cat >"$work/droplet.env" <<EOF
export CONCH_OPT=$opt CONCH_ETC=$etc CONCH_DATA_ROOT=$up_data CONCH_BACKUP_DIR=$work/upbackups
export CONCH_CADDYFILE=$caddyfile CONCH_TMPFILES_DIR=$work/tmpfiles CONCH_METADATA_URL=file://$work/metadata
export SYSTEMCTL=$work/systemctl CADDY=$sb/caddy USERADD=$sb/useradd USERDEL=$sb/userdel GETENT=$sb/getent SS=$sb/ss
export CONCH_PUBLIC_CURL=$sb/public-curl CONCH_RELEASE_URL=file://$rel
export CONCH_HEALTH_TIMEOUT=\${CONCH_HEALTH_TIMEOUT:-10} CONCH_LOOPBACK_TIMEOUT=\${CONCH_LOOPBACK_TIMEOUT:-30}
EOF
for s in add-instance remove-instance list-instances; do
    printf '#!/usr/bin/env bash\n. %q\nexec %q "$@"\n' "$work/droplet.env" "$deploy/$s.sh" >"$work/$s"
    chmod 0755 "$work/$s"
done
ai() { "$work/add-instance" "$@"; }
# onpty <command>...: run it on a terminal (script(1)), as ssh -t would,
# carriage returns dropped from what it prints. With ONPTY_ERR=<file> its
# standard error goes to that file instead, so the two can be told apart.
onpty() {
    local cmd
    cmd=$(printf '%q ' "$@")
    [ -z "${ONPTY_ERR:-}" ] || cmd+=" 2>$(printf '%q' "$ONPTY_ERR")"
    SHELL=/bin/bash script -qec "$cmd" /dev/null </dev/null | tr -d '\r'
}
# typed <answer> <command>...: run it on a terminal, typing answer.
typed() {
    local a=$1
    shift
    printf '%s\n' "$a" | SHELL=/bin/bash script -qec "$(printf '%q ' "$@")" /dev/null | tr -d '\r'
}

# tree: every path under the droplet's locations with its type, mode and
# symlink target, and the SHA-256 of every file; of the data root only the
# entries (beta's running conchd changes what is inside its directory).
tree() {
    {
        local r
        for r in "$etc" "$work/caddy" "$work/tmpfiles" "$opt" "$work/upbackups" "$work/passwd"; do
            [ -e "$r" ] || continue
            find "$r" -printf '%p %y %m %l\n'
            find "$r" -type f -exec sha256sum {} +
        done
        find "$up_data" -mindepth 1 -maxdepth 1 -printf '%p %y %m\n'
    } | sort
}
# unchanged <snapshot> <what>: the tree is as the snapshot has it.
unchanged() {
    local d
    d=$(diff "$1" <(tree) || true)
    [ -z "$d" ] || fail "$2: the droplet changed:
$d"
}
calls() { { cat "$work"/systemctl.calls "$work"/caddy.calls "$work"/useradd.calls 2>/dev/null || true; } | wc -l; }

tree >"$work/t0"
c0=$(calls)

# Each refusal changes nothing and runs nothing.
refuse() { # <expected exit> <reason> <args to add-instance.sh>...
    local want=$1 reason=$2 status=0
    shift 2
    ai "$@" >"$work/refuse.out" 2>&1 || status=$?
    [ "$status" = "$want" ] || fail "add-instance $*: exit $status, want $want: $(cat "$work/refuse.out")"
    grep -qF -- "$reason" "$work/refuse.out" || fail "add-instance $*: wrong reason: $(cat "$work/refuse.out")"
    unchanged "$work/t0" "add-instance $*"
    [ "$(calls)" = "$c0" ] || fail "add-instance $*: called systemctl, caddy or useradd"
}
for bad in Gamma 1gamma gamma- g_amma 'gamma;id' a2345678901234567890123456z; do
    refuse 2 'bad name' "$bad" example.test --yes
done
for r in livekit turn backup drill www removed; do refuse 2 'is reserved' "$r" example.test --yes; done
refuse 2 'bad domain' gamma 'example.test;id' --yes
refuse 2 '--temporary with --operator-totp required is refused' gamma example.test --temporary --operator-totp required --yes
# shellcheck disable=SC2016 # the payload is literal
refuse 2 'bad operator name' gamma example.test --operator '$(id)' --yes
refuse 1 'already exists' beta example.test --yes
refuse 1 'already exists' alpha example.test --yes
echo 'conch-epsilon:x:994:994::/nonexistent:/usr/sbin/nologin' >>"$work/passwd"
tree >"$work/t0"
refuse 1 'the user conch-epsilon already exists' epsilon example.test --yes
sed -i '/^conch-epsilon:/d' "$work/passwd"
tree >"$work/t0"
printf '3 A gamma 203.0.113.7\n4 AAAA gamma 2001:db8::7\n' >>"$work/dns-records"
refuse 1 'port 8081 is taken' gamma example.test --port 8081 --yes
bport_env=$(sed -n 's/^CONCHD_LISTEN=127.0.0.1://p' "$etc/beta.env")
refuse 1 "port $bport_env is taken" gamma example.test --port "$bport_env" --yes
refuse 2 'bad port' gamma example.test --port 80 --yes
refuse 1 'does not resolve to this droplet' zeta example.test --yes
grep -qF 'doctl compute domain records create example.test --record-type AAAA --record-name zeta --record-data 2001:db8::7' "$work/refuse.out" ||
    fail "an unresolved name: the records to create are not printed: $(cat "$work/refuse.out")"
printf '5 A eta 198.51.100.9\n6 AAAA eta 2001:db8::7\n' >>"$work/dns-records"
refuse 1 'does not resolve to this droplet' eta example.test --yes
printf '7 A theta 203.0.113.7\n' >>"$work/dns-records"
refuse 1 'does not resolve to this droplet' theta example.test --yes
refuse 1 'uses CONCHD_LIVEKIT_URL=wss://livekit.example.test' gamma example.org --yes
ok "add-instance: bad, reserved and taken names, taken ports, unresolved or wrong DNS, a wrong domain and --temporary with --operator-totp required are refused before any change"

# --dry-run: every check, the plan, and nothing else. The default port skips
# 8081 (listening) and is not pushed past 8082 by the stray backup files.
ai gamma example.test --temporary --dry-run >"$work/dry.out" 2>&1 || fail "dry run failed: $(cat "$work/dry.out")"
grep -q 'CONCHD_LISTEN=127.0.0.1:8082' "$work/dry.out" || fail "dry run: not port 8082: $(cat "$work/dry.out")"
grep -q 'calls drop for a few seconds on: alpha, beta$' "$work/dry.out" || fail "dry run: does not name the instances whose calls drop: $(cat "$work/dry.out")"
grep -q 'nothing was changed' "$work/dry.out" || fail "dry run: does not say nothing changed"
unchanged "$work/t0" "dry run"
[ "$(calls)" = "$c0" ] || fail "dry run: called systemctl, caddy or useradd"
ok "add-instance --dry-run: prints the plan, changes nothing, runs nothing"

# The setup link is printed to standard output: a file there is refused
# before any change, so the link can never land in one.
status=0
ai gamma example.test --temporary --yes >"$work/nostdout.out" 2>&1 || status=$?
[ "$status" = 1 ] && grep -q 'standard output is not a terminal' "$work/nostdout.out" ||
    fail "standard output not a terminal: exit $status: $(cat "$work/nostdout.out")"
unchanged "$work/t0" "standard output not a terminal"
[ "$(calls)" = "$c0" ] || fail "standard output not a terminal: called systemctl, caddy or useradd"
ok "add-instance: standard output that is not a terminal is refused before any change"

# No terminal to ask on and no --yes: LiveKit's restart is not agreed to.
# (setsid on the pty: its standard output is a terminal, but it has none to
# open as /dev/tty.)
status=0
onpty setsid -w "$work/add-instance" gamma example.test --temporary >"$work/notty.out" 2>&1 || status=$?
[ "$status" = 1 ] && grep -q 'no terminal to ask on' "$work/notty.out" || fail "no terminal, no --yes: $(cat "$work/notty.out")"
unchanged "$work/t0" "no terminal, no --yes"
ok "add-instance: with no terminal and no --yes it refuses, before any change"

# A failure partway is undone, newest first, back to the same bytes: the
# data directory, Caddy's validation and reload, LiveKit's restart, and the
# operator bootstrap (conchd refuses an operator named as the server).
gport=$((20000 + RANDOM % 20000))
up_mode=$(stat -c %a "$up_data")
for f in datadir caddy reload-caddy restart-livekit bootstrap; do
    extra=()
    case $f in
        datadir) chmod 0500 "$up_data" ;;
        caddy) touch "$work/fail-caddy" ;;
        bootstrap) extra=(--operator conch) ;;
        *) touch "$work/fail-$f" ;;
    esac
    status=0
    onpty "$work/add-instance" gamma example.test --temporary --port "$gport" --yes "${extra[@]}" >"$work/undo-$f.out" 2>&1 || status=$?
    rm -f "$work/fail-caddy" "$work/fail-reload-caddy" "$work/fail-restart-livekit"
    chmod "$up_mode" "$up_data"
    [ "$status" = 1 ] || fail "$f failure: add-instance exited $status, want 1 (undone): $(cat "$work/undo-$f.out")"
    unchanged "$work/t0" "$f failure"
    cmp -s "$caddyfile" "$work/Caddyfile.as-deployed" || fail "$f failure: the Caddyfile is not as it was"
    for t in "$work"/caddy/.[!.]* "$etc"/.[!.]*; do
        [ ! -e "$t" ] || fail "$f failure: a temporary file was left: $t"
    done
    case $f in
        caddy) grep -q 'caddy validate refused' "$work/undo-$f.out" ;;
        reload-caddy) grep -q 'undone: restored .*Caddyfile and reloaded Caddy' "$work/undo-$f.out" ;;
        restart-livekit | bootstrap) grep -q 'undone: restarted LiveKit without that key' "$work/undo-$f.out" ;;
        datadir) grep -q 'undone: removed the user conch-gamma' "$work/undo-$f.out" ;;
    esac || fail "$f failure: wrong account: $(cat "$work/undo-$f.out")"
    grep -q 'undone: removed the user conch-gamma' "$work/undo-$f.out" ||
        fail "$f failure: does not say what it undid: $(cat "$work/undo-$f.out")"
done
ok "add-instance: a failure making the data directory, in caddy validate, Caddy's reload, LiveKit's restart or the operator bootstrap is undone back to the same bytes, and says what it undid"

# An undo step that itself fails (here userdel, after caddy validate failed)
# is exit 5, not 1, so conch-server keeps the DNS records of what is left.
touch "$work/fail-caddy" "$work/fail-userdel"
status=0
onpty "$work/add-instance" gamma example.test --temporary --port "$gport" --yes >"$work/undo-fail.out" 2>&1 || status=$?
rm -f "$work/fail-caddy" "$work/fail-userdel"
[ "$status" = 5 ] || fail "a failed undo step: add-instance exited $status, want 5: $(cat "$work/undo-fail.out")"
grep -q "could not undo 'user'" "$work/undo-fail.out" && grep -q 'ROLLBACK INCOMPLETE' "$work/undo-fail.out" ||
    fail "a failed undo step: does not say what is left: $(cat "$work/undo-fail.out")"
grep -q '^conch-gamma:' "$work/passwd" || fail "a failed undo step: the user should be left in place"
sed -i '/^conch-gamma:/d' "$work/passwd"
unchanged "$work/t0" "a failed undo step, once its user is removed by hand"
ok "add-instance: an undo step that fails is exit 5 and names what is left, so the DNS records are kept"

# The same when the LiveKit key file cannot be put back (its saved copy is
# gone): exit 5 and the new key named as left, not exit 1.
touch "$work/fail-caddy" "$work/lose-keys-orig"
status=0
onpty "$work/add-instance" gamma example.test --temporary --port "$gport" --yes >"$work/undo-keys.out" 2>&1 || status=$?
rm -f "$work/fail-caddy" "$work/lose-keys-orig"
[ "$status" = 5 ] && grep -q "could not undo 'key'" "$work/undo-keys.out" ||
    fail "a key file that cannot be put back: add-instance exited $status, want 5: $(cat "$work/undo-keys.out")"
grep -q '^gamma-' "$etc/livekit-keys" || fail "a key file that cannot be put back: the test proves nothing, gamma's key is not there"
sed -i '/^gamma-/d' "$etc/livekit-keys"
unchanged "$work/t0" "a key file that cannot be put back, once the key line is removed by hand"
ok "add-instance: a LiveKit key file that cannot be put back is exit 5, so the DNS records are kept"

# A failure after conchd started (it never answers): the undo stops conchd,
# the backup timer and the backup unit, removes the backup directory a backup
# made during the run, and then the rest, back to the same bytes.
printf '10 A pi 203.0.113.7\n11 AAAA pi 2001:db8::7\n' >>"$work/dns-records"
touch "$work/fail-start-conchd@pi"
: >"$work/systemctl.calls"
status=0
onpty env CONCH_LOOPBACK_TIMEOUT=2 "$work/add-instance" pi example.test --port "$gport" --yes >"$work/undo-pi.out" 2>&1 || status=$?
[ "$status" = 1 ] || fail "a normal server whose conchd never answers: exit $status, want 1: $(cat "$work/undo-pi.out")"
grep -qx 'start conch-backup@pi.timer' "$work/systemctl.calls" && grep -qx 'stop conch-backup@pi.timer' "$work/systemctl.calls" &&
    grep -qx 'stop conch-backup@pi.service' "$work/systemctl.calls" && grep -qx 'stop conchd@pi' "$work/systemctl.calls" ||
    fail "the undo did not stop conchd, the backup timer and the backup unit: $(cat "$work/systemctl.calls")"
grep -q "undone: removed $work/upbackups/pi, which a backup made during this run" "$work/undo-pi.out" ||
    fail "the undo does not say it removed the backup directory: $(cat "$work/undo-pi.out")"
[ ! -e "$work/upbackups/pi" ] && [ ! -e "$work/pids/pi" ] || fail "the undo left pi's backup directory or its process"
unchanged "$work/t0" "a failure after conchd started"
ok "add-instance: a failure after start stops conchd, the backup timer and unit, removes the backup directory the run made, and undoes the rest to the same bytes"

# The same, but conchd will not stop: nothing more is deleted while it may
# run, what is left is named, exit 5; remove-instance takes it all once it
# stops.
printf '12 A sigma 203.0.113.7\n13 AAAA sigma 2001:db8::7\n' >>"$work/dns-records"
touch "$work/fail-start-conchd@sigma" "$work/fail-stop-conchd@sigma"
status=0
onpty env CONCH_LOOPBACK_TIMEOUT=2 "$work/add-instance" sigma example.test --temporary --port "$gport" --yes >"$work/undo-sigma.out" 2>&1 || status=$?
[ "$status" = 5 ] && grep -q 'ROLLBACK INCOMPLETE' "$work/undo-sigma.out" && grep -q "Left in place.*$etc/sigma.env" "$work/undo-sigma.out" ||
    fail "a conchd that will not stop: exit $status, want 5 and what is left: $(cat "$work/undo-sigma.out")"
[ -f "$etc/sigma.env" ] && [ -d "$up_data/sigma" ] && grep -q '^conch-sigma:' "$work/passwd" && grep -q '^sigma-' "$etc/livekit-keys" ||
    fail "the undo deleted sigma's files while its conchd may still run"
kill -0 "$(cat "$work/pids/sigma")" || fail "sigma's process is gone, so the test proves nothing"
typed sigma "$work/remove-instance" sigma >"$work/rm-sigma.out" 2>&1 || fail "removing what the incomplete undo left failed: $(cat "$work/rm-sigma.out")"
d=$(diff <(grep -vF -e "$work/upbackups/removed" -e "$work/tmpfiles/conch-removed.conf" "$work/t0") \
    <(tree | grep -vF -e "$work/upbackups/removed" -e "$work/tmpfiles/conch-removed.conf") || true)
[ -z "$d" ] || fail "remove-instance did not clear what the incomplete undo left:
$d"
ok "add-instance: when conchd will not stop, the undo deletes nothing more, names what is left and exits 5; remove-instance then clears it"

# The same when stopping the backup timer fails, though conchd stops: a
# failed stop blocks every delete too, exit 5; remove-instance then clears it.
printf '14 A tau 203.0.113.7\n15 AAAA tau 2001:db8::7\n' >>"$work/dns-records"
touch "$work/fail-start-conchd@tau" "$work/fail-stop-conch-backup@tau.timer"
status=0
onpty env CONCH_LOOPBACK_TIMEOUT=2 "$work/add-instance" tau example.test --temporary --port "$gport" --yes >"$work/undo-tau.out" 2>&1 || status=$?
rm -f "$work/fail-stop-conch-backup@tau.timer"
[ "$status" = 5 ] && grep -q 'ROLLBACK INCOMPLETE' "$work/undo-tau.out" && grep -q 'systemctl stop conch-backup@tau.timer failed' "$work/undo-tau.out" ||
    fail "a backup timer that will not stop: exit $status, want 5: $(cat "$work/undo-tau.out")"
[ -f "$etc/tau.env" ] && [ -d "$up_data/tau" ] && grep -q '^conch-tau:' "$work/passwd" ||
    fail "the undo deleted tau's files though its backup timer would not stop"
typed tau "$work/remove-instance" tau >"$work/rm-tau.out" 2>&1 || fail "removing what tau's incomplete undo left failed: $(cat "$work/rm-tau.out")"
d=$(diff <(grep -vF -e "$work/upbackups/removed" -e "$work/tmpfiles/conch-removed.conf" "$work/t0") \
    <(tree | grep -vF -e "$work/upbackups/removed" -e "$work/tmpfiles/conch-removed.conf") || true)
[ -z "$d" ] || fail "remove-instance did not clear what tau's incomplete undo left:
$d"
ok "add-instance: when the backup timer will not stop, the undo deletes nothing more and exits 5; remove-instance then clears it"

# Success: a temporary server with password-only operators.
: >"$work/systemctl.calls"
ONPTY_ERR=$work/add.err onpty "$work/add-instance" gamma example.test --temporary --operator-totp optional --operator "Nick O'Brien" \
    --port "$gport" --yes >"$work/add.out" || fail "add-instance gamma failed: $(cat "$work/add.out" "$work/add.err")"
genv=$etc/gamma.env
[ "$(stat -c %a "$genv")" = 600 ] || fail "gamma.env is mode $(stat -c %a "$genv")"
while read -r k; do
    grep -q "^$k=." "$genv" || fail "gamma.env has no $k"
done < <(sed -n 's/^\(CONCHD_[A-Z_]*\)=.*/\1/p' "$deploy/instance.env.example")
grep -qx 'CONCHD_MODE=temporary' "$genv" && grep -qx 'CONCHD_OPERATOR_TOTP=optional' "$genv" &&
    grep -qx 'CONCHD_LIVEKIT_ROOM_PREFIX=gamma-' "$genv" && grep -qx "CONCHD_LISTEN=127.0.0.1:$gport" "$genv" &&
    grep -qx 'CONCHD_LIVEKIT_URL=wss://livekit.example.test' "$genv" || fail "gamma.env: $(grep -v SECRET "$genv" | grep -v TOTP_KEY)"
gkey=$(sed -n 's/^CONCHD_LIVEKIT_API_KEY=//p' "$genv")
gsecret=$(sed -n 's/^CONCHD_LIVEKIT_API_SECRET=//p' "$genv")
gtotp=$(sed -n 's/^CONCHD_TOTP_KEY=//p' "$genv")
[[ $gkey =~ ^gamma-[0-9a-f]{8}$ ]] && [[ $gsecret =~ ^[0-9a-f]{64}$ ]] && [ "$(printf '%s' "$gtotp" | base64 -d | wc -c)" = 32 ] ||
    fail "gamma's key, secret or TOTP key is not in the runbook's format"
grep -qxF "$gkey: $gsecret" "$etc/livekit-keys" || fail "the key pair is not in livekit-keys"
[ "$(stat -c %a "$etc/livekit-keys")" = 600 ] || fail "livekit-keys lost its mode"
[ "$(stat -c %a "$up_data/gamma")" = 700 ] || fail "gamma's data directory is mode $(stat -c %a "$up_data/gamma")"
grep -q '^conch-gamma:' "$work/passwd" || fail "no user conch-gamma"
grep -qx '# conch-instance gamma begin' "$caddyfile" && grep -qx 'gamma.example.test {' "$caddyfile" &&
    grep -qx $'\treverse_proxy 127.0.0.1:'"$gport" "$caddyfile" || fail "no marked Caddy block for gamma: $(cat "$caddyfile")"
grep -qx 'd '"$work/upbackups"'/removed 0700 conch-backup conch-backup 14d' "$work/tmpfiles/conch-removed.conf" ||
    fail "the tmpfiles rule is not right: $(cat "$work/tmpfiles/conch-removed.conf")"
grep -qx 'reload caddy' "$work/systemctl.calls" && grep -qx 'restart livekit' "$work/systemctl.calls" &&
    grep -qx 'start conchd@gamma' "$work/systemctl.calls" || fail "gamma: systemctl calls: $(cat "$work/systemctl.calls")"
if grep -q 'conch-backup@gamma.timer' "$work/systemctl.calls"; then fail "a temporary server got a backup timer"; fi
curl -fsS "http://127.0.0.1:$gport/healthz" | grep -q '"server_mode":"temporary"' || fail "gamma's /healthz does not say temporary"
[ "$(grep -c '^https://gamma.example.test/app/#/setup/' "$work/add.out")" = 1 ] || fail "the setup link is not on stdout once: $(cat "$work/add.out")"
grep -q "setup link for operator \"Nick O'Brien\"" "$work/add.err" || fail "the operator's name did not reach conchd whole: $(cat "$work/add.err")"
link=$(grep '^https://gamma.example.test/app/#/setup/' "$work/add.out")
# No secret is printed, logged by a stand-in, or written outside the env file
# (and, for the LiveKit secret, the key file).
for s in "$gsecret" "$gtotp" "$link"; do
    if grep -rqF -- "$s" "$work/add.err" "$work"/*.calls "$work/gamma.log"; then fail "a secret was printed or logged"; fi
done
if grep -qF -- "$gsecret" "$work/add.out" || grep -qF -- "$gtotp" "$work/add.out"; then fail "a secret is on stdout"; fi
for s in "$gsecret" "$gtotp"; do
    where=$(grep -rlF -- "$s" "$etc" "$work/caddy" "$work/tmpfiles" "$work/upbackups" "$up_data" "$opt" "$work/passwd" | sort)
    want=$genv
    [ "$s" != "$gsecret" ] || want=$(printf '%s\n' "$genv" "$etc/livekit-keys" | sort)
    [ "$where" = "$want" ] || fail "a secret is in $where"
done
ok "add-instance gamma --temporary: user, data, key, env (every key, temporary, optional), Caddy block, tmpfiles rule, LiveKit restarted, no timer; /healthz ok and temporary; the setup link once on stdout; no secret printed, logged or written elsewhere"

# The same name again: refused, nothing changed.
tree >"$work/t1"
status=0
ai gamma example.test --temporary --yes >"$work/again.out" 2>&1 || status=$?
[ "$status" = 1 ] && grep -q 'already exists' "$work/again.out" || fail "a second add of gamma: $(cat "$work/again.out")"
unchanged "$work/t1" "a second add of gamma"
ok "add-instance: a second run for the same name is refused and changes nothing"

# list-instances: every instance, its mode and health; not the stray files.
"$work/list-instances" >"$work/list.out" 2>&1 || fail "list-instances failed: $(cat "$work/list.out")"
grep -Eq '^gamma +[0-9]+ +temporary +optional +active +ok v0\.0\.1$' "$work/list.out" &&
    grep -Eq '^beta +[0-9]+ +normal +required +active +ok v0\.0\.1$' "$work/list.out" &&
    grep -Eq '^alpha +8080 +normal +optional +inactive +no answer$' "$work/list.out" || fail "list-instances: $(cat "$work/list.out")"
[ "$(grep -c '^[a-z]' "$work/list.out")" = 3 ] || fail "list-instances lists something that is not an instance: $(cat "$work/list.out")"
[ "$("$work/list-instances" --names | tr '\n' ' ')" = 'alpha beta gamma ' ] || fail "list-instances --names: $("$work/list-instances" --names)"
[ "$("$work/list-instances" --addresses | tr '\n' ' ')" = 'ipv4 203.0.113.7 ipv6 2001:db8::7 ' ] ||
    fail "list-instances --addresses: $("$work/list-instances" --addresses)"
ok "list-instances: each instance with port, mode, TOTP setting, unit and /healthz; not backup.env or alpha.env.bak-alpha2"

# remove-instance refusals, each before any change.
ri_refuse() { # <reason> <command>...
    local reason=$1 status=0
    shift
    "$@" >"$work/ri.out" 2>&1 || status=$?
    [ "$status" != 0 ] && grep -qF -- "$reason" "$work/ri.out" || fail "remove-instance ($*): $(cat "$work/ri.out")"
    unchanged "$work/t1" "remove-instance $*"
}
ri_refuse 'is a normal server' typed beta "$work/remove-instance" beta
ri_refuse 'bad name' "$work/remove-instance" '../gamma'
ri_refuse 'shared names' "$work/remove-instance" backup --force-normal
ri_refuse 'without' typed alpha "$work/remove-instance" alpha --force-normal
grep -q 'remove that block by hand' "$work/ri.out" || fail "alpha's unmarked block: does not say to edit by hand"
ri_refuse 'on a terminal, and there is none' setsid -w "$work/remove-instance" gamma
ri_refuse "you typed 'gamm', not 'gamma'" typed gamm "$work/remove-instance" gamma
mkdir -m 0700 "$work/elsewhere-data"
ln -s "$work/elsewhere-data" "$up_data/iota"
tree >"$work/t1"
ri_refuse 'is a symlink' typed iota "$work/remove-instance" iota --force-normal
rm "$up_data/iota"
ln -s "$genv" "$etc/iota.env"
tree >"$work/t1"
ri_refuse 'is a symlink' typed iota "$work/remove-instance" iota --force-normal
rm "$etc/iota.env"
[ -d "$work/elsewhere-data" ] && [ -f "$genv" ] || fail "a refused removal followed a symlink"
rm -rf "${work:?}/elsewhere-data"
kill -0 "$(cat "$work/pids/gamma")" || fail "a refused removal stopped gamma"
ok "remove-instance: a normal server without --force-normal, a bad or shared name, alpha's unmarked Caddy block, no terminal, a wrongly typed name and a symlinked data directory or env file are refused before any change"

# The mode: temporary only when every line naming CONCHD_MODE is exactly
# CONCHD_MODE=temporary. systemd also reads an indented line, blanks around
# '=', export and quotes, so each of these is a normal server, refused
# without --force-normal; so is a continued line that may swallow the
# assignment, and a carriage return; and a quoted value running over
# several lines, inside which CONCHD_MODE=temporary is no assignment at all.
mode_cases=(
    $'CONCHD_MODE=temporary\n CONCHD_MODE=normal'
    $'CONCHD_MODE=temporary\n\tCONCHD_MODE=normal'
    $'CONCHD_MODE=temporary\nCONCHD_MODE = normal'
    $'CONCHD_MODE=temporary\nexport CONCHD_MODE=normal'
    $'CONCHD_MODE="temporary"'
    $'CONCHD_LISTEN=127.0.0.1:1 \\\nCONCHD_MODE=temporary'
    $'CONCHD_MODE=temporary\r'
    $' # CONCHD_MODE=temporary'
    $'OTHER=\'\nCONCHD_MODE=temporary\n\''
    $'OTHER="\nCONCHD_MODE=temporary\n"'
)
for m in "${mode_cases[@]}"; do
    printf '%s\n' "$m" >"$etc/omicron.env"
    tree >"$work/t1"
    ri_refuse 'is a normal server' typed omicron "$work/remove-instance" omicron
    "$work/list-instances" | grep -Eq '^omicron +\? +normal ' || fail "list-instances shows omicron as temporary for: $m"
done
printf '# a comment that names CONCHD_MODE=normal\nCONCHD_MODE=temporary\nCONCHD_MODE=temporary\n' >"$etc/omicron.env"
"$work/list-instances" | grep -Eq '^omicron +\? +temporary ' || fail "list-instances: a plain temporary env file is not temporary"
rm "$etc/omicron.env"
ok "remove-instance and list-instances: CONCHD_MODE is temporary only when every line naming it is exactly CONCHD_MODE=temporary"

# A name of which nothing is on this droplet is refused, exit 2.
tree >"$work/t1"
for n in zz ssh; do
    status=0
    typed "$n" "$work/remove-instance" "$n" --force-normal >"$work/ri.out" 2>&1 || status=$?
    [ "$status" = 2 ] && grep -q "nothing of $n is on this droplet" "$work/ri.out" || fail "remove-instance $n: exit $status: $(cat "$work/ri.out")"
    unchanged "$work/t1" "remove-instance $n"
done
ok "remove-instance: a name with nothing on the droplet is refused with exit 2"

# Removal: exactly what add-instance made goes, after a final archive, which
# waits in removed/ for the tmpfiles rule.
gpid=$(cat "$work/pids/gamma")
: >"$work/systemctl.calls"
typed gamma "$work/remove-instance" gamma >"$work/rm.out" 2>&1 || fail "remove-instance gamma failed: $(cat "$work/rm.out")"
! kill -0 "$gpid" 2>/dev/null || fail "gamma's conchd still runs"
grep -qx 'stop conchd@gamma' "$work/systemctl.calls" && grep -qx 'disable conchd@gamma' "$work/systemctl.calls" &&
    grep -qx 'stop conch-backup@gamma.timer' "$work/systemctl.calls" && grep -qx 'reload caddy' "$work/systemctl.calls" &&
    grep -qx 'restart livekit' "$work/systemctl.calls" || fail "removal: systemctl calls: $(cat "$work/systemctl.calls")"
# (sha256sum's lines start with the hash, so the paths are matched anywhere.)
d=$(diff <(grep -vF -e "$work/upbackups/removed" -e "$work/tmpfiles/conch-removed.conf" "$work/t0") \
    <(tree | grep -vF -e "$work/upbackups/removed" -e "$work/tmpfiles/conch-removed.conf") || true)
[ -z "$d" ] || fail "removal did not leave the droplet as it was before gamma was added:
$d"
cmp -s "$caddyfile" "$work/Caddyfile.as-deployed" || fail "the Caddyfile after removal differs from before the add"
final=$(ls "$work"/upbackups/removed/gamma-*/conch-gamma-*.tar.gz)
tar -tzf "$final" | grep -x conch.db >/dev/null || fail "the final archive holds no conch.db"
[ ! -e "$work/shared-user-touched" ] || fail "userdel was asked for a shared user"
grep -q 'A and AAAA for gamma.example.test' "$work/rm.out" || fail "removal does not print the DNS records to delete: $(cat "$work/rm.out")"
ok "remove-instance gamma: typed confirmation, stopped, final archive moved to removed/, and exactly what add-instance made is gone (the Caddyfile byte for byte)"

# Two instances through an upgrade and a rollback: delta, a normal server
# with password-only operators and a backup timer, beside beta.
dport=$((gport + 1))
: >"$work/systemctl.calls"
printf '8 A delta 203.0.113.7\n9 AAAA delta 2001:db8::7\n' >>"$work/dns-records"
# The conchd the instance's steps run (the bootstrap) must not hold the
# descriptor of the lock on the env directory: this one refuses to run if it does.
# (Swapped by rename: beta runs from that file, which cannot be written.)
lock_release=$(readlink -f "$opt/current")
mv "$lock_release/conchd" "$work/conchd-unwrapped"
# shellcheck disable=SC2016 # expanded by the wrapper
printf '#!/bin/sh\nif [ -e /proc/$$/fd/9 ]; then echo "conchd: started holding the lock descriptor 9" >&2; exit 9; fi\nexec %s "$@"\n' \
    "'$work/conchd-unwrapped'" >"$work/conchd-wrapper"
chmod 0755 "$work/conchd-wrapper"
mv "$work/conchd-wrapper" "$lock_release/conchd"
status=0
onpty "$work/add-instance" delta example.test --operator-totp optional --port "$dport" --yes >"$work/add-delta.out" 2>&1 || status=$?
mv -f "$work/conchd-unwrapped" "$lock_release/conchd"
[ "$status" = 0 ] || fail "add-instance delta failed: $(cat "$work/add-delta.out")"
if grep -q 'holding the lock descriptor' "$work/add-delta.out"; then fail "the bootstrap ran holding the lock's descriptor"; fi
grep -qx 'enable conch-backup@delta.timer' "$work/systemctl.calls" || fail "a normal server got no backup timer"
if grep -q '^CONCHD_MODE=' "$etc/delta.env"; then fail "a normal server's env file sets CONCHD_MODE"; fi
dversion() { curl -fsS "http://127.0.0.1:$dport/healthz" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p'; }
up v0.0.2 beta delta >"$work/up-two.out" 2>&1 || fail "the upgrade of beta and delta failed: $(cat "$work/up-two.out")"
[ "$(version)" = v0.0.2 ] && [ "$(dversion)" = v0.0.2 ] || fail "after the upgrade of two: beta $(version), delta $(dversion)"
up --rollback beta delta >"$work/rb-two.out" 2>&1 || fail "the rollback of beta and delta failed: $(cat "$work/rb-two.out")"
[ "$(version)" = v0.0.1 ] && [ "$(dversion)" = v0.0.1 ] || fail "after the rollback of two: beta $(version), delta $(dversion)"
ok "conch-upgrade: two instances, one made by add-instance, upgraded and rolled back together"
CONCH_DATA_ROOT=$up_data CONCH_BACKUP_DIR=$work/upbackups "$deploy/conch-backup.sh" delta --local-only >/dev/null 2>&1 ||
    fail "a backup of delta failed"
typed delta "$work/remove-instance" delta >"$work/rm-delta-normal.out" 2>&1 && fail "delta, a normal server, was removed without --force-normal"
typed delta "$work/remove-instance" delta --force-normal --purge-backups >"$work/rm-delta.out" 2>&1 ||
    fail "remove-instance delta failed: $(cat "$work/rm-delta.out")"
[ ! -e "$work/upbackups/delta" ] && [ ! -e "$opt/state/delta" ] && [ ! -e "$etc/delta.env" ] && [ ! -e "$up_data/delta" ] ||
    fail "delta's backups, rollback record, env file or data were left"
for t in "$work"/upbackups/removed/delta-*; do
    [ ! -e "$t" ] || fail "--purge-backups moved delta's archives instead of deleting them"
done
grep -qx 'stop conch-backup@delta.timer' "$work/systemctl.calls" || fail "delta's timer was not stopped"
ok "remove-instance delta --force-normal --purge-backups: a normal server with a timer, a rollback record and pre-upgrade archives is gone entirely"

# Interrupted once conchd answers (here while the check through Caddy is
# still failing): the server is kept, running, exit 4.
printf '14 A rho 203.0.113.7\n15 AAAA rho 2001:db8::7\n' >>"$work/dns-records"
touch "$work/fail-public"
: >"$work/public-curl.calls"
onpty env CONCH_HEALTH_TIMEOUT=60 "$work/add-instance" rho example.test --temporary --port "$((gport + 3))" --yes >"$work/rho.out" 2>&1 &
rpid=$!
for _ in $(seq 1 150); do
    if grep -q 'rho.example.test' "$work/public-curl.calls"; then break; fi
    sleep 0.2
done
grep -q 'rho.example.test' "$work/public-curl.calls" || fail "add-instance rho never reached the check through Caddy: $(cat "$work/rho.out")"
pkill -TERM -f "^bash $deploy/add-instance.sh rho " || fail "no add-instance rho to interrupt"
status=0
wait "$rpid" || status=$?
rm -f "$work/fail-public"
[ "$status" = 4 ] && grep -q 'the server is kept and runs' "$work/rho.out" || fail "an interrupted add after commit: exit $status, want 4: $(cat "$work/rho.out")"
[ -f "$etc/rho.env" ] && kill -0 "$(cat "$work/pids/rho")" || fail "an interruption after commit undid rho"
typed rho "$work/remove-instance" rho >"$work/rm-rho.out" 2>&1 || fail "removing rho failed: $(cat "$work/rm-rho.out")"
sed -i '/ rho /d' "$work/dns-records"
ok "add-instance: interrupted after conchd answered, the server is kept and it exits 4"

# --- conch-server, against an ssh that runs the droplet's side here ---------
#
# A checkout with ops/deploy at the tag the "droplet" runs (v0.0.1), the
# droplet's /opt/conch/bin empty, a doctl that keeps the DNS records above,
# and DNS-over-HTTPS answered from them. The ssh stand-in runs the remote
# command with bash, as sshd hands it to root's shell, so the quoting is
# exercised for real. A doctl or ssh found on PATH instead of the stand-ins
# fails the test: the real ones are never run.

repo=$work/repo
mkdir -p "$repo/ops/deploy" "$work/remote-bin" "$sb/never"
cp "$deploy"/*.sh "$repo/ops/deploy/"
# A fixture repository of its own, untouched by global signing or hooks.
fixture_git() { git -C "$repo" -c user.name=test -c user.email=test@example.test -c commit.gpgsign=false \
    -c tag.gpgsign=false -c core.hooksPath=/dev/null "$@"; }
fixture_git init -q
fixture_git add ops
fixture_git commit -q --no-verify -m fixture
fixture_git tag v0.0.1
for t in ssh doctl; do
    printf '#!/bin/sh\ntouch %q\nexit 99\n' "$work/real-$t-called" >"$sb/never/$t"
done
cat >"$sb/ssh" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for ssh: logs its arguments and runs the remote command here, with
# the droplet's locations
w=$CONCH_TEST_WORK
printf '%s\n' "$*" >>"$w/ssh.calls"
while [ $# -gt 0 ]; do
    case "$1" in
        -t | -T) shift ;;
        -i | -o) shift 2 ;;
        --) shift; break ;;
        *) echo "stand-in ssh: unexpected $1" >&2; exit 255 ;;
    esac
done
[ "$1" = root@203.0.113.7 ] || exit 255
shift
. "$w/droplet.env"
exec bash -c "$*"
STANDIN
cat >"$sb/doctl" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for doctl on the zone example.test, keeping the test's DNS records
w=$CONCH_TEST_WORK
printf '%s\n' "$*" >>"$w/doctl.calls"
[ "$1 $2" = "--context conch" ] && [ "$3 $4 $5" = "compute domain records" ] && [ "$7" = example.test ] || exit 9
verb=$6
shift 7
case "$verb" in
    list) [ "$*" = "--format ID,Type,Name,Data --no-header" ] || exit 9; cat "$w/dns-records" ;;
    create)
        while [ $# -gt 0 ]; do
            case "$1" in --record-type) t=$2; shift ;; --record-name) n=$2; shift ;; --record-data) d=$2; shift ;; esac
            shift
        done
        id=$((100 + $(wc -l <"$w/dns-records")))
        echo "$id $t $n $d" >>"$w/dns-records"
        echo "$id" ;;
    delete) [ "$2" = --force ] && grep -q "^$1 " "$w/dns-records" || exit 9; sed -i "/^$1 /d" "$w/dns-records" ;;
    *) exit 9 ;;
esac
STANDIN
cat >"$sb/doh" <<'STANDIN'
#!/usr/bin/env bash
# stand-in for curl's DNS-over-HTTPS queries, answered from the test's DNS records
w=$CONCH_TEST_WORK
printf '%s\n' "$*" >>"$w/doh.calls"
q=${!#}
q=${q#*\?name=}
name=${q%%&*}
t=${q##*type=}
awk -v t="$t" -v n="${name%.example.test}" -v q="$name" '
    BEGIN { c = (t == "A") ? 1 : 28; printf "{\"Status\":0,\"Answer\":[" }
    $2 == t && $3 == n { printf "%s{\"name\":\"%s.\",\"type\":%d,\"TTL\":300,\"data\":\"%s\"}", (k++ ? "," : ""), q, c, $4 }
    END { print "]}" }' "$w/dns-records"
STANDIN
chmod 0755 "$sb"/*
touch "$work/id_test"
cat >"$work/server.env" <<EOF
# conch-server settings for the test
CONCH_SERVER_HOST=root@203.0.113.7
CONCH_SERVER_DOMAIN=example.test
CONCH_SERVER_SSH_KEY=$work/id_test
CONCH_SERVER_DOCTL_CONTEXT=conch
CONCH_SERVER_OPERATOR=Nick O'Brien
EOF
chmod 0600 "$work/server.env"
# $work/cs: conch-server with the stand-ins, as one command (so it can run
# under script(1)); CS_CONFIG names other settings.
cat >"$work/cs" <<EOF
#!/usr/bin/env bash
export CONCH_SERVER_CONFIG=\${CS_CONFIG:-$work/server.env} SSH=$sb/ssh DOCTL=$sb/doctl CURL=$sb/doh CONCH_SERVER_REPO=$repo
export CONCH_SERVER_REMOTE_OPT=$(readlink -f "$opt") CONCH_SERVER_REMOTE_BIN=$work/remote-bin
export CONCH_SERVER_DOH='https://one.test/dns-query https://two.test/resolve' CONCH_SERVER_DNS_TIMEOUT=5 CONCH_SERVER_DNS_INTERVAL=0
export PATH=$sb/never:\$PATH
exec $(printf %q "$PWD/ops/conch-server") "\$@"
EOF
chmod 0755 "$work/cs"
cs() { CS_CONFIG=${CS_CONFIG:-} "$work/cs" "$@"; }

# Settings that are not exactly what they should be are refused before
# anything runs.
# shellcheck disable=SC2016 # the payloads are literal
for c in host domain extra twice loose; do
    cp "$work/server.env" "$work/bad.env"
    case $c in
        host) sed -i 's/^CONCH_SERVER_HOST=.*/CONCH_SERVER_HOST=root@203.0.113.7;touch x/' "$work/bad.env" ;;
        domain) sed -i 's/^CONCH_SERVER_DOMAIN=.*/CONCH_SERVER_DOMAIN=$(id).test/' "$work/bad.env" ;;
        extra) echo 'PATH=/tmp' >>"$work/bad.env" ;;
        twice) echo 'CONCH_SERVER_DOMAIN=example.org' >>"$work/bad.env" ;;
        loose) chmod 0620 "$work/bad.env" ;;
    esac
    [ "$c" = loose ] || chmod 0600 "$work/bad.env"
    if CS_CONFIG=$work/bad.env cs list >"$work/cs-bad.out" 2>&1; then fail "conch-server took settings with a bad $c"; fi
    [ ! -e "$work/ssh.calls" ] && [ ! -e "$work/doctl.calls" ] || fail "conch-server ran ssh or doctl with a bad $c: $(cat "$work/cs-bad.out")"
done
# shellcheck disable=SC2016 # the payload is literal
if cs new 'kappa;id' >"$work/cs-bad.out" 2>&1 || cs new kappa --operator '$(id)' >>"$work/cs-bad.out" 2>&1; then
    fail "conch-server new took a bad name or operator"
fi
[ ! -e "$work/ssh.calls" ] || fail "conch-server new ran ssh with a bad name or operator"
ok "conch-server: bad settings (a host or domain with shell characters, an unknown or repeated key, a file others can write), a bad name or operator are refused before ssh or doctl run"

# The settings example in docs/deploy.md, verbatim, is accepted.
awk '/Its settings are in `~\/.config\/conch\/server.env`/ { f = 1; next }
    f && /^```sh$/ { c = 1; next }
    c && /^```$/ { exit }
    c { print }' docs/deploy.md >"$work/doc-server.env"
grep -qx 'CONCH_SERVER_OPERATOR=your-name' "$work/doc-server.env" || fail "the settings example was not found in docs/deploy.md: $(cat "$work/doc-server.env")"
chmod 0600 "$work/doc-server.env"
mkdir -p "$work/home/.ssh"
touch "$work/home/.ssh/conch-alpha"
HOME=$work/home CS_CONFIG=$work/doc-server.env cs sync >"$work/cs-doc.out" 2>&1 ||
    fail "conch-server refuses the settings example in docs/deploy.md: $(cat "$work/cs-doc.out")"
rm -f "$work/ssh.calls"
ok "conch-server: the settings example in docs/deploy.md parses as it is written"

# new: scripts copied, DNS made and waited for, add-instance run with every
# argument quoted, the setup link on this terminal only.
kport=$((gport + 2))
onpty "$work/cs" new kappa --temporary --port "$kport" --yes >"$work/cs-new.out" 2>&1 || fail "conch-server new kappa failed: $(cat "$work/cs-new.out")"
for s in add-instance.sh remove-instance.sh list-instances.sh conch-backup.sh conch-upgrade.sh; do
    cmp -s "$deploy/$s" "$work/remote-bin/$s" && [ "$(stat -c %a "$work/remote-bin/$s")" = 755 ] ||
        fail "conch-server new did not install $s 0755 from the tag"
done
grep -qx 'kappa' <(awk '$2 == "A" && $4 == "203.0.113.7" { print $3 }' "$work/dns-records") &&
    grep -qx 'kappa' <(awk '$2 == "AAAA" { print $3 }' "$work/dns-records") || fail "no A and AAAA records for kappa: $(cat "$work/dns-records")"
grep -qF "add-instance.sh kappa example.test --temporary --operator Nick\\ O\\'Brien --port $kport --yes" "$work/ssh.calls" ||
    fail "the remote command is not quoted as expected: $(cat "$work/ssh.calls")"
grep -q "^-t .*add-instance.sh" "$work/ssh.calls" || fail "add-instance did not run with ssh -t"
grep -q "setup link for operator \"Nick O'Brien\"" "$work/cs-new.out" || fail "the operator's name did not reach conchd whole: $(cat "$work/cs-new.out")"
[ "$(grep -c '^https://kappa.example.test/app/#/setup/' "$work/cs-new.out")" = 1 ] || fail "conch-server new: the setup link is not shown once"
klink=$(grep '^https://kappa.example.test/app/#/setup/' "$work/cs-new.out")
ksecret=$(sed -n 's/^CONCHD_LIVEKIT_API_SECRET=//p' "$etc/kappa.env")
ktotp=$(sed -n 's/^CONCHD_TOTP_KEY=//p' "$etc/kappa.env")
for s in "$klink" "$ksecret" "$ktotp"; do
    if grep -qF -- "$s" "$work/ssh.calls" "$work/doctl.calls" "$work/doh.calls"; then fail "a secret is in the ssh, doctl or DNS logs"; fi
done
if grep -qF -- "$ksecret" "$work/cs-new.out" || grep -qF -- "$ktotp" "$work/cs-new.out"; then fail "conch-server new printed a secret"; fi
cs sync >"$work/cs-sync.out" 2>&1 || fail "conch-server sync failed: $(cat "$work/cs-sync.out")"
if grep -q installed "$work/cs-sync.out"; then fail "conch-server sync copied scripts that already match"; fi
ok "conch-server new kappa: scripts installed from the tag, A and AAAA made and seen, add-instance run quoted over ssh -t, the setup link shown once and no secret logged"

# list and upgrade: every instance, all named.
cs list >"$work/cs-list.out" 2>&1 || fail "conch-server list failed: $(cat "$work/cs-list.out")"
grep -Eq '^kappa +[0-9]+ +temporary +optional +active +ok v0\.0\.1$' "$work/cs-list.out" &&
    grep -Eq '^beta .* ok v0\.0\.1$' "$work/cs-list.out" || fail "conch-server list: $(cat "$work/cs-list.out")"
if cs upgrade v0.0.9 >"$work/cs-up.out" 2>&1; then fail "an upgrade to a release that fails its checksum went ahead"; fi
grep -qF 'conch-upgrade.sh v0.0.9 alpha beta kappa' "$work/ssh.calls" || fail "conch-server upgrade did not name every instance: $(tail -n 2 "$work/ssh.calls")"
grep -q 'does not match checksums.txt' "$work/cs-up.out" && [ "$(version)" = v0.0.1 ] || fail "conch-server upgrade v0.0.9: $(cat "$work/cs-up.out")"
ok "conch-server list shows every instance with its mode and health; upgrade names every instance"

# With its output not on a terminal, new is refused before ssh or doctl run:
# ssh -t would give the droplet a terminal anyway, and the setup link would
# land in the file.
ssh_before=$(wc -l <"$work/ssh.calls") doctl_before=$(wc -l <"$work/doctl.calls")
status=0
cs new nu --temporary --yes >"$work/cs-notty.out" 2>&1 || status=$?
[ "$status" = 2 ] && grep -q 'printed to a terminal only' "$work/cs-notty.out" || fail "conch-server new to a file: exit $status: $(cat "$work/cs-notty.out")"
[ "$(wc -l <"$work/ssh.calls")" = "$ssh_before" ] && [ "$(wc -l <"$work/doctl.calls")" = "$doctl_before" ] ||
    fail "conch-server new to a file ran ssh or doctl"
ok "conch-server new: with its output on a file, it refuses before ssh or doctl runs"

# A name with a record pointing elsewhere is refused before anything is
# made; an add-instance that refuses has its new records deleted again.
printf '50 A lambda 198.51.100.9\n' >>"$work/dns-records"
doctl_before=$(wc -l <"$work/doctl.calls")
if onpty "$work/cs" new lambda --temporary --yes >"$work/cs-lambda.out" 2>&1; then fail "conch-server new lambda went ahead"; fi
grep -q 'not this droplet' "$work/cs-lambda.out" || fail "conch-server new lambda: $(cat "$work/cs-lambda.out")"
if tail -n +"$((doctl_before + 1))" "$work/doctl.calls" | grep -q create; then fail "conch-server new lambda created a record"; fi
sed -i '/ lambda /d' "$work/dns-records"
cp "$work/dns-records" "$work/dns-before-mu"
if onpty "$work/cs" new mu --temporary --port 8081 --yes >"$work/cs-mu.out" 2>&1; then fail "conch-server new mu went ahead on a taken port"; fi
grep -q 'port 8081 is taken' "$work/cs-mu.out" && grep -q 'deleted the record' "$work/cs-mu.out" || fail "conch-server new mu: $(cat "$work/cs-mu.out")"
cmp -s "$work/dns-records" "$work/dns-before-mu" || fail "the records conch-server new mu made were not deleted"
ok "conch-server new: a record pointing elsewhere is refused before any change; records made for a refused add are deleted"

# An add interrupted after it committed keeps the server, and conch-server
# keeps its DNS records.
touch "$work/fail-public"
: >"$work/public-curl.calls"
onpty "$work/cs" new xi --temporary --port "$((kport + 1))" --yes >"$work/cs-xi.out" 2>&1 &
xpid=$!
for _ in $(seq 1 150); do
    if grep -q 'xi.example.test' "$work/public-curl.calls"; then break; fi
    sleep 0.2
done
pkill -TERM -f "^bash $work/remote-bin/add-instance.sh xi " || fail "no add-instance xi to interrupt: $(cat "$work/cs-xi.out")"
status=0
wait "$xpid" || status=$?
rm -f "$work/fail-public"
[ "$status" = 4 ] && grep -q 'The DNS records are kept' "$work/cs-xi.out" || fail "conch-server new xi, interrupted: exit $status: $(cat "$work/cs-xi.out")"
[ "$(awk '$3 == "xi"' "$work/dns-records" | wc -l)" = 2 ] && kill -0 "$(cat "$work/pids/xi")" ||
    fail "conch-server deleted the records of a server it kept, or the server is gone"
typed xi "$work/cs" rm xi >"$work/cs-rm-xi.out" 2>&1 || fail "conch-server rm xi failed: $(cat "$work/cs-rm-xi.out")"
ok "conch-server new: an add interrupted after it committed keeps the server and its DNS records (exit 4)"

# rm of a name the droplet does not list: refused, unless --force-normal, and
# then remove-instance refuses it (nothing there) and the records stay.
printf '60 A nu 203.0.113.7\n' >>"$work/dns-records"
if cs rm nu >"$work/cs-nu.out" 2>&1; then fail "conch-server rm nu went ahead"; fi
grep -q 'lists no server named nu' "$work/cs-nu.out" || fail "conch-server rm nu: $(cat "$work/cs-nu.out")"
if grep -q 'remove-instance.sh nu' "$work/ssh.calls"; then fail "conch-server rm nu ran remove-instance"; fi
status=0
typed nu "$work/cs" rm nu --force-normal >"$work/cs-nu2.out" 2>&1 || status=$?
[ "$status" = 2 ] && grep -q 'nothing of nu is on this droplet' "$work/cs-nu2.out" || fail "conch-server rm nu --force-normal: exit $status: $(cat "$work/cs-nu2.out")"
grep -q '^60 A nu ' "$work/dns-records" || fail "conch-server rm deleted the records of a name with nothing on the droplet"
sed -i '/ nu /d' "$work/dns-records"
ok "conch-server rm: a name the droplet does not list is refused; with --force-normal, nothing there is refused too, and the records stay"

# rm: typed on the droplet's terminal, then the DNS records go.
typed kappa "$work/cs" rm kappa >"$work/cs-rm.out" 2>&1 || fail "conch-server rm kappa failed: $(cat "$work/cs-rm.out")"
grep -q "^-t .*remove-instance.sh kappa$" "$work/ssh.calls" || fail "remove-instance did not run with ssh -t: $(tail -n 3 "$work/ssh.calls")"
if grep -q ' kappa ' "$work/dns-records"; then fail "kappa's DNS records were not deleted: $(cat "$work/dns-records")"; fi
[ ! -e "$etc/kappa.env" ] && ! grep -q '^conch-kappa:' "$work/passwd" || fail "conch-server rm left kappa on the droplet"
for s in "$ksecret" "$ktotp" "$klink"; do
    if grep -qF -- "$s" "$work/ssh.calls" "$work/doctl.calls" "$work/cs-rm.out"; then fail "a secret is in the logs after rm"; fi
done
[ ! -e "$work/real-ssh-called" ] && [ ! -e "$work/real-doctl-called" ] || fail "a real ssh or doctl was run"
ok "conch-server rm kappa: removed on the droplet after the typed name, then its A and AAAA records deleted"

echo "deploy-scripts-test: PASS"
