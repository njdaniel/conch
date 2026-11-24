#!/usr/bin/env bash
# Test of scripts/pwa-manifest-check.mjs: it must pass a good build and fail
# each broken one (issue #1970). A check that cannot fail is not a check.
# Run by `make web-check`. Needs Node, no network, no built web/dist.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
    echo "pwa-manifest-check-test: FAIL: $*"
    exit 1
}

good="$tmp/good"
mkdir "$good"
cp web/public/*.png web/public/conch-manifest.json "$good"/
cat >"$good/index.html" <<'HTML'
<!doctype html><html><head>
<link rel="manifest" href="/app/conch-manifest.json" />
</head><body></body></html>
HTML

node scripts/pwa-manifest-check.mjs "$good" >/dev/null || fail "a good build was refused"

# broken NAME SED-OR-COMMAND: copy the good build, break one thing, expect failure.
broken() {
    local name=$1
    shift
    local d="$tmp/$name"
    cp -r "$good" "$d"
    "$@" "$d"
    if node scripts/pwa-manifest-check.mjs "$d" >/dev/null 2>&1; then
        fail "$name: a broken build passed"
    fi
}

no_link() { printf '<!doctype html><html><head></head></html>' >"$1/index.html"; }
no_theme() { sed -i '/"theme_color"/d' "$1/conch-manifest.json"; }
bad_background() { sed -i 's/"background_color": "[^"]*"/"background_color": "red"/' "$1/conch-manifest.json"; }
bad_scope() { sed -i 's#"scope": "/app/"#"scope": "/"#' "$1/conch-manifest.json"; }
no_icon() { rm "$1/pwa-192.png"; }
bad_icon() { echo not a png >"$1/pwa-512.png"; }

broken no-link no_link
broken no-theme no_theme
broken bad-background bad_background
broken bad-scope bad_scope
broken no-icon no_icon
broken bad-icon bad_icon
echo "pwa-manifest-check-test: OK"
