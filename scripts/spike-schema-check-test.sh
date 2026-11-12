#!/usr/bin/env bash
# spike-schema-check-test.sh is the test of scripts/spike-schema-check.sh. A
# fresh copy of web/src/schema must pass it. A copy with one changed byte, or
# with a file missing, must fail it and say what to run.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

CHECK=./scripts/spike-schema-check.sh
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "spike-schema-check-test: FAIL: $*" >&2; exit 1; }

fresh() {
    rm -rf "$WORK/schema"
    mkdir "$WORK/schema"
    cp web/src/schema/generated.ts web/src/schema/validate.ts "$WORK/schema/"
}

# A fresh copy passes.
fresh
status=0
out=$("$CHECK" "$WORK/schema" 2>&1) || status=$?
[ "$status" -eq 0 ] || fail "a fresh copy does not pass: $out"

# One changed byte in either file fails, and the message names the fix.
for f in generated.ts validate.ts; do
    fresh
    printf ' ' >> "$WORK/schema/$f"
    status=0
    out=$("$CHECK" "$WORK/schema" 2>&1) || status=$?
    [ "$status" -ne 0 ] || fail "a one-byte edit to $f passes"
    grep -q "cp web/src/schema/generated.ts" <<<"$out" || fail "the failure for $f does not name the fix: $out"
done

# A missing file, either one, fails.
for f in generated.ts validate.ts; do
    fresh
    rm "$WORK/schema/$f"
    status=0
    out=$("$CHECK" "$WORK/schema" 2>&1) || status=$?
    [ "$status" -ne 0 ] || fail "a missing $f passes"
    grep -q "$f is missing" <<<"$out" || fail "the failure does not name the missing $f: $out"
done

# Two named copies: both fresh passes; one stale fails and names that one, and
# the fresh one is not blamed.
fresh
rm -rf "$WORK/second"
cp -r "$WORK/schema" "$WORK/second"
status=0
out=$("$CHECK" "$WORK/schema" "$WORK/second" 2>&1) || status=$?
[ "$status" -eq 0 ] || fail "two fresh copies do not pass: $out"
printf ' ' >> "$WORK/second/validate.ts"
status=0
out=$("$CHECK" "$WORK/schema" "$WORK/second" 2>&1) || status=$?
[ "$status" -ne 0 ] || fail "a stale second copy passes"
grep -q "copy in $WORK/second differs" <<<"$out" || fail "the failure does not name the second copy: $out"
! grep -q "copy in $WORK/schema differs" <<<"$out" || fail "the fresh copy was blamed: $out"

# The default copies, in a scratch repository holding only web/src/schema: no
# spike present passes silently; an ios copy alone is checked (fresh passes,
# stale fails); an android copy alone likewise. A spike directory that is
# absent is never an error.
REPO="$WORK/repo"
mkdir -p "$REPO/scripts" "$REPO/web/src"
cp scripts/spike-schema-check.sh "$REPO/scripts/"
cp -r web/src/schema "$REPO/web/src/"
git -C "$REPO" init -q
in_repo() { (cd "$REPO" && ./scripts/spike-schema-check.sh 2>&1); }
status=0
out=$(in_repo) || status=$?
[ "$status" -eq 0 ] || fail "no spike present does not pass: $out"
grep -q "no spike copy" <<<"$out" || fail "no spike present does not say so: $out"
for spike in ios android; do
    d="$REPO/spikes/mobile/$spike/src/logic/schema"
    mkdir -p "$d"
    cp web/src/schema/generated.ts web/src/schema/validate.ts "$d/"
    status=0
    out=$(in_repo) || status=$?
    [ "$status" -eq 0 ] || fail "a fresh $spike copy alone does not pass: $out"
    printf ' ' >> "$d/generated.ts"
    status=0
    out=$(in_repo) || status=$?
    [ "$status" -ne 0 ] || fail "a stale $spike copy alone passes"
    grep -q "spikes/mobile/$spike/src/logic/schema differs" <<<"$out" || fail "the failure does not name the $spike copy: $out"
    rm -rf "$REPO/spikes"
done

# A named directory that doesn't exist fails.
status=0
out=$("$CHECK" "$WORK/nowhere" 2>&1) || status=$?
[ "$status" -ne 0 ] || fail "a named directory that doesn't exist passes"

echo "spike-schema-check-test: OK"
