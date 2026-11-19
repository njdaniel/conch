#!/usr/bin/env bash
# mobile-rows-check.sh — the Android device run sheet and its receipt agree
# with the mobile capability matrix (issue #2008).
#
#   mobile-rows-check.sh [MATRIX SHEET RECEIPT]
#
# The matrix is docs/reviews/mobile-capabilities.md: its rows are the table
# lines that start "| M-<letter><number> |". The check fails when:
#   - the run sheet names a row ID (M-F1, M-C10, ...) the matrix does not have;
#   - a matrix row is named nowhere in the run sheet;
#   - the receipt's table lacks a matrix row, or lists one twice, or lists an
#     ID that is neither a matrix row nor a matrix row with a lower-case
#     suffix (M-C7b, a sub-row of M-C7);
#   - a receipt row's Result cell does not start with pass, fail, not run or
#     unsupported;
#   - a relative Markdown link in the sheet or the receipt names a file that
#     does not exist.
# With no arguments it checks the repository's own three files.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
if [ $# -eq 0 ]; then
    set -- "$root/docs/reviews/mobile-capabilities.md" \
        "$root/spikes/mobile/android/DEVICE-RUN.md" \
        "$root/docs/reviews/mobile-android-voice.md"
fi
[ $# -eq 3 ] || { echo "usage: $0 [MATRIX SHEET RECEIPT]" >&2; exit 2; }
matrix=$1 sheet=$2 receipt=$3

errors=0
fail() { echo "mobile-rows-check: FAIL: $*" >&2; errors=$((errors + 1)); }

id_re='M-[A-Z][0-9]+'
rows=$(grep -oE "^\| $id_re \|" "$matrix" | grep -oE "$id_re" | sort -u || true)
[ -n "$rows" ] || { echo "mobile-rows-check: FAIL: no rows in $matrix" >&2; exit 1; }
is_row() { grep -qxF "$1" <<<"$rows"; }

# The sheet: every ID it names is a matrix row, and every row is named.
named=$(grep -oE "(^|[^A-Za-z0-9-])${id_re}[a-z]?([^A-Za-z0-9]|$)" "$sheet" |
    grep -oE "${id_re}[a-z]?" | sort -u || true)
while IFS= read -r id; do
    [ -z "$id" ] && continue
    is_row "$id" || fail "$sheet names $id, which is not a row of $matrix"
done <<<"$named"
while IFS= read -r id; do
    grep -qxF "$id" <<<"$named" || fail "$sheet does not name matrix row $id"
done <<<"$rows"

# The receipt's table: the column called Result, then one line per row.
result_col=$(awk -F'|' '/^\| Id \|/ { for (i = 2; i < NF; i++) { c = $i; gsub(/^ +| +$/, "", c); if (c == "Result") { print i; exit } } }' "$receipt")
[ -n "$result_col" ] || fail "$receipt has no table with Id and Result columns"
listed=$(grep -oE "^\| ${id_re}[a-z]? \|" "$receipt" | grep -oE "${id_re}[a-z]?" || true)
while IFS= read -r id; do
    [ -z "$id" ] && continue
    is_row "$id" || is_row "${id%[a-z]}" || fail "$receipt lists $id, which is not a row of $matrix"
done <<<"$(sort -u <<<"$listed")"
while IFS= read -r id; do
    n=$(grep -cxF "$id" <<<"$listed" || true)
    [ "$n" -eq 1 ] || fail "$receipt lists matrix row $id $n times, want 1"
done <<<"$rows"
if [ -n "$result_col" ]; then
    while IFS=$'\t' read -r id result; do
        case $result in
            pass* | fail* | "not run"* | unsupported*) ;;
            *) fail "$receipt row $id: Result \"$result\" is not pass, fail, not run or unsupported" ;;
        esac
    done < <(awk -F'|' -v col="$result_col" -v re="^ M-[A-Z][0-9]+[a-z]? $" \
        '$2 ~ re { id = $2; r = $col; gsub(/^ +| +$/, "", id); gsub(/^ +| +$/, "", r); print id "\t" r }' "$receipt")
fi

# Relative links in the sheet and the receipt resolve.
for f in "$sheet" "$receipt"; do
    dir=$(dirname "$f")
    while IFS= read -r target; do
        path=${target%%#*}
        [ -z "$path" ] && continue
        [ -e "$dir/$path" ] || fail "$f links to $target, which does not exist"
    done < <(grep -oE '\]\([^)[:space:]]+\)' "$f" | sed -E 's/^\]\((.*)\)$/\1/' |
        grep -vE '^([a-z]+:|#)' || true)
done

if [ "$errors" -gt 0 ]; then
    exit 1
fi
echo "mobile-rows-check: ok ($(wc -l <<<"$rows") matrix rows)"
