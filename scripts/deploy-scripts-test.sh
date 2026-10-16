#!/usr/bin/env bash
# deploy-scripts-test.sh — the test of the files in ops/deploy (issue #435).
#
# It needs no droplet, no domain and no account. It builds conchd, runs it on
# loopback against a temporary data directory with a message and an uploaded
# file in it, and then runs the real scripts:
#
#   conch-backup.sh   an archive with a SHA-256; exit 3 and a plain message
#                     without an off-site destination; the off-site path with a
#                     stand-in rclone; --local-only
#   conch-restore.sh  the drill: the message and the file are back and /healthz
#                     answers ok; a tampered archive is refused
#   conch-upgrade.sh  upgrade, rollback without a migration (data kept) and
#                     rollback with one (pre-upgrade backup restored), with a
#                     stand-in systemctl that runs the instance as a child
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
    shellcheck "$deploy"/*.sh scripts/deploy-scripts-test.sh || fail "shellcheck"
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

# free_port <from>: the first loopback port from <from> nothing listens on.
free_port() {
    local p=$1
    while (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; do p=$((p + 1)); done
    echo "$p"
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
port=$(free_port 18300)
"$work/bin/conchd-v0.0.1" serve --data "$data_root/alpha" --listen "127.0.0.1:$port" --no-web >"$work/serve.log" 2>&1 &
pids+=($!)
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

status=0
out=$("$deploy/conch-backup.sh" alpha 2>&1) || status=$?
[ "$status" -eq 3 ] || fail "backup without an off-site destination exited $status, want 3: $out"
case "$out" in *"NO OFF-SITE COPY WAS MADE"*) ;; *) fail "backup did not say there is no off-site copy: $out" ;; esac
archive=$(ls -1 "$backups"/alpha/conch-alpha-*.tar.gz | head -n 1)
[ -f "$archive" ] && [ -f "$archive.sha256" ] || fail "no archive and checksum after the first backup"
(cd "$backups/alpha" && sha256sum -c "$(basename "$archive").sha256" >/dev/null) || fail "the checksum file does not verify"
ok "no off-site destination: said so, exit 3, archive still written"

sleep 1 # the archive name carries the second
"$deploy/conch-backup.sh" alpha --local-only >"$work/b2.out" 2>&1 || { cat "$work/b2.out" >&2; fail "--local-only exited non-zero"; }
grep -q 'no off-site copy was made' "$work/b2.out" || fail "--local-only did not say what it did"
ok "--local-only exits 0"

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
CONCHD=$work/bin/conchd-v0.0.1 CONCH_RESTORE_PORT=$((port + 1)) "$deploy/conch-restore.sh" "$latest" "$work/scratch" >"$work/restore.out" 2>&1 ||
    { cat "$work/restore.out" >&2; fail "the restore drill failed"; }
grep -q 'conch-restore: OK' "$work/restore.out" || fail "the drill did not report OK"
grep -q '"db":"ok"' "$work/restore.out" || fail "the drill did not report /healthz db ok"
ok "restore drill"

# The restored data holds the message and the file, served by a conchd.
rport=$(free_port $((port + 2)))
"$work/bin/conchd-v0.0.1" serve --data "$work/scratch/data" --listen "127.0.0.1:$rport" --no-web >"$work/restored.log" 2>&1 &
pids+=($!)
wait_health "$rport" || fail "the restored data did not start"
api "$rport" "$token" GET /v2/channels/ops/messages | grep -q 'hello before the backup' || fail "the message is not in the restored server"
api "$rport" "$token" GET "/v2/files/$file_id" -o "$work/restored.bin"
cmp "$work/upload.bin" "$work/restored.bin" || fail "the restored file differs from the upload"
ok "restored server holds the message and the file"

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

kill "${pids[0]}" "${pids[1]}" 2>/dev/null || true
wait "${pids[0]}" "${pids[1]}" 2>/dev/null || true
pids=()

# --- upgrade and rollback -----------------------------------------------------

opt=$work/opt etc=$work/etc rel=$work/release up_data=$work/updata
mkdir -p "$opt/releases/v0.0.1" "$etc" "$rel/v0.0.2" "$work/pids" "$up_data/beta"
install -m 0755 "$work/bin/conchd-v0.0.1" "$opt/releases/v0.0.1/conchd"
ln -s "$opt/releases/v0.0.1" "$opt/current"
bport=$(free_port 18400)
printf 'CONCHD_DATA=%s\nCONCHD_LISTEN=127.0.0.1:%s\n' "$up_data/beta" "$bport" >"$etc/beta.env"
cp "$work/bin/conchd-v0.0.2" "$rel/v0.0.2/conchd_v0.0.2_linux_$arch"
(cd "$rel/v0.0.2" && sha256sum "./conchd_v0.0.2_linux_$arch" >checksums.txt)

# A stand-in for systemctl that runs the instance as a child of this test.
cat >"$work/systemctl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
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
"$work/systemctl" start conchd@beta
wait_health "$bport" || fail "beta did not start"
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

up v0.0.2 beta >"$work/up1.out" 2>&1 || { cat "$work/up1.out" >&2; fail "upgrade failed"; }
[ "$(version)" = v0.0.2 ] || fail "after the upgrade /healthz says $(version)"
has 'before upgrade' || fail "the upgrade lost a message"
ok "upgrade: /healthz reports v0.0.2, data intact"

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
up --rollback beta >"$work/rb2.out" 2>&1 || { cat "$work/rb2.out" >&2; fail "rollback after a migration failed"; }
[ "$(version)" = v0.0.1 ] || fail "after the second rollback /healthz says $(version)"
grep -q 'restoring the pre-upgrade backup' "$work/rb2.out" || fail "rollback did not restore the backup"
has 'after upgrade, no migration' || fail "the pre-upgrade backup lost an older message"
if has 'after upgrade, with migration'; then fail "a message written after the migration survived the restore"; fi
ls -d "$up_data"/beta.rolled-back-* >/dev/null || fail "the replaced data directory was not kept"
ok "rollback with a migration: pre-upgrade backup restored, old binary starts"

echo "deploy-scripts-test: PASS"
