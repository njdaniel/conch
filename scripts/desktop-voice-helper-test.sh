#!/usr/bin/env bash
# Tests of scripts/desktop-voice-helper.sh (issue #1377): what it copies, what
# it refuses and what it says. A stand-in script plays conch-voice; no Rust, no
# Electron. Run by `make desktop-check`.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
script=$PWD/scripts/desktop-voice-helper.sh

fail() {
    echo "desktop-voice-helper-test: FAIL: $*"
    exit 1
}

mkdir -p "$tmp/root/bin" "$tmp/root/voice/src" "$tmp/res"
printf '#!/bin/sh\necho "conch-voice 9.9.9"\n' >"$tmp/root/bin/conch-voice"
chmod 755 "$tmp/root/bin/conch-voice"
touch -d '2000-01-01' "$tmp/root/bin/conch-voice"
printf 'fn main() {}\n' >"$tmp/root/voice/src/main.rs"
touch -d '1999-01-01' "$tmp/root/voice/src/main.rs"
export DESKTOP_VOICE_ROOT=$tmp/root
unset CONCH_VOICE_BIN DESKTOP_REQUIRE_VOICE

run() { # run <args...>: sets out and rc
    set +e
    out=$("$script" "$@" 2>&1)
    rc=$?
    set -e
}

# The default binary is copied, 0755, and the output says so.
run "$tmp/res"
[ "$rc" = 0 ] || fail "default copy failed: $out"
case $out in *"bundled $tmp/res/conch-voice (conch-voice 9.9.9)"*) ;; *) fail "no 'bundled' line: $out" ;; esac
[ "$(stat -c %a "$tmp/res/conch-voice")" = 755 ] || fail "mode is not 0755"
cmp -s "$tmp/root/bin/conch-voice" "$tmp/res/conch-voice" || fail "copy differs"

# A source file in voice/ newer than the binary: stale, refused.
rm "$tmp/res/conch-voice"
touch -d '2001-01-01' "$tmp/root/voice/src/main.rs"
run "$tmp/res"
[ "$rc" != 0 ] || fail "a stale helper was accepted"
case $out in *"not built from this tree"*) ;; *) fail "stale: wrong message: $out" ;; esac
[ ! -e "$tmp/res/conch-voice" ] || fail "a stale helper was copied"
# ...but an explicit CONCH_VOICE_BIN is the caller's word.
CONCH_VOICE_BIN=$tmp/root/bin/conch-voice run "$tmp/res"
[ "$rc" = 0 ] || fail "explicit CONCH_VOICE_BIN refused: $out"
touch -d '1999-01-01' "$tmp/root/voice/src/main.rs"

# The destination is replaced, and made 0755 whatever it was.
chmod 777 "$tmp/res/conch-voice"
run "$tmp/res"
[ "$(stat -c %a "$tmp/res/conch-voice")" = 755 ] || fail "mode not reset to 0755"

# No helper: success and a statement; with DESKTOP_REQUIRE_VOICE=1, failure.
rm "$tmp/res/conch-voice" "$tmp/root/bin/conch-voice"
run "$tmp/res"
[ "$rc" = 0 ] || fail "a missing helper failed the build: $out"
case $out in *"built without the voice helper"*) ;; *) fail "no 'built without' line: $out" ;; esac
[ ! -e "$tmp/res/conch-voice" ] || fail "something was copied from nothing"
DESKTOP_REQUIRE_VOICE=1 run "$tmp/res"
[ "$rc" != 0 ] || fail "DESKTOP_REQUIRE_VOICE=1 accepted a missing helper"

# Refusals: a link, a non-executable file, a file that does not run.
printf '#!/bin/sh\necho x\n' >"$tmp/real"
chmod 755 "$tmp/real"
ln -s "$tmp/real" "$tmp/link"
CONCH_VOICE_BIN=$tmp/link run "$tmp/res"
[ "$rc" != 0 ] || fail "a link was accepted"
printf '#!/bin/sh\nexit 0\n' >"$tmp/noexec"
chmod 644 "$tmp/noexec"
CONCH_VOICE_BIN=$tmp/noexec run "$tmp/res"
[ "$rc" != 0 ] || fail "a non-executable file was accepted"
printf '#!/bin/sh\nexit 3\n' >"$tmp/broken"
chmod 755 "$tmp/broken"
CONCH_VOICE_BIN=$tmp/broken run "$tmp/res"
[ "$rc" != 0 ] || fail "a helper that does not run was accepted"
run "$tmp/nonexistent-dir"
[ "$rc" != 0 ] || fail "a missing resources directory was accepted"
[ ! -e "$tmp/res/conch-voice" ] || fail "a refused helper was copied"

echo "desktop-voice-helper-test: OK"
