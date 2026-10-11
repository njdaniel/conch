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

# A dependency list is only worth something if the crates on it are the ones
# built. Cargo can swap a crate for another copy three ways, none of which
# changes its name: [patch] and [replace] in a manifest, and source replacement
# in a cargo configuration file. None is needed here, so each is refused. (A
# git or other-registry source that got in some other way is cargo-deny's to
# catch.)
swaps=0
while IFS= read -r -d '' manifest; do
    if grep -nE '^\s*\[\s*(patch|replace)\b' "$manifest" >/dev/null; then
        echo "voice-depgate: FAIL: $manifest has a [patch] or [replace] section, which swaps a crate for another copy under the same name"
        swaps=1
    fi
done < <(find "$WORKSPACE" -name Cargo.toml -not -path '*/target/*' -print0)
while IFS= read -r -d '' config; do
    if grep -nE '^\s*\[\s*(patch|source)\b' "$config" >/dev/null; then
        echo "voice-depgate: FAIL: $config replaces a crate or a crate source"
        swaps=1
    fi
done < <(find "$WORKSPACE" . -maxdepth 3 -path '*/.cargo/config*' -not -path './.git/*' -print0 2>/dev/null)
if [ "$swaps" -ne 0 ]; then
    exit 1
fi

# --no-deps: only the workspace's own manifests are read, nothing is resolved
# or downloaded. A dependency on another member of the workspace is not a new
# dependency. cargo runs inside the workspace so that its rust-toolchain.toml
# is the one that applies.
metadata=$(cd "$WORKSPACE" && cargo metadata --no-deps --format-version 1)

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
