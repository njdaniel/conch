#!/usr/bin/env bash
# Licence gate for web/ (ADR-007): what `cargo deny check licenses` is for
# voice/. Every package in web/package-lock.json, transitive and
# platform-specific ones included, must carry a licence on
# web/licences-allowlist.txt. The licence is read from the lockfile's own
# "license" field, so this needs no network, no node_modules and no tool beyond
# jq; a package without the field fails.
#
# An SPDX expression passes if it is on the list as written, or if it is
# "A OR B" with at least one side passing, or "A AND B" with both passing
# (AND binding tighter than OR, as in SPDX). Anything with parentheses inside
# it fails, rather than be guessed at.
#
# usage: web-licences.sh [lockfile [allowlist]]   (defaults: web/)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

LOCKFILE="${1:-web/package-lock.json}"
ALLOWLIST="${2:-web/licences-allowlist.txt}"

for f in "$LOCKFILE" "$ALLOWLIST"; do
    if [ ! -f "$f" ]; then
        echo "web-licences: FAIL: $f missing"
        exit 1
    fi
done

allow=$(grep -vE '^\s*(#|$)' "$ALLOWLIST" | sed 's/[[:space:]]*$//')

listed() {
    grep -qxF -- "$1" <<<"$allow"
}

# term: one licence, or licences joined by AND, all of which must pass.
term_allowed() {
    local rest=$1 part
    while :; do
        part=${rest%% AND *}
        listed "$part" || return 1
        [ "$part" = "$rest" ] && return 0
        rest=${rest#* AND }
    done
}

allowed() {
    local expr=$1 rest alt
    listed "$expr" && return 0
    if [[ "$expr" == \(*\) ]]; then
        expr=${expr:1:${#expr}-2}
    fi
    [[ "$expr" == *[\(\)]* ]] && return 1
    rest=$expr
    while :; do
        alt=${rest%% OR *}
        term_allowed "$alt" && return 0
        [ "$alt" = "$rest" ] && return 1
        rest=${rest#* OR }
    done
}

# path<TAB>version<TAB>licence for every package but the client itself (the
# lockfile's "" entry), which is this repository's.
declare -A count=() refused=()
missing=()
total=0
while IFS=$'\t' read -r path version licence; do
    name="${path##*node_modules/}@$version"
    total=$((total + 1))
    if [ -z "$licence" ]; then
        missing+=("$name")
        continue
    fi
    count[$licence]=$((${count[$licence]:-0} + 1))
    if ! allowed "$licence"; then
        refused[$licence]="${refused[$licence]:+${refused[$licence]}, }$name"
    fi
done < <(jq -r '
    .packages | to_entries[] | select(.key != "")
    | "\(.key)\t\(.value.version // "")\t\(.value.license // "" | if type == "string" then . else "" end)"' "$LOCKFILE")

failed=0
for name in "${missing[@]}"; do
    echo "web-licences: FAIL: $name has no license field in $LOCKFILE"
    failed=1
done
if [ "${#refused[@]}" -gt 0 ]; then
    while read -r licence; do
        echo "web-licences: FAIL: \"$licence\" is not in $ALLOWLIST (adding it requires Nick's sign-off); it is the licence of ${refused[$licence]}"
        failed=1
    done < <(printf '%s\n' "${!refused[@]}" | sort)
fi

summary=$(for licence in "${!count[@]}"; do echo "${count[$licence]} $licence"; done | sort -rn | paste -sd, - | sed 's/,/, /g')
echo "web-licences: $total packages: $summary"

if [ "$failed" -ne 0 ]; then
    exit 1
fi
echo "web-licences: OK"
