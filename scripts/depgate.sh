#!/usr/bin/env bash
# Dependency gate (ADR-000 §authority: new go.mod deps require Nick's sign-off).
# Fails if go.mod requires a module not listed in deps-allowlist.txt.
# Only direct requirements are gated; transitive deps follow the direct ones.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

ALLOWLIST="deps-allowlist.txt"

if [ ! -f "$ALLOWLIST" ]; then
    echo "depgate: FAIL — $ALLOWLIST missing"
    exit 1
fi

violations=0
while read -r mod; do
    if ! grep -qxF "$mod" <(grep -vE '^\s*(#|$)' "$ALLOWLIST"); then
        echo "depgate: FAIL — $mod is not in $ALLOWLIST (new deps require Nick's sign-off)"
        violations=1
    fi
done < <(go mod edit -json | jq -r '(.Require // [])[] | select(.Indirect != true) | .Path')

if [ "$violations" -ne 0 ]; then
    exit 1
fi

echo "depgate: OK"
