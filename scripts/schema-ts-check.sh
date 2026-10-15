#!/usr/bin/env bash
# schema-ts-check.sh — the web client's wire types are generated (issue #348,
# CLAUDE.md rule 2): cmd/schema-ts writes web/src/schema/generated.ts and
# generated.families.ts from pkg/schema. This writes them again into a
# temporary directory and fails, showing the diff, when the committed files
# differ: a Go type or a table in pkg/schema/ts.go changed without
# `go generate ./pkg/schema`, or a generated file was edited by hand. It needs
# Go only, no Node.
#
# Usage: scripts/schema-ts-check.sh [dir]
# dir holds the committed files; it defaults to web/src/schema, and
# scripts/schema-ts-check-test.sh passes a copy.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

dir=${1:-web/src/schema}
GO=${GO:-go}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

"$GO" run ./cmd/schema-ts -out "$tmp"

stale=0
for f in generated.ts generated.families.ts; do
    if ! diff -u --label "$dir/$f (committed)" --label "$f (from pkg/schema)" "$dir/$f" "$tmp/$f"; then
        stale=1
    fi
done

if [ "$stale" -ne 0 ]; then
    echo "schema-ts-check: FAIL — the generated TypeScript wire types differ from pkg/schema (diff above)."
    echo "Run 'go generate ./pkg/schema' and commit the result; never edit the generated files by hand."
    exit 1
fi

echo "schema-ts-check: OK"
