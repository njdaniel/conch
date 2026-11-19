#!/usr/bin/env bash
# spike-schema-check.sh: each mobile spike keeps a verbatim copy of the web
# client's wire types (spikes/mobile/android/src/logic/schema/, issue #1719;
# spikes/mobile/ios/src/logic/schema/, M5-B #1394). `go generate ./pkg/schema`
# doesn't write those copies, so one goes stale when the schema moves. This
# fails, showing the diff, when a copied file differs from web/src/schema. It
# needs no Node: each spike's own byte-identity test runs only in its
# check.sh, which CI doesn't run.
#
# When M6 (#1111) replaces the spikes with mobile/, this check moves there or
# goes away with the copies.
#
# Usage: scripts/spike-schema-check.sh [dir...]
# Each dir holds a copy. With none, it checks the default copies
# (spikes/mobile/android and spikes/mobile/ios under src/logic/schema), and a
# default that is absent is skipped silently: that spike may not exist on this
# branch, or may be gone (M6, #1111). scripts/spike-schema-check-test.sh passes
# planted copies.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

defaults=(
    spikes/mobile/android/src/logic/schema
    spikes/mobile/ios/src/logic/schema
)
src=web/src/schema

dirs=("$@")
named=1
if [ "$#" -eq 0 ]; then
    dirs=("${defaults[@]}")
    named=0
fi

stale=0
checked=0
for dir in "${dirs[@]}"; do
    if [ ! -d "$dir" ]; then
        # Only a default may be absent. A directory named on the command line
        # that doesn't exist is a mistake.
        if [ "$named" -eq 1 ]; then
            echo "spike-schema-check: FAIL: $dir is not a directory"
            exit 1
        fi
        continue
    fi
    checked=$((checked + 1))
    bad=0
    for f in generated.ts validate.ts; do
        if [ ! -f "$dir/$f" ]; then
            echo "spike-schema-check: $dir/$f is missing"
            bad=1
            continue
        fi
        if ! diff -u --label "$dir/$f (spike copy)" --label "$src/$f" "$dir/$f" "$src/$f"; then
            bad=1
        fi
    done
    if [ "$bad" -ne 0 ]; then
        echo "spike-schema-check: FAIL: the schema copy in $dir differs from $src (diff above)."
        echo "Run 'cp $src/generated.ts $src/validate.ts $dir/' and commit the result."
        stale=1
    fi
done

if [ "$stale" -ne 0 ]; then
    exit 1
fi

if [ "$checked" -eq 0 ]; then
    echo "spike-schema-check: OK (no spike copy at ${defaults[*]})"
    exit 0
fi

echo "spike-schema-check: OK"
