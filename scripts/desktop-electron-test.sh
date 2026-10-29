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
(cd "$tmp/zipsrc" && zip -q "$tmp/electron-v1.2.3-linux-x64.zip" electron)
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
    out=$(CONCH_CACHE="$1" ./scripts/desktop-electron.sh 2>&1) || status=$?
}

# refuses <name> <cache>: failed, said so on one FAIL line, left no download or
# unpacked directory behind.
refuses() {
    local name=$1 cache=$2 lines
    [ "$status" -ne 0 ] || fail "$name: succeeded, want a refusal: $out"
    lines=$(grep -c '^desktop-electron: FAIL:' <<<"$out" || true)
    [ "$lines" = 1 ] || fail "$name: want one FAIL line, got $lines: $out"
    [ ! -e "$cache/electron-1.2.3" ] || fail "$name: left $cache/electron-1.2.3 behind"
    ! compgen -G "$cache/.electron-fetch.*" >/dev/null || fail "$name: left a download behind"
}

# The good case, then the second run.
pins "$good"
sums "$good"
run "$tmp/c1"
[ "$status" -eq 0 ] || fail "good zip refused: $out"
[ -x "$tmp/c1/electron-1.2.3/electron" ] || fail "good zip was not unpacked"
run "$tmp/c1"
[ "$status" -eq 0 ] || fail "second run failed: $out"
grep -q 'already in' <<<"$out" || fail "second run did not say it was a no-op: $out"
! grep -q 'downloading' <<<"$out" || fail "second run downloaded again: $out"

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
[ ! -e "$tmp/c6/electron-1.2.3" ] || fail "a missing pin file left a directory behind"

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
out=$(CONCH_CACHE="$tmp/c9" CONCH_ELECTRON_URL="https://example.com/e.zip" ./scripts/desktop-electron.sh 2>&1) || status=$?
refuses "a foreign address" "$tmp/c9"

# The address the script uses without an override is Electron's release.
grep -q 'https://github.com/electron/electron/releases/download/' scripts/desktop-electron.sh ||
    fail "the script does not name Electron's release address"

echo "desktop-electron-test: OK"
