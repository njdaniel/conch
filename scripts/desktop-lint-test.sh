#!/usr/bin/env bash
# Tests of scripts/desktop-lint.sh: a gate that cannot fail is not a gate, so
# each rule is shown failing on what it exists to stop. Run by
# `make desktop-check`. Needs no network and no Node.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
    echo "desktop-lint-test: FAIL: $*"
    exit 1
}

fresh() { # fresh <dir>: a clean tree
    mkdir -p "$1/desktop/main" "$1/web" "$1/scripts"
    echo '{"devDependencies":{"typescript":"1.0.0","vite":"2.0.0","vitest":"3.0.0","eslint":"4.0.0","typescript-eslint":"5.0.0","@playwright/test":"6.0.0"}}' >"$1/web/package.json"
    cp "$1/web/package.json" "$1/desktop/package.json"
    echo 'export const ok = true;' >"$1/desktop/main/a.ts"
}

refuses() { # refuses <name> <dir>
    local out status=0
    out=$(./scripts/desktop-lint.sh "$2" 2>&1) || status=$?
    [ "$status" -ne 0 ] || fail "$1: accepted, want a refusal"
    grep -q 'desktop-lint: FAIL' <<<"$out" || fail "$1: no FAIL line: $out"
}

fresh "$tmp/clean"
./scripts/desktop-lint.sh "$tmp/clean" >/dev/null || fail "a clean tree was refused"

fresh "$tmp/a"
echo "app.commandLine.appendSwitch('no-sandbox'); const a = '--no-sandbox';" >"$tmp/a/desktop/main/a.ts"
refuses "--no-sandbox in code" "$tmp/a"

fresh "$tmp/b"
echo 'run: electron --disable-gpu-sandbox app' >"$tmp/b/scripts/x.sh"
refuses "--disable-gpu-sandbox in a script" "$tmp/b"

fresh "$tmp/c"
echo 'const prefs = { webSecurity: false };' >"$tmp/c/desktop/main/a.ts"
refuses "webSecurity false" "$tmp/c"

fresh "$tmp/d"
echo "app.on('certificate-error', (e) => e.preventDefault());" >"$tmp/d/desktop/main/a.ts"
refuses "a certificate-error handler" "$tmp/d"

fresh "$tmp/e"
echo '{"devDependencies":{"typescript":"1.0.1","vite":"2.0.0","vitest":"3.0.0","eslint":"4.0.0","typescript-eslint":"5.0.0","@playwright/test":"6.0.0"}}' >"$tmp/e/desktop/package.json"
refuses "a shared tool at another version" "$tmp/e"

# Prose in docs may name the flags.
fresh "$tmp/f"
mkdir "$tmp/f/docs"
echo 'Never pass --no-sandbox.' >"$tmp/f/docs/x.md"
./scripts/desktop-lint.sh "$tmp/f" >/dev/null || fail "prose in docs/ was refused"

echo "desktop-lint-test: OK"
