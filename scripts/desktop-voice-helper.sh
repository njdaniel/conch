#!/usr/bin/env bash
# Puts the voice helper into the unpacked desktop app (issue #1377, D3-6 of
# #771; the .deb and release versioning stay #774). Run by
# scripts/desktop-build.sh after electron-builder.
#
# usage: desktop-voice-helper.sh <resources directory>
#
# The helper is CONCH_VOICE_BIN if set, else bin/conch-voice (`make
# rust-build`). It is copied to <resources>/conch-voice, mode 0755, which is
# the one path main's findHelper (desktop/main/voice/spawn.ts) will run. No
# PATH lookup happens and nothing is run through a shell.
#
# "Matching": the helper must come from the same tree as the app. For the
# default bin/conch-voice that is enforced by refusing a binary older than any
# file under voice/ (outside voice/target): rebuild with `make rust-build`.
# An explicit CONCH_VOICE_BIN is taken as the caller's word. Compatibility
# itself is decided by protocol negotiation, never by a version string.
#
# With no helper the build still succeeds and says so (the app then shows
# voice unavailable); DESKTOP_REQUIRE_VOICE=1 makes that a failure.
# DESKTOP_VOICE_ROOT moves the repository root, for the script's test.
set -euo pipefail

fail() {
    echo "desktop-voice-helper: FAIL: $*" >&2
    exit 1
}

[ $# -eq 1 ] || fail "usage: desktop-voice-helper.sh <resources directory>"
resources=$1
[ -d "$resources" ] || fail "$resources is not a directory"

root=${DESKTOP_VOICE_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}
explicit=0
if [ -n "${CONCH_VOICE_BIN:-}" ]; then
    src=$CONCH_VOICE_BIN
    explicit=1
else
    src=$root/bin/conch-voice
fi
# An absolute path, so the checks, the --version run and the copy all name the
# same file: a bare name would be looked up on PATH for --version alone.
case $src in /*) ;; *) src=$PWD/$src ;; esac

if [ ! -e "$src" ] && [ ! -L "$src" ]; then
    if [ "${DESKTOP_REQUIRE_VOICE:-}" = 1 ]; then
        fail "DESKTOP_REQUIRE_VOICE=1 and there is no helper at $src (run make rust-build, or set CONCH_VOICE_BIN)"
    fi
    echo "desktop-voice-helper: built without the voice helper: no $src (make rust-build, or set CONCH_VOICE_BIN)"
    exit 0
fi

[ ! -L "$src" ] || fail "$src is a link; name the binary itself"
[ -f "$src" ] || fail "$src is not a regular file"
[ -x "$src" ] || fail "$src is not executable"

if [ "$explicit" = 0 ] && [ -d "$root/voice" ]; then
    newer=$(find "$root/voice" -path "$root/voice/target" -prune -o -type f -newer "$src" -print -quit)
    [ -z "$newer" ] || fail "$src is older than $newer: it is not built from this tree; run make rust-build"
fi

# The helper must run at all (a wrong-architecture or truncated file would
# otherwise ship). --version prints and exits 0 without touching audio.
version=$(env -i "$src" --version 2>&1) || fail "$src --version failed: $version"

dest=$resources/conch-voice
rm -f "$dest"
install -m 0755 -- "$src" "$dest"
echo "desktop-voice-helper: bundled $dest ($version) from $src"
