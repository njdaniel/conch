#!/usr/bin/env bash
# Dependency gate for web/ (ADR-007; ADR-000 §authority: new dependencies
# require Nick's sign-off). The Node counterpart of scripts/voice-depgate.sh.
# Fails if web/package.json:
#   - names a direct dependency (of any kind) that is not in
#     web/deps-allowlist.txt;
#   - gives any direct dependency a version that is not one exact version (a
#     range, a tag, a URL, a path or a git reference);
#   - has "overrides", which swaps a package for another under the same name;
#   - pins a Node in engines other than the one web/.nvmrc names;
# or if web/package-lock.json installs any package from somewhere other than
# the npm registry, or without an integrity hash to check it against.
# Only direct dependencies are listed; transitive ones follow them, pinned by
# the lockfile, and scripts/web-licences.sh checks their licences.
#
# usage: web-depgate.sh [web-dir [allowlist]]   (defaults: web/)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

WEB="${1:-web}"
ALLOWLIST="${2:-$WEB/deps-allowlist.txt}"
MANIFEST="$WEB/package.json"
LOCKFILE="$WEB/package-lock.json"
REGISTRY="https://registry.npmjs.org/"

for f in "$ALLOWLIST" "$MANIFEST" "$LOCKFILE" "$WEB/.nvmrc"; do
    if [ ! -f "$f" ]; then
        echo "web-depgate: FAIL: $f missing"
        exit 1
    fi
done

violations=0
fail() {
    echo "web-depgate: FAIL: $*"
    violations=1
}

# name<TAB>version for every direct dependency, whatever its kind.
while IFS=$'\t' read -r name version; do
    if ! grep -qxF "$name" <(grep -vE '^\s*(#|$)' "$ALLOWLIST"); then
        fail "$name is not in $ALLOWLIST (new dependencies require Nick's sign-off)"
    fi
    if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
        fail "$name is \"$version\" in $MANIFEST, not one exact version"
    fi
done < <(jq -r '
    [.dependencies, .devDependencies, .optionalDependencies, .peerDependencies]
    | map(. // {} | to_entries[]) | .[] | "\(.key)\t\(.value)"' "$MANIFEST")

if jq -e 'has("overrides")' "$MANIFEST" >/dev/null; then
    fail "$MANIFEST has \"overrides\", which swaps a package for another copy under the same name"
fi

want_node=$(tr -d '[:space:]' <"$WEB/.nvmrc")
have_node=$(jq -r '.engines.node // ""' "$MANIFEST")
if [ "$have_node" != "${want_node#v}" ]; then
    fail "engines.node in $MANIFEST is \"$have_node\", but $WEB/.nvmrc pins ${want_node#v}"
fi

# Every installed package: from the registry, with a hash npm ci checks.
while IFS=$'\t' read -r path resolved integrity; do
    if [[ "$resolved" != "$REGISTRY"* ]]; then
        fail "$path in $LOCKFILE comes from \"$resolved\", not $REGISTRY"
    fi
    if [ -z "$integrity" ]; then
        fail "$path in $LOCKFILE has no integrity hash"
    fi
done < <(jq -r '
    .packages | to_entries[] | select(.key != "")
    | "\(.key)\t\(.value.resolved // "")\t\(.value.integrity // "")"' "$LOCKFILE")

if [ "$violations" -ne 0 ]; then
    exit 1
fi

echo "web-depgate: OK"
