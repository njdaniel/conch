#!/bin/sh
# Fails unless the linter about to run is the golangci-lint version pinned in
# .golangci-lint-version, which is also the version CI installs. Releases
# disagree about what to report, so any other version can pass here and fail
# in CI (issue #143). Silent on success.
#
# Usage: scripts/lint-version.sh [linter command ...]   (default: golangci-lint)
set -eu

[ "$#" -gt 0 ] || set -- golangci-lint

pin="$(dirname "$0")/../.golangci-lint-version"
[ -r "$pin" ] || { echo "lint: .golangci-lint-version is missing; it names the golangci-lint version to use" >&2; exit 1; }
want=$(tr -d '[:space:]' <"$pin")
want=${want#v}
[ -n "$want" ] || { echo "lint: .golangci-lint-version is empty" >&2; exit 1; }

# The wording of the version line differs between releases; take only the
# word that follows "version".
found=$("$@" version 2>/dev/null | awk '{
	for (i = 1; i < NF; i++) if ($i == "version") { sub(/^v/, "", $(i + 1)); print $(i + 1); exit }
}') || found=

[ "$found" = "$want" ] && exit 0

echo "lint: need golangci-lint $want (.golangci-lint-version), found ${found:-none} from '$*'; install with: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$want" >&2
exit 1
