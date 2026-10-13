#!/usr/bin/env bash
# invisible-chars-test.sh — the test of scripts/invisible-chars.sh: each
# forbidden character, alone in a tracked file of a throwaway repository, must
# fail the check and be named by code point; ordinary text must pass.
#
# The characters are written here as bytes (printf with \x), never as
# themselves: this file is tracked and must pass the check it tests.
set -euo pipefail

CHECK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/invisible-chars.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "invisible-chars-test: FAIL: $*" >&2; exit 1; }

new_repo() {
    rm -rf "$WORK/repo"
    git init -q "$WORK/repo"
}

# expect_refused <name> <code point> <printf bytes>
expect_refused() {
    local name=$1 want=$2 bytes=$3 out status=0
    new_repo
    # shellcheck disable=SC2059  # the bytes are this script's own constants
    printf "a line before\nx := \"$bytes\"\na line after\n" >"$WORK/repo/sample.go"
    git -C "$WORK/repo" add sample.go
    out=$("$CHECK" "$WORK/repo" 2>&1) || status=$?
    [ "$status" -eq 1 ] || fail "$name: exit status $status, want 1; output: $out"
    case "$out" in
        *"sample.go:2: "*"$want"*) ;;
        *) fail "$name: output does not name sample.go:2 and $want: $out" ;;
    esac
}

expect_refused "right-to-left override" "U+202E" '\xe2\x80\xae'
expect_refused "left-to-right embedding" "U+202A" '\xe2\x80\xaa'
expect_refused "left-to-right isolate" "U+2066" '\xe2\x81\xa6'
expect_refused "pop directional isolate" "U+2069" '\xe2\x81\xa9'
expect_refused "right-to-left mark" "U+200F" '\xe2\x80\x8f'
expect_refused "Arabic letter mark" "U+061C" '\xd8\x9c'
expect_refused "line separator" "U+2028" '\xe2\x80\xa8'
expect_refused "paragraph separator" "U+2029" '\xe2\x80\xa9'
expect_refused "C1 next line" "U+0085" '\xc2\x85'
expect_refused "escape" "U+001B" '\x1b[2J'
expect_refused "bell" "U+0007" '\x07'
expect_refused "backspace" "U+0008" '\x08'
expect_refused "delete" "U+007F" '\x7f'

# Two in one line are both named.
new_repo
printf 'x := "\xe2\x80\xae and \xe2\x80\xa8"\n' >"$WORK/repo/two.md"
git -C "$WORK/repo" add two.md
out=$("$CHECK" "$WORK/repo" 2>&1) && fail "two characters in one line passed"
case "$out" in
    *"U+2028"*"U+202E"*) ;;
    *) fail "two characters in one line are not both named: $out" ;;
esac

# Ordinary text passes: tabs, CRLF line ends, accents, CJK, an emoji sequence
# joined with zero-width joiners, a no-break space, a soft hyphen.
new_repo
printf 'tab\there\r\ncaf\xc3\xa9 \xe6\x97\xa5\xe6\x9c\xac \xf0\x9f\x91\xa8\xe2\x80\x8d\xf0\x9f\x91\xa9\xe2\x80\x8d\xf0\x9f\x91\xa7 a\xc2\xa0b so\xc2\xadft\n' >"$WORK/repo/ordinary.txt"
git -C "$WORK/repo" add ordinary.txt
out=$("$CHECK" "$WORK/repo" 2>&1) || fail "ordinary text was refused: $out"

# An untracked file is not this check's business, and a binary file is skipped.
new_repo
printf 'tracked and clean\n' >"$WORK/repo/clean.txt"
git -C "$WORK/repo" add clean.txt
printf 'untracked \xe2\x80\xae\n' >"$WORK/repo/untracked.txt"
printf '\x00\x01\x02binary\xe2\x80\xae' >"$WORK/repo/blob.bin"
git -C "$WORK/repo" add blob.bin
out=$("$CHECK" "$WORK/repo" 2>&1) || fail "an untracked or binary file was refused: $out"

echo "invisible-chars-test: OK"
