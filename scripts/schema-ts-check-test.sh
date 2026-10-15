#!/usr/bin/env bash
# schema-ts-check-test.sh — the test of scripts/schema-ts-check.sh: a copy of
# the committed generated files must pass it; the copy with one planted edit,
# or with a file missing, must fail it and show the line.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

CHECK=./scripts/schema-ts-check.sh
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "schema-ts-check-test: FAIL: $*" >&2; exit 1; }

fresh() {
    rm -rf "$WORK/schema"
    mkdir "$WORK/schema"
    cp web/src/schema/generated.ts web/src/schema/generated.families.ts "$WORK/schema/"
}

# The committed files, unchanged, pass.
fresh
status=0
out=$("$CHECK" "$WORK/schema" 2>&1) || status=$?
[ "$status" -eq 0 ] || fail "the committed files do not pass (run go generate ./pkg/schema): $out"

# A hand edit to a validator fails, and the diff shows it.
fresh
grep -q '^  ticket: str,$' "$WORK/schema/generated.ts" || fail "the line to plant an edit in is not in generated.ts"
sed -i 's/^  ticket: str,$/  ticket: int,/' "$WORK/schema/generated.ts"
status=0
out=$("$CHECK" "$WORK/schema" 2>&1) || status=$?
[ "$status" -eq 1 ] || fail "a planted edit: exit status $status, want 1; output: $out"
case "$out" in
    *"-  ticket: int,"*"+  ticket: str,"*"schema-ts-check: FAIL"*) ;;
    *) fail "a planted edit: the output does not show it: $out" ;;
esac

# A missing families file fails.
fresh
rm "$WORK/schema/generated.families.ts"
status=0
out=$("$CHECK" "$WORK/schema" 2>&1) || status=$?
[ "$status" -eq 1 ] || fail "a missing file: exit status $status, want 1; output: $out"

echo "schema-ts-check-test: OK"
