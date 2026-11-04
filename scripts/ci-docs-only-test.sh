#!/usr/bin/env bash
# Test for scripts/ci-docs-only.sh (issue #1300): the classification table,
# and that every test reading a docs file has that file listed, so a docs
# change it depends on still runs the whole CI workflow.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
script=./scripts/ci-docs-only.sh
fails=0
fail() { echo "ci-docs-only-test: FAIL: $*"; fails=1; }

# case: expected answer, then the changed paths (one per line).
check() {
    local want=$1; shift
    local got
    got=$(printf '%s\n' "$@" | "$script")
    [ "$got" = "$want" ] || fail "paths [$*] = $got, want $want"
}

check true  docs/design/admin-roles.md
check true  docs/design/admin-roles.md docs/design/delegated-administration.md
check true  README.md CHANGELOG.md
check true  .claude/skills/dogfood-check/SKILL.md
check true  docs/design/browser-ui-polish/after-error-phone.png
check true  .github/ISSUE_TEMPLATE/task.md
check true  web/README.md
check false docs/design/admin-roles.md internal/server/invites.go
check false internal/server/invites.go
check false .github/workflows/ci.yml
check false e2e/reviewpost/testdata/hostile.md
check false testdata/notes.md
check false docs/p3-walkthrough.md
check false docs/design/conch-voice.md
check false docs/design/tui-voice.md
check false docs/deploy.md
check false docs/reports/2026-07-21-principal-review.md
check false docs/design/admin-roles.md docs/p3-walkthrough.md
got=$(printf '' | "$script"); [ "$got" = false ] || fail "no paths = $got, want false"
got=$(printf '\n\n' | "$script"); [ "$got" = false ] || fail "blank lines only = $got, want false"

# Readers: every tracked Go file that names a docs path in a string, and
# every shell test that reads one outside a comment, must be one we know,
# and each known reader's docs must be listed in READ_BY_TESTS. A new
# reader fails here until its docs are added to the script.
known_readers="e2e/p3-local/main_test.go
e2e/reviewpost/post_linux_test.go
e2e/reviewpost/run_linux_test.go
pkg/schema/voice_control_test.go
pkg/schema/voice_devices_test.go
scripts/deploy-scripts-test.sh"
readers=$( { git grep -lE '"docs("|/)' -- '*.go' ':!*node_modules*' || true
             git grep -lE '^[^#]*[^A-Za-z0-9_/.-]docs/[A-Za-z0-9_.-]' -- 'scripts/*-test.sh' || true; } | sort -u)
if [ "$readers" != "$known_readers" ]; then
    fail "the files that read docs changed. Add the docs a new reader reads to READ_BY_TESTS in $script, then update known_readers here:
$(diff <(echo "$known_readers") <(echo "$readers") || true)"
fi
listed=$("$script" --read-by-tests)
for doc in docs/deploy.md docs/design/conch-voice.md docs/design/tui-voice.md docs/p3-walkthrough.md docs/reports/; do
    grep -qxF "$doc" <<<"$listed" || fail "$doc is read by a test but not in READ_BY_TESTS"
done
while IFS= read -r doc; do
    case $doc in */) [ -d "$doc" ] ;; *) [ -f "$doc" ] ;; esac || fail "READ_BY_TESTS lists $doc, which does not exist"
done <<<"$listed"

if [ "$fails" -ne 0 ]; then exit 1; fi
echo "ci-docs-only-test: OK"
