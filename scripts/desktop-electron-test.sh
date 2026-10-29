#!/usr/bin/env bash
# Tests of scripts/desktop-electron.sh, which decides whether the Electron the
# desktop shell ships with is the one that was pinned. A check that cannot fail
# is not a check, so each refusal is shown. A fixture zip stands in for the
# download: no network, no Node, no real Electron. Run by `make desktop-check`.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
    echo "desktop-electron-test: FAIL: $*"
    exit 1
}

# A stand-in release: a zip with an executable called electron.
mkdir "$tmp/zipsrc"
printf '#!/bin/sh\necho fixture\n' >"$tmp/zipsrc/electron"
chmod +x "$tmp/zipsrc/electron"
# The real zip carries 0777 on some files; so does this one.
printf 'library\n' >"$tmp/zipsrc/libffmpeg.so"
printf 'sandbox\n' >"$tmp/zipsrc/chrome-sandbox"
chmod 777 "$tmp/zipsrc/libffmpeg.so" "$tmp/zipsrc/chrome-sandbox"
(cd "$tmp/zipsrc" && zip -q "$tmp/electron-v1.2.3-linux-x64.zip" electron libffmpeg.so chrome-sandbox)
good=$(sha256sum "$tmp/electron-v1.2.3-linux-x64.zip" | cut -d' ' -f1)
bad=$(printf 'x' | sha256sum | cut -d' ' -f1)

mkdir -p "$tmp/desktop/node_modules/electron"
echo '{ "devDependencies": { "electron": "1.2.3" } }' >"$tmp/desktop/package.json"

pins() { # pins <sha256>
    printf 'CONCH_ELECTRON_VERSION=1.2.3\nCONCH_ELECTRON_LINUX_X64_SHA256=%s\n' "$1" >"$tmp/pins.sh"
}
sums() { # sums <sha256>
    printf '{ "electron-v1.2.3-linux-x64.zip": "%s" }\n' "$1" >"$tmp/desktop/node_modules/electron/checksums.json"
}

export CONCH_DESKTOP_DIR="$tmp/desktop"
export CONCH_DESKTOP_PINS="$tmp/pins.sh"
export CONCH_ELECTRON_URL="file://$tmp/electron-v1.2.3-linux-x64.zip"

# run <cache dir>: output and status into $out and $status.
run() {
    status=0
    out=$(CONCH_CACHE="$1" ./scripts/desktop-electron.sh "$1.dest" 2>&1) || status=$?
}

# refuses <name> <cache>: failed, said so on one FAIL line, left no download or
# unpacked directory behind.
refuses() {
    local name=$1 cache=$2 lines
    [ "$status" -ne 0 ] || fail "$name: succeeded, want a refusal: $out"
    lines=$(grep -c '^desktop-electron: FAIL:' <<<"$out" || true)
    [ "$lines" = 1 ] || fail "$name: want one FAIL line, got $lines: $out"
    [ ! -e "$cache.dest" ] || fail "$name: left $cache.dest behind"
    ! compgen -G "$cache/electron/.electron-fetch.*" >/dev/null || fail "$name: left a download behind"
}

# The good case, then the second run.
pins "$good"
sums "$good"
run "$tmp/c1"
[ "$status" -eq 0 ] || fail "good zip refused: $out"
[ -x "$tmp/c1.dest/electron" ] || fail "good zip was not unpacked"
run "$tmp/c1"
[ "$status" -eq 0 ] || fail "second run failed: $out"
! grep -q 'downloading' <<<"$out" || fail "second run downloaded again: $out"

# Permissions: the cache is private, the zip is 0600, and nothing unpacked is
# writable by group or other, whatever the zip said (it said 0777).
[ "$(stat -c %a "$tmp/c1/electron")" = 700 ] || fail "a new cache is mode $(stat -c %a "$tmp/c1/electron"), want 700"
[ "$(stat -c %a "$tmp/c1/electron/electron-v1.2.3-linux-x64.zip")" = 600 ] || fail "the cached zip is not mode 600"
[ -z "$(find "$tmp/c1.dest" -perm /022)" ] || fail "unpacked files are group or other writable: $(find "$tmp/c1.dest" -perm /022)"
[ -x "$tmp/c1.dest/electron" ] || fail "the executable lost its execute bit"

# A cache hit trusts nothing it holds. Corrupt the unpacked executable and a
# library and leave the marker alone: the next run unpacks them afresh from the
# verified zip, and what is there afterwards is what the zip holds.
echo 'tampered' >"$tmp/c1.dest/electron"
echo 'tampered' >"$tmp/c1.dest/libffmpeg.so"
run "$tmp/c1"
[ "$status" -eq 0 ] || fail "run after tampering failed: $out"
cmp -s "$tmp/zipsrc/electron" "$tmp/c1.dest/electron" || fail "a tampered executable survived a run"
cmp -s "$tmp/zipsrc/libffmpeg.so" "$tmp/c1.dest/libffmpeg.so" || fail "a tampered library survived a run"

