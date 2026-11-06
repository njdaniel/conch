#!/usr/bin/env bash
# Go module scope (issue #954): web/ and desktop/ are Node projects, and npm
# packages may ship Go source in their node_modules. go.mod ignores both
# directories, so `go test ./...`, `go vet ./...` and golangci-lint never
# treat that third-party code as part of this module.
#
# The check plants a throwaway Go package in each directory and asserts that
# `go list ./...` leaves it out. So it holds whether or not npm ci has run,
# and it fails if the ignore directive is ever dropped.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

module=$(go list -m)
probes=()
cleanup() { for d in "${probes[@]}"; do rm -rf "$d"; done; }
trap cleanup EXIT

for dir in web desktop; do
    probe="$dir/zz-go-scope-probe-$$"
    mkdir -p "$probe"
    probes+=("$probe")
    printf 'package probe\n' > "$probe/probe.go"
done

if leaked=$(go list ./... | grep -E "^$module/(web|desktop)(/|$)"); then
    echo "go-scope: FAIL: go ./... reaches into a Node project (add it to go.mod's ignore block):"
    echo "$leaked"
    exit 1
fi

echo "go-scope: OK"
