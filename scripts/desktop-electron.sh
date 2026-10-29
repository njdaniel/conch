#!/usr/bin/env bash
# Fetches the Electron the desktop shell is built with (desktop/, ADR-007) into
# ~/.cache/conch/electron-<version>/, for electron-builder's `electronDist`.
#
# Why not `install-electron`, the bin of the electron package: it extracts with
# a package that loads prebuilt native binaries, on the machine that runs it,
# and `.npmrc` keeps install scripts off (#770, decision Q3). This script
# downloads electron-v<version>-linux-x64.zip from Electron's GitHub release
# and nowhere else, and keeps it only if its SHA-256 equals BOTH
#   - the pin in scripts/desktop-pins.sh, and
#   - the hash in desktop/node_modules/electron/checksums.json, which Electron
#     publishes in its own npm package and which npm ci has verified against
#     the integrity hash in desktop/package-lock.json.
# A zip that matches only one of them, or neither, is removed and nothing is
# unpacked. It then unpacks with the system `unzip` and does nothing on a second
# run once the directory is in place.
#
# So it needs desktop/node_modules (npm ci) first. `make desktop-build` does
# both.
#
# usage: desktop-electron.sh
#
# For the script's own test (scripts/desktop-electron-test.sh), and for a
# mirror: CONCH_CACHE moves the cache (default ~/.cache/conch);
# CONCH_DESKTOP_PINS, CONCH_DESKTOP_DIR and CONCH_ELECTRON_CHECKSUMS name the
# pin file, the desktop/ directory and the checksums.json to use;
# CONCH_ELECTRON_URL replaces the address, and only a file:// address is
# accepted: nothing but Electron's GitHub release may be fetched over the
# network. The expected hashes never change with it.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
desktop=${CONCH_DESKTOP_DIR:-$root/desktop}
. "${CONCH_DESKTOP_PINS:-$root/scripts/desktop-pins.sh}"

fail() {
    echo "desktop-electron: FAIL: $*" >&2
    exit 1
}

version=$CONCH_ELECTRON_VERSION
want_pin=$CONCH_ELECTRON_LINUX_X64_SHA256
archive="electron-v${version}-linux-x64.zip"
base="https://github.com/electron/electron/releases/download/"
url="${base}v${version}/${archive}"
if [ -n "${CONCH_ELECTRON_URL:-}" ]; then
    case "$CONCH_ELECTRON_URL" in
    file://*) url=$CONCH_ELECTRON_URL ;;
    *) fail "CONCH_ELECTRON_URL may only be a file:// address; Electron is downloaded from $base and nowhere else" ;;
    esac
fi

if [ "$(uname -s)-$(uname -m)" != "Linux-x86_64" ]; then
    fail "only Linux x86_64 is supported, this is $(uname -s) $(uname -m)"
fi
[[ "$want_pin" =~ ^[0-9a-f]{64}$ ]] || fail "scripts/desktop-pins.sh has no SHA-256 for Electron $version"

have=$(jq -r '.devDependencies.electron // ""' "$desktop/package.json") || fail "cannot read $desktop/package.json"
[ "$have" = "$version" ] || fail "desktop/package.json pins electron \"$have\" but scripts/desktop-pins.sh pins $version"

sums=${CONCH_ELECTRON_CHECKSUMS:-$desktop/node_modules/electron/checksums.json}
[ -f "$sums" ] || fail "$sums is missing; run npm ci in desktop/ first"
want_npm=$(jq -r --arg a "$archive" '.[$a] // ""' "$sums") || fail "cannot read $sums"
[ -n "$want_npm" ] || fail "$sums has no hash for $archive"
if [ "$want_npm" != "$want_pin" ]; then
    fail "scripts/desktop-pins.sh ($want_pin) and the electron package's checksums.json ($want_npm) disagree about $archive"
fi

cache="${CONCH_CACHE:-$HOME/.cache/conch}"
dir="$cache/electron-$version"
mkdir -p "$cache"

# One run at a time (two worktrees, a hook and a shell).
exec 9>"$cache/.electron.lock"
flock 9

if [ -x "$dir/electron" ] && [ "$(cat "$dir/.sha256" 2>/dev/null)" = "$want_pin" ]; then
    echo "desktop-electron: Electron $version is already in $dir"
    exit 0
fi

work=$(mktemp -d "$cache/.electron-fetch.XXXXXX")
trap 'rm -rf "$work"' EXIT

echo "desktop-electron: downloading $url"
curl --fail --silent --show-error --location --proto '=https,file' --proto-redir '=https' --output "$work/$archive" "$url" ||
    fail "could not download $url"
got=$(sha256sum "$work/$archive" | cut -d' ' -f1)
if [ "$got" != "$want_pin" ] || [ "$got" != "$want_npm" ]; then
    rm -f "$work/$archive"
    fail "SHA-256 of $archive is $got, but the pin is $want_pin and checksums.json says $want_npm; nothing was unpacked"
fi

mkdir "$work/electron"
unzip -q "$work/$archive" -d "$work/electron"
rm -f "$work/$archive"
[ -x "$work/electron/electron" ] || fail "$archive has no electron executable; nothing was put in place"
echo "$want_pin" >"$work/electron/.sha256"
rm -rf "$dir"
mv "$work/electron" "$dir" # the directory appears complete or not at all
echo "desktop-electron: Electron $version unpacked into $dir"
