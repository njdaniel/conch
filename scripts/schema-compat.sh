#!/usr/bin/env bash
# Schema compatibility gate (CLAUDE.md rule 2, ADR-000 D8).
#
# Golden fixtures in pkg/schema/testdata/ pin the wire form of every published
# schema version. A change to an existing fixture is a breaking change to a
# published version and hard-fails: published versions are immutable — breaking
# changes must ship as a NEW versioned fixture (e.g. foo.v1.json -> foo.v2.json)
# via the schema-change skill, with Nick's sign-off. Adding fixtures is fine.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

FIXTURES_DIR="pkg/schema/testdata"

if [ ! -d "$FIXTURES_DIR" ] || [ -z "$(ls -A "$FIXTURES_DIR" 2>/dev/null)" ]; then
    echo "schema-compat: no golden fixtures yet ($FIXTURES_DIR empty) — skipping"
    exit 0
fi

# Diff fixtures against the merge target (CI) or HEAD (local pre-commit).
base="${SCHEMA_COMPAT_BASE:-HEAD}"
if ! git rev-parse --verify --quiet "$base" >/dev/null; then
    echo "schema-compat: base '$base' not found — skipping"
    exit 0
fi

changed=$(git diff --name-status "$base" -- "$FIXTURES_DIR" | awk '$1 ~ /^[MD]/ {print $2}')
if [ -n "$changed" ]; then
    echo "schema-compat: FAIL — published schema fixtures modified or deleted:"
    echo "$changed"
    echo "Published schema versions are immutable. Add a new version instead (schema-change skill; requires Nick's sign-off)."
    exit 1
fi

echo "schema-compat: OK"
