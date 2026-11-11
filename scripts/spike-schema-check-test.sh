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

# One changed byte fails, and the message names the fix.
fresh
printf ' ' >> "$WORK/schema/generated.ts"
status=0
out=$("$CHECK" "$WORK/schema" 2>&1) || status=$?
[ "$status" -ne 0 ] || fail "a one-byte edit to generated.ts passes"
grep -q "cp web/src/schema/generated.ts" <<<"$out" || fail "the failure does not name the fix: $out"

# A missing file fails.
fresh
rm "$WORK/schema/validate.ts"
status=0
out=$("$CHECK" "$WORK/schema" 2>&1) || status=$?
[ "$status" -ne 0 ] || fail "a missing validate.ts passes"
grep -q "validate.ts is missing" <<<"$out" || fail "the failure does not name the missing file: $out"

echo "spike-schema-check-test: OK"
