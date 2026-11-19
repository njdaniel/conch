#!/usr/bin/env bash
# mobile-rows-check-test.sh — the test of scripts/mobile-rows-check.sh: a
# matching matrix, sheet and receipt pass; each kind of disagreement fails and
# is named. Works on throwaway files only.
set -euo pipefail

CHECK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/mobile-rows-check.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "mobile-rows-check-test: FAIL: $*" >&2; exit 1; }

# A matrix of three rows, a sheet naming each, a receipt listing each.
fixtures() {
    rm -rf "$WORK/t"
    mkdir -p "$WORK/t/review" "$WORK/t/spike"
    cat >"$WORK/t/review/matrix.md" <<'EOF'
| Id | Capability | Pass criterion | Android | iOS | Experiment |
| --- | --- | --- | --- | --- | --- |
| **Foreground** | | | | | |
| M-F1 | Join | heard | P | P | ME1 |
| M-C7 | Camera prompt | asked | P | P | ME10 |
| M-H3 | iOS only | recorded | — | U / P | ME3 |
EOF
    cat >"$WORK/t/spike/sheet.md" <<'EOF'
See the [receipt](../review/receipt.md#rows).
1. **M-F1.** Join.
2. **M-C7.** Camera.
**M-H3** is iOS only.
EOF
    cat >"$WORK/t/review/receipt.md" <<'EOF'
Steps are in the [sheet](../spike/sheet.md).
| Id | Capability | Result | Evidence | Who ran it | Emulator |
| --- | --- | --- | --- | --- | --- |
| M-F1 | Join | not run | | | Pass |
| M-C7 | Camera prompt | pass | run 2 | Nick | not run |
| M-C7b | Sub-row of M-C7 | fail | run 2 | Nick | not run |
| M-H3 | iOS only | not run (iOS only) | | | not run |
EOF
}

run() { "$CHECK" "$WORK/t/review/matrix.md" "$WORK/t/spike/sheet.md" "$WORK/t/review/receipt.md" 2>&1; }

# expect_refused <name> <text the output must hold>
expect_refused() {
    local name=$1 want=$2 out status=0
    out=$(run) || status=$?
    [ "$status" -eq 1 ] || fail "$name: exit status $status, want 1; output: $out"
    case "$out" in
        *"$want"*) ;;
        *) fail "$name: output does not say \"$want\": $out" ;;
    esac
}

fixtures
out=$(run) || fail "matching files: refused: $out"

fixtures
echo '3. **M-Z9.** Nothing.' >>"$WORK/t/spike/sheet.md"
expect_refused "unknown ID in the sheet" "names M-Z9, which is not a row"

fixtures
sed -i '/M-C7/d' "$WORK/t/spike/sheet.md"
expect_refused "row missing from the sheet" "does not name matrix row M-C7"

fixtures
sed -i '/^| M-F1 /d' "$WORK/t/review/receipt.md"
expect_refused "row missing from the receipt" "lists matrix row M-F1 0 times"

fixtures
echo '| M-F1 | Join again | not run | | | |' >>"$WORK/t/review/receipt.md"
expect_refused "row listed twice" "lists matrix row M-F1 2 times"

fixtures
echo '| M-Q1 | Unknown | not run | | | |' >>"$WORK/t/review/receipt.md"
expect_refused "unknown ID in the receipt" "lists M-Q1, which is not a row"

fixtures
sed -i 's/| M-F1 | Join | not run |/| M-F1 | Join | maybe |/' "$WORK/t/review/receipt.md"
expect_refused "bad Result" "Result \"maybe\""

fixtures
echo 'And a [missing link](gone.md).' >>"$WORK/t/spike/sheet.md"
expect_refused "broken link" "links to gone.md"

echo "mobile-rows-check-test: ok"
