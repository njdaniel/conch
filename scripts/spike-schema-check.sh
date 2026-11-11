#!/usr/bin/env bash
# spike-schema-check.sh: the Android spike keeps a verbatim copy of the web
# client's wire types (spikes/mobile/android/src/logic/schema/, issue #1719).
# `go generate ./pkg/schema` doesn't write that copy, so it goes stale when
# the schema moves. This fails, showing the diff, when a copied file differs
# from web/src/schema. It needs no Node: the spike's own byte-identity test
# runs only in its check.sh, which CI doesn't run.
#
# When M6 (#1111) replaces the spike with mobile/, this check moves there or
# goes away with the copy.
#
# Usage: scripts/spike-schema-check.sh [dir]
# dir holds the copy. It defaults to spikes/mobile/android/src/logic/schema,
# and scripts/spike-schema-check-test.sh passes a planted copy.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

dir=${1:-spikes/mobile/android/src/logic/schema}
src=web/src/schema

if [ ! -d "$dir" ]; then
    echo "spike-schema-check: OK (no spike copy at $dir)"
    exit 0
fi

stale=0
for f in generated.ts validate.ts; do
    if [ ! -f "$dir/$f" ]; then
        echo "spike-schema-check: $dir/$f is missing"
        stale=1
        continue
    fi
    if ! diff -u --label "$dir/$f (spike copy)" --label "$src/$f" "$dir/$f" "$src/$f"; then
        stale=1
    fi
done

if [ "$stale" -ne 0 ]; then
    echo "spike-schema-check: FAIL: the Android spike's schema copy differs from $src (diff above)."
    echo "Run 'cp $src/generated.ts $src/validate.ts $dir/' and commit the result."
    exit 1
fi

echo "spike-schema-check: OK"
