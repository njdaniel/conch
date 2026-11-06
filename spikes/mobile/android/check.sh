#!/usr/bin/env bash
# The Android spike's gate (#1109): the repository's npm gates pointed at this
# directory, then the typecheck and the Gradle lockfile checks. Outside
# `make check` on purpose. Needs the Node in .nvmrc and `npm ci` done.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(git -C "$here" rev-parse --show-toplevel)
rel=${here#"$root"/}
cd "$root"

want=$(tr -d '[:space:]' <"$here/.nvmrc")
if [ "$(node -v)" != "v${want#v}" ]; then
    echo "check: FAIL: Node $(node -v) but .nvmrc pins ${want#v}" >&2
    exit 1
fi

scripts/web-depgate.sh "$rel" "$rel/deps-allowlist.txt"
scripts/web-licences.sh "$rel/package-lock.json" "$rel/licences-allowlist.txt"

(cd "$here" && npm ci --ignore-scripts && npx tsc --noEmit)
echo "check: tsc OK"

# Gradle: the lockfiles must exist and name only exact versions (the build's
# plugins in buildscript-gradle.lockfile, the app's classpaths in
# app/gradle.lockfile).
for lock in "$here/android/buildscript-gradle.lockfile" "$here/android/app/gradle.lockfile"; do
    if ! grep -qvE '^(#|empty=)' "$lock" 2>/dev/null; then
        echo "check: FAIL: $lock missing or empty" >&2
        exit 1
    fi
    if grep -vE '^(#|empty=)' "$lock" | cut -d= -f1 | grep -E '[+\[(]|latest' >/dev/null; then
        echo "check: FAIL: $lock names a range" >&2
        exit 1
    fi
done
echo "check: Gradle lockfiles OK"
echo "check: OK"
