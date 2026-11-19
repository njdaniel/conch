#!/usr/bin/env bash
# The iOS spike's gate (#1393): the repository's npm gates pointed at this
# directory, then the typecheck, then the CocoaPods checks. Outside `make
# check` on purpose. Needs Bash 4+ and the Node in .nvmrc. Runs on Linux; the
# Pod half is "pending the Mac build" until Podfile.lock is available.
# Use --installed after npm ci to reuse the installed dependencies.
set -euo pipefail

if [ "$#" -gt 1 ] || { [ "$#" -eq 1 ] && [ "$1" != --installed ]; }; then
    echo "usage: $0 [--installed]" >&2
    exit 2
fi

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

if [ "$#" -eq 0 ]; then
    (cd "$here" && npm ci --ignore-scripts)
fi
(cd "$here" && npm run typecheck)
echo "check: tsc OK"

# The logic's unit tests (node --test over tsc output; no test dependency) and
# the byte-identity check of the schema copy.
(cd "$here" && npm test)
echo "check: unit tests OK"

# CocoaPods: Podfile.lock is produced on the Mac (`pod install`). Until it is
# available, report it as pending, not as a pass.
lock="$here/ios/Podfile.lock"
if [ ! -s "$lock" ]; then
    echo "check: Pods PENDING the Mac build: ios/Podfile.lock missing (README step 3)"
    echo "check: OK (npm half only; nothing iOS-native has been built)"
    exit 0
fi
if ! grep -q '^SPEC CHECKSUMS:' "$lock"; then
    echo "check: FAIL: $lock has no SPEC CHECKSUMS" >&2
    exit 1
fi
# Every pod in the lockfile is an exact version; a ranged requirement under
# DEPENDENCIES (">= ", "~>") must come from the Podfile, which names none.
if sed -n '/^DEPENDENCIES:/,/^$/p' "$lock" | grep -E '\((>|<|~>)' >/dev/null; then
    echo "check: FAIL: $lock DEPENDENCIES names a range" >&2
    exit 1
fi
if grep -q 'PENDING THE MAC BUILD' "$here/licences.md"; then
    echo "check: FAIL: Podfile.lock exists but licences.md still has the Pod placeholder (regenerate it)" >&2
    exit 1
fi
if grep -Eq '^\| .*\| NOT READ:' "$here/licences.md"; then
    echo "check: FAIL: licences.md has Pod licences that were not read; fill them in from the pod repositories" >&2
    exit 1
fi
echo "check: Pods OK"
echo "check: OK"
