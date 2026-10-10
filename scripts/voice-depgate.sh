#!/usr/bin/env bash
# Dependency gate for voice/ (ADR-006; ADR-000 §authority: new dependencies
# require Nick's sign-off). The Rust counterpart of scripts/depgate.sh.
# Fails if a crate in the voice/ workspace depends directly on a crate that is
# not listed in voice/deps-allowlist.txt. Only direct dependencies are gated
# (normal, build and dev alike); transitive ones follow the direct ones, and
# `cargo deny check` covers where they come from, their advisories and their
# licences.
#
# usage: voice-depgate.sh [workspace-dir [allowlist]]   (defaults: voice/)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

WORKSPACE="${1:-voice}"
ALLOWLIST="${2:-voice/deps-allowlist.txt}"

if [ ! -f "$ALLOWLIST" ]; then
    echo "voice-depgate: FAIL: $ALLOWLIST missing"
    exit 1
fi

# --no-deps: only the workspace's own manifests are read, nothing is resolved
# or downloaded. A dependency on another member of the workspace is not a new
# dependency.
metadata=$(cargo metadata --no-deps --format-version 1 --manifest-path "$WORKSPACE/Cargo.toml")

violations=0
while read -r crate; do
    if ! grep -qxF "$crate" <(grep -vE '^\s*(#|$)' "$ALLOWLIST"); then
        echo "voice-depgate: FAIL: $crate is not in $ALLOWLIST (new dependencies require Nick's sign-off)"
        violations=1
    fi
done < <(jq -r '
    [.packages[].name] as $members
    | [.packages[].dependencies[] | select(.name as $n | $members | index($n) | not) | .name]
    | unique[]' <<<"$metadata")

if [ "$violations" -ne 0 ]; then
    exit 1
fi

echo "voice-depgate: OK"
