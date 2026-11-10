#!/usr/bin/env bash
# Tests of scripts/desktop-version.sh (issue #1596): the tag to app and Debian
# versions, the refusals, and the helper check. A stand-in script plays
# conch-voice. Run by `make desktop-check`.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
script=$PWD/scripts/desktop-version.sh

fail() {
    echo "desktop-version-test: FAIL: $*"
    exit 1
}

run() { # run <args...>: sets out and rc
    set +e
    out=$("$script" "$@" 2>&1)
    rc=$?
    set -e
}

# expect <command> <tag> <want>
expect() {
    run "$1" "$2"
    [ "$rc" = 0 ] || fail "$1 $2 failed: $out"
    [ "$out" = "$3" ] || fail "$1 $2 gave '$out', want '$3'"
}

expect app v0.1.0-alpha.18 0.1.0-alpha.18
expect app v1.2.3 1.2.3
expect deb v0.1.0-alpha.18 0.1.0~alpha.18
expect deb v1.2.3 1.2.3
expect deb v1.0.0-rc.1.x 1.0.0~rc.1.x

# Malformed tags: failure, and exactly one line.
for bad in 0.1.0 v1 v1.2 v1.2.3.4 v01.2.3 v1.2.3- v1.2.3-a..b v1.2.3+build v1.2.3-a_b v1.0.0-rc-1 'v1.2.3--' 'v1.2.3 ' latest ''; do
    for cmd in app deb; do
        run "$cmd" "$bad"
        [ "$rc" != 0 ] || fail "$cmd accepted '$bad'"
        [ "$(printf '%s\n' "$out" | wc -l)" = 1 ] || fail "$cmd '$bad' did not fail in one line: $out"
    done
done
run bogus v1.2.3
[ "$rc" != 0 ] || fail "an unknown command was accepted"
run
[ "$rc" != 0 ] || fail "no arguments were accepted"

# The mapping orders the way a release should. dpkg decides where it exists;
# else the order is the recorded one (the same three, in ascending order).
a=$("$script" deb v0.1.0-alpha.9)
b=$("$script" deb v0.1.0-alpha.18)
c=$("$script" deb v0.1.0)
[ "$a" = '0.1.0~alpha.9' ] && [ "$b" = '0.1.0~alpha.18' ] && [ "$c" = '0.1.0' ] || fail "recorded table: $a $b $c"
if command -v dpkg >/dev/null 2>&1; then
    dpkg --compare-versions "$a" lt "$b" || fail "dpkg: $a is not < $b"
    dpkg --compare-versions "$b" lt "$c" || fail "dpkg: $b is not < $c"
fi

# The helper check.
helper() { # helper <version words>: a stand-in that prints them
    printf '#!/bin/sh\necho "%s"\n' "$1" >"$tmp/h"
    chmod 755 "$tmp/h"
}
helper 'conch-voice 0.1.0-alpha.18 (48000 Hz, 20 ms frames)'
run helper v0.1.0-alpha.18 "$tmp/h"
[ "$rc" = 0 ] || fail "a matching helper was refused: $out"
run helper v0.1.0-alpha.19 "$tmp/h"
[ "$rc" != 0 ] || fail "a helper of another version was accepted"
case $out in *"conch-voice 0.1.0-alpha.19"*) ;; *) fail "mismatch message does not name the want: $out" ;; esac
helper 'conch-voice 0.1.0 (48000 Hz, 20 ms frames)'
run helper v0.1.0-alpha.18 "$tmp/h"
[ "$rc" != 0 ] || fail "a helper of a prefix version was accepted"
helper 'other-tool 0.1.0-alpha.18'
run helper v0.1.0-alpha.18 "$tmp/h"
[ "$rc" != 0 ] || fail "a helper with another name was accepted"
printf '#!/bin/sh\nexit 3\n' >"$tmp/h"
run helper v0.1.0-alpha.18 "$tmp/h"
[ "$rc" != 0 ] || fail "a helper that does not run was accepted"
run helper v0.1.0-alpha.18 "$tmp/missing"
[ "$rc" != 0 ] || fail "a missing helper was accepted"

echo "desktop-version-test: OK"
