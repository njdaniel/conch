#!/usr/bin/env bash
# Fetches and verifies the Electron the desktop shell is built with (desktop/, ADR-007),
# and unpacks it into a directory of the caller, for electron-builder's `electronDist`.
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
# unpacked. The verified zip stays in the cache (an owned directory that group
# and other cannot write, or the script refuses). Every run checks that zip's
# hash again and unpacks it afresh with the system `unzip`, with group and other
# write removed from every file, so the directory electron-builder packs from
# is never something a cache hit merely trusted. No download on a second run.
#
# So it needs desktop/node_modules (npm ci) first. `make desktop-build` does
# both.
#
# usage: desktop-electron.sh <directory>
#
# The directory is the caller's own, per build (desktop-build.sh uses
# desktop/out/electron-dist); it is emptied and filled. Only the verified zip
# lives in the shared cache, so two builds never share an extracted tree.
#
# The cache's parent, ~/.cache/conch, is assumed not writable by other users
# (the default home directory layout); only the electron/ directory in it is
# checked.
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

dest=${1:-}
[ -n "$dest" ] || fail "usage: desktop-electron.sh <directory to unpack Electron into>"

cache="${CONCH_CACHE:-$HOME/.cache/conch}/electron"
zip="$cache/$archive"

# The cache is owned by this user and not writable by group or other, or
# nothing in it can be trusted: a build input that someone else could have
# changed is not one. A cache that does not exist yet is made private.
if [ -e "$cache" ] || [ -L "$cache" ]; then
    if [ -L "$cache" ] || [ ! -d "$cache" ]; then
        fail "$cache is not a plain directory"
    fi
    read -r owner mode < <(stat -c '%u %a' "$cache")
    [ "$owner" = "$(id -u)" ] || fail "$cache is owned by uid $owner, not by you"
    if (((8#$mode & 8#022) != 0)); then
        fail "$cache is writable by group or other (mode $mode); chmod go-w it"
    fi
else
    mkdir -p -m 700 "$cache"
fi

# One run at a time (two worktrees, a hook and a shell).
exec 9>"$cache/.electron.lock"
flock 9

# A download directory left by a run that was killed hard (the trap did not
# run) is stale: nothing else holds the lock.
rm -rf "$cache"/.electron-fetch.*

work=$(mktemp -d "$cache/.electron-fetch.XXXXXX")
trap 'rm -rf "$work"' EXIT

# The verified zip is what the cache holds. It is checked again on every run,
# and the directory electron-builder packs from is extracted from it afresh
# every run, so nothing that happened to the old directory (or to a library in
# it) is carried forward.
if [ -f "$zip" ]; then
    got=$(sha256sum "$zip" | cut -d' ' -f1)
    if [ "$got" != "$want_pin" ]; then
        rm -f "$zip"
        fail "the cached $zip has SHA-256 $got, not the pinned $want_pin; it was removed, run this again to download it"
    fi
else
    echo "desktop-electron: downloading $url"
    curl --fail --silent --show-error --location --proto '=https,file' --proto-redir '=https' --output "$work/$archive" "$url" ||
        fail "could not download $url"
    got=$(sha256sum "$work/$archive" | cut -d' ' -f1)
    if [ "$got" != "$want_pin" ] || [ "$got" != "$want_npm" ]; then
        rm -f "$work/$archive"
        fail "SHA-256 of $archive is $got, but the pin is $want_pin and checksums.json says $want_npm; nothing was unpacked"
    fi
    chmod 600 "$work/$archive"
    mv "$work/$archive" "$zip"
fi

rm -rf "$dest"
mkdir -p "$dest"
unzip -q "$zip" -d "$dest"
[ -x "$dest/electron" ] || fail "$archive has no electron executable; $dest is not usable"
# The zip carries mode 0777 on some files. Nothing here is writable by group
# or other, before it is packed or run.
chmod -R go-w "$dest"
echo "desktop-electron: Electron $version unpacked from the verified $zip into $dest"