# The cached zip itself is checked on every run: tampered, it is refused and
# removed, and nothing is unpacked from it.
echo 'tampered' >>"$tmp/c1/electron/electron-v1.2.3-linux-x64.zip"
rm -rf "$tmp/c1.dest"
run "$tmp/c1"
[ "$status" -ne 0 ] || fail "a tampered cached zip was accepted"
[ "$(grep -c '^desktop-electron: FAIL:' <<<"$out")" = 1 ] || fail "want one FAIL line for a tampered cached zip: $out"
[ ! -e "$tmp/c1.dest" ] || fail "something was unpacked from a tampered zip"
[ ! -e "$tmp/c1/electron/electron-v1.2.3-linux-x64.zip" ] || fail "the tampered zip was kept"
run "$tmp/c1"
[ "$status" -eq 0 ] || fail "the run after the refusal did not fetch afresh: $out"

# A cache that group or other can write to is refused, with one FAIL line.
mkdir -p "$tmp/c1w" && mkdir -m 775 "$tmp/c1w/electron"
run "$tmp/c1w"
[ "$status" -ne 0 ] || fail "a group-writable cache was accepted"
[ "$(grep -c '^desktop-electron: FAIL:' <<<"$out")" = 1 ] || fail "want one FAIL line for a writable cache: $out"
[ ! -e "$tmp/c1w.dest" ] || fail "something was unpacked into a writable cache"
mkdir -p "$tmp/c1link" "$tmp/c1t" && chmod 700 "$tmp/c1t"
ln -s "$tmp/c1t" "$tmp/c1link/electron"
run "$tmp/c1link"
[ "$status" -ne 0 ] || fail "a symlinked cache was accepted"

# No extracted tree is shared: the cache holds the zip only.
[ -z "$(find "$tmp/c1/electron" -mindepth 1 -type d ! -name '.electron-fetch.*')" ] || fail "the shared cache holds an unpacked directory: $(find "$tmp/c1/electron" -mindepth 1 -type d)"

# Two builds at once, each into its own directory, against a cold cache and
# then a warm one: both must succeed and end with a complete tree.
for cache in "$tmp/cc1" "$tmp/c1"; do
    for i in a b c; do
        (CONCH_CACHE="$cache" ./scripts/desktop-electron.sh "$tmp/par-$i" >"$tmp/par-$i.log" 2>&1; echo $? >"$tmp/par-$i.status") &
    done
    wait
    for i in a b c; do
        [ "$(cat "$tmp/par-$i.status")" = 0 ] || fail "concurrent run $i failed: $(cat "$tmp/par-$i.log")"
        for f in electron libffmpeg.so chrome-sandbox; do
            cmp -s "$tmp/zipsrc/$f" "$tmp/par-$i/$f" || fail "concurrent run $i has an incomplete tree ($f)"
        done
    done
done

# A download directory left by a run that was killed hard is removed.
mkdir "$tmp/c1/electron/.electron-fetch.stale"
run "$tmp/c1"
[ "$status" -eq 0 ] || fail "run beside a stale download directory failed: $out"
[ ! -e "$tmp/c1/electron/.electron-fetch.stale" ] || fail "a stale download directory survived"

# A zip that differs from the pin (and so from nothing else either).
pins "$bad"
sums "$bad"
run "$tmp/c2"
refuses "zip differs from both" "$tmp/c2"

# Differs from the pin only.
pins "$bad"
sums "$good"
run "$tmp/c3"
refuses "zip differs from the pin" "$tmp/c3"

# The pin and checksums.json disagree: refused before any download, even
# though the zip matches one of them.
pins "$good"
sums "$bad"
run "$tmp/c4"
refuses "pin and checksums.json disagree" "$tmp/c4"
! grep -q 'downloading' <<<"$out" || fail "disagreeing hashes were not caught before the download"

# The pin file has no hash.
printf 'CONCH_ELECTRON_VERSION=1.2.3\nCONCH_ELECTRON_LINUX_X64_SHA256=\n' >"$tmp/pins.sh"
sums "$good"
run "$tmp/c5"
refuses "no pinned hash" "$tmp/c5"

# A missing pin file stops the script (it is sourced).
pins "$good"
rm "$tmp/pins.sh"
run "$tmp/c6"
[ "$status" -ne 0 ] || fail "a missing pin file was accepted"
[ ! -e "$tmp/c6.dest" ] || fail "a missing pin file left a directory behind"

# The package.json and the pin name different versions.
pins "$good"
echo '{ "devDependencies": { "electron": "1.2.4" } }' >"$tmp/desktop/package.json"
run "$tmp/c7"
refuses "package.json and pin versions differ" "$tmp/c7"
echo '{ "devDependencies": { "electron": "1.2.3" } }' >"$tmp/desktop/package.json"

# No checksums.json (npm ci was not run).
rm "$tmp/desktop/node_modules/electron/checksums.json"
run "$tmp/c8"
refuses "no checksums.json" "$tmp/c8"
sums "$good"

# Only Electron's GitHub release may be fetched over the network.
status=0
out=$(CONCH_CACHE="$tmp/c9" CONCH_ELECTRON_URL="https://example.com/e.zip" ./scripts/desktop-electron.sh "$tmp/c9.dest" 2>&1) || status=$?
refuses "a foreign address" "$tmp/c9"

# The address the script uses without an override is Electron's release.
grep -q 'https://github.com/electron/electron/releases/download/' scripts/desktop-electron.sh ||
    fail "the script does not name Electron's release address"

echo "desktop-electron-test: OK"
