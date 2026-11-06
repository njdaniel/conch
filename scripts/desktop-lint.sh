#!/usr/bin/env bash
# Repository-wide rules for the desktop shell (issue #940, CLAUDE.md), the ones
# a typechecker cannot see. Run by `make desktop-check`; its test is
# scripts/desktop-lint-test.sh.
#
#   1. No file passes --no-sandbox or --disable-gpu-sandbox, sets
#      webSecurity to false, or handles Chromium's certificate-error event.
#      (Prose in docs/ may name them; code, scripts, workflows and the
#      Makefile may not.)
#   2. The tools desktop/ shares with web/ are at web/'s exact versions, so the
#      two never drift apart silently.
#
# usage: desktop-lint.sh [repository root]   (default: this repository)
set -euo pipefail

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
cd "$root"

violations=0
fail() {
    echo "desktop-lint: FAIL: $*"
    violations=1
}

banned='--no-sandbox|--disable-gpu-sandbox|webSecurity[[:space:]]*:[[:space:]]*false|certificate-error'
# This script and its test name the patterns; node_modules and the build
# outputs are not ours.
while IFS= read -r hit; do
    fail "$hit"
done < <(grep -rInE -e "$banned" \
    --include='*.ts' --include='*.tsx' --include='*.js' --include='*.mjs' --include='*.cjs' --include='*.json' \
    --include='*.sh' --include='*.yml' --include='*.yaml' --include='Makefile' \
    --exclude-dir=node_modules --exclude-dir=out --exclude-dir=dist --exclude-dir=.git --exclude-dir=target \
    --exclude='desktop-lint.sh' --exclude='desktop-lint-test.sh' \
    --exclude='package-lock.json' . || true)

if [ -f desktop/package.json ] && [ -f web/package.json ]; then
    for pkg in typescript vite vitest eslint typescript-eslint @playwright/test; do
        want=$(jq -r --arg p "$pkg" '.devDependencies[$p] // .dependencies[$p] // ""' web/package.json)
        have=$(jq -r --arg p "$pkg" '.devDependencies[$p] // ""' desktop/package.json)
        if [ -z "$want" ]; then
            fail "web/package.json does not name $pkg"
        elif [ "$have" != "$want" ]; then
            fail "desktop/package.json has $pkg \"$have\" but web/package.json has \"$want\""
        fi
    done
fi

if [ "$violations" -ne 0 ]; then
    exit 1
fi
echo "desktop-lint: OK"
