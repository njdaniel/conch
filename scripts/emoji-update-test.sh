#!/usr/bin/env bash
# emoji-update-test.sh — the test of scripts/emoji-update.sh, run by
# `make check` (issue #953): the vendored data has the pinned hash; the update
# script refuses a hash mismatch and a wrong version header, writing nothing;
# it accepts the file it is told to expect; and cmd/emoji-gen refuses a data
# file that no longer matches the pin, so a hand edit never reaches the
# catalog. Everything runs in a copy; the tree is untouched.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

UPDATE=./scripts/emoji-update.sh
VENDORED=third_party/unicode
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "emoji-update-test: FAIL: $*" >&2; exit 1; }

# The vendored file has the pinned hash.
(cd "$VENDORED" && sha256sum -c --quiet emoji-test.sha256) || fail "$VENDORED/emoji-test.txt does not match its pin"
hash=$(cut -d' ' -f1 "$VENDORED/emoji-test.sha256")
version=$(sed -n 's/^# Version: //p' "$VENDORED/emoji-test.txt")
[ -n "$version" ] || fail "no # Version: header in the vendored file"

fresh() {
    rm -rf "$WORK/unicode"
    mkdir "$WORK/unicode"
    cp "$VENDORED/emoji-test.txt" "$VENDORED/emoji-test.sha256" "$WORK/unicode/"
}
unchanged() {
    cmp -s "$VENDORED/emoji-test.txt" "$WORK/unicode/emoji-test.txt" || fail "$1: the data file was written"
    cmp -s "$VENDORED/emoji-test.sha256" "$WORK/unicode/emoji-test.sha256" || fail "$1: the pin was written"
}

# A wrong hash is refused, and nothing is written.
fresh
wrong=$(printf 'not the file' | sha256sum | cut -d' ' -f1)
status=0
out=$("$UPDATE" --source "$VENDORED/emoji-test.txt" --dir "$WORK/unicode" --no-generate "$version" "$wrong" 2>&1) || status=$?
[ "$status" -eq 1 ] || fail "a wrong hash: exit status $status, want 1; output: $out"
case "$out" in
    *"SHA-256 mismatch"*"nothing was written"*) ;;
    *) fail "a wrong hash: the output does not say so: $out" ;;
esac
unchanged "a wrong hash"

# A file whose header is not the version named is refused, even with its
# own hash.
fresh
sed 's/^# Version: .*/# Version: 0.1/' "$VENDORED/emoji-test.txt" > "$WORK/other.txt"
other=$(sha256sum "$WORK/other.txt" | cut -d' ' -f1)
status=0
out=$("$UPDATE" --source "$WORK/other.txt" --dir "$WORK/unicode" --no-generate "$version" "$other" 2>&1) || status=$?
[ "$status" -eq 1 ] || fail "a wrong version header: exit status $status, want 1; output: $out"
case "$out" in
    *"'# Version:' header is '0.1', not $version"*) ;;
    *) fail "a wrong version header: the output does not say so: $out" ;;
esac
unchanged "a wrong version header"

# The header is compared as a string, not a pattern: a file whose header
# says 18x0, with its own hash, is not the 18.0 asked for. Likewise a
# header with the version as a prefix or a suffix.
major=${version%%.*}
minor=${version#*.}
for bad in "${major}x${minor}" "${version}1" "1${version}" "${version}.0"; do
    fresh
    sed "s/^# Version: .*/# Version: $bad/" "$VENDORED/emoji-test.txt" > "$WORK/other.txt"
    other=$(sha256sum "$WORK/other.txt" | cut -d' ' -f1)
    status=0
    out=$("$UPDATE" --source "$WORK/other.txt" --dir "$WORK/unicode" --no-generate "$version" "$other" 2>&1) || status=$?
    [ "$status" -eq 1 ] || fail "header '$bad' asked for as $version: exit status $status, want 1; output: $out"
    case "$out" in
        *"'# Version:' header is '$bad', not $version"*) ;;
        *) fail "header '$bad': the output does not say so: $out" ;;
    esac
    unchanged "header '$bad'"
done

# A file with no header at all is refused too.
fresh
grep -v '^# Version:' "$VENDORED/emoji-test.txt" > "$WORK/other.txt"
other=$(sha256sum "$WORK/other.txt" | cut -d' ' -f1)
status=0
out=$("$UPDATE" --source "$WORK/other.txt" --dir "$WORK/unicode" --no-generate "$version" "$other" 2>&1) || status=$?
[ "$status" -eq 1 ] || fail "no header: exit status $status, want 1; output: $out"
case "$out" in
    *"header is 'missing'"*) ;;
    *) fail "no header: the output does not say so: $out" ;;
esac
unchanged "no header"

# A malformed hash or version is refused before anything is read: the
# version must be exactly <digits>.<digits>, the hash 64 lower-case hex.
fresh
for args in "$version abc" "x.y $hash" "$version" "${version}junk $hash" "18 $hash" "18.0.0 $hash" ".0 $hash" "18. $hash" "v$version $hash" "$version ${hash}0" "$version ${hash^^}"; do
    status=0
    # shellcheck disable=SC2086  # the arguments are split on purpose
    out=$("$UPDATE" --source "$VENDORED/emoji-test.txt" --dir "$WORK/unicode" --no-generate $args 2>&1) || status=$?
    [ "$status" -ne 0 ] || fail "arguments '$args' were accepted: $out"
    case "$args" in
        "$version abc"|"$version ${hash}0"|"$version ${hash^^}") want="SHA-256 must be 64 lower-case hex digits" ;;
        "$version") want="usage:" ;;
        *) want="version must be <major>.<minor>" ;;
    esac
    case "$out" in
        *"$want"*) ;;
        *) fail "arguments '$args': the output does not say '$want': $out" ;;
    esac
done
unchanged "malformed arguments"

# The right hash is accepted: the file and the pin are written (here, the
# same bytes into the copy).
fresh
rm "$WORK/unicode/emoji-test.txt" "$WORK/unicode/emoji-test.sha256"
out=$("$UPDATE" --source "$VENDORED/emoji-test.txt" --dir "$WORK/unicode" --no-generate "$version" "$hash" 2>&1) || fail "the right hash was refused: $out"
cmp -s "$VENDORED/emoji-test.txt" "$WORK/unicode/emoji-test.txt" || fail "the accepted file differs from the source"
cmp -s "$VENDORED/emoji-test.sha256" "$WORK/unicode/emoji-test.sha256" || fail "the written pin differs: $(cat "$WORK/unicode/emoji-test.sha256")"

# The generator refuses a data file that no longer matches the pin (a hand
# edit), and writes nothing.
fresh
printf '1F44D 1F44D ; fully-qualified # x E0.6 two thumbs\n' >> "$WORK/unicode/emoji-test.txt"
status=0
out=$(go run ./cmd/emoji-gen -data "$WORK/unicode/emoji-test.txt" -sha256 "$WORK/unicode/emoji-test.sha256" -go "$WORK/out.go" -json "$WORK/out.json" 2>&1) || status=$?
[ "$status" -eq 1 ] || fail "an edited data file: emoji-gen exit status $status, want 1; output: $out"
case "$out" in
    *"the pin says"*) ;;
    *) fail "an edited data file: emoji-gen does not name the pin: $out" ;;
esac
if [ -e "$WORK/out.go" ] || [ -e "$WORK/out.json" ]; then
    fail "an edited data file: emoji-gen wrote output"
fi

echo "emoji-update-test: OK"
