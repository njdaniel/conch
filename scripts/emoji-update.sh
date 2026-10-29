#!/usr/bin/env bash
# emoji-update.sh — the one way the vendored Unicode emoji data changes
# (issue #953, docs/design/message-reactions.md §3.1, third_party/unicode/README.md).
#
# It downloads emoji-test.txt for one Unicode Emoji version, refuses it unless
# its SHA-256 is the one given (writing nothing), checks that its "# Version:"
# header is that version, replaces the vendored file and its pin, and runs
# `go generate ./pkg/schema` so the Go catalog, the web catalog and the
# TypeScript constants are written from it in one run. cmd/emoji-gen refuses
# a data file whose hash is not the pin's, so the pin and the file move
# together here or not at all.
#
# Usage:
#   scripts/emoji-update.sh [--source <file>] [--dir <dir>] [--no-generate] <version> <sha256>
#
#   <version>      the Unicode Emoji version, <major>.<minor>, for example 18.0
#   <sha256>       the published file's SHA-256, lower-case hex
#   --source       read the file from <file> instead of downloading it
#                  (the test uses this; so may a machine without network)
#   --dir          the directory of the vendored files (default third_party/unicode)
#   --no-generate  stop after writing the file and the pin
#
# Since Unicode 17.0 the file is published under the Unicode version's own
# directory, https://www.unicode.org/Public/<major>.<minor>.0/emoji/.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

source=""
dir=third_party/unicode
generate=1
while [ "$#" -gt 0 ]; do
    case "$1" in
        --source) source=$2; shift 2 ;;
        --dir) dir=$2; shift 2 ;;
        --no-generate) generate=0; shift ;;
        --) shift; break ;;
        -*) echo "emoji-update: unknown option $1" >&2; exit 2 ;;
        *) break ;;
    esac
done
if [ "$#" -ne 2 ]; then
    echo "usage: scripts/emoji-update.sh [--source <file>] [--dir <dir>] [--no-generate] <version> <sha256>" >&2
    exit 2
fi
version=$1
want=$2

fail() { echo "emoji-update: refused: $*" >&2; exit 1; }

case "$version" in
    [0-9]*.[0-9]*) ;;
    *) fail "the version must be <major>.<minor>, got '$version'" ;;
esac
case "$want" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
    *) fail "the SHA-256 must be 64 lower-case hex digits" ;;
esac
[ -d "$dir" ] || fail "no directory $dir"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
candidate=$tmp/emoji-test.txt

if [ -n "$source" ]; then
    cp -- "$source" "$candidate"
else
    url="https://www.unicode.org/Public/${version}.0/emoji/emoji-test.txt"
    echo "emoji-update: downloading $url"
    curl -fsSL --proto '=https' --max-time 120 -o "$candidate" "$url" || fail "the download of $url failed"
fi

# The hash first: nothing below runs over a file that is not the one named.
got=$(sha256sum "$candidate" | cut -d' ' -f1)
if [ "$got" != "$want" ]; then
    fail "SHA-256 mismatch: the file has $got, you gave $want; nothing was written"
fi
if ! grep -q "^# Version: ${version}\$" "$candidate"; then
    fail "the file's '# Version:' header is not $version; nothing was written"
fi

cp -- "$candidate" "$dir/emoji-test.txt"
printf '%s  emoji-test.txt\n' "$want" > "$dir/emoji-test.sha256"
echo "emoji-update: $dir/emoji-test.txt is Unicode Emoji $version ($want)"

if [ "$generate" -eq 1 ]; then
    go generate ./pkg/schema
    echo "emoji-update: regenerated pkg/schema/emoji_catalog_gen.go, web/src/emoji/catalog.json and web/src/schema/generated*.ts"
    echo "emoji-update: update the version in $dir/README.md and commit everything together"
fi
