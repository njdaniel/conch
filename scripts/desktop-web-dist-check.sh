#!/usr/bin/env bash
# The browser build must hold no desktop code (issue #940, linux-desktop.md §2:
# "no Electron dependency in browser graph"). Fails if any file in the web
# build mentions the desktop shell's bridge or scheme. Run it on web/dist after
# `make web-build`; CI's desktop-smoke job does.
#
# usage: desktop-web-dist-check.sh [dist directory]   (default: web/dist)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
dist=${1:-web/dist}

if [ ! -f "$dist/index.html" ]; then
    echo "desktop-web-dist-check: FAIL: $dist/index.html is missing; run make web-build first"
    exit 1
fi

if hits=$(grep -rIlE -e 'conchDesktop|conch-app:' "$dist"); then
    echo "desktop-web-dist-check: FAIL: desktop code is in the browser build:"
    echo "$hits"
    exit 1
fi
echo "desktop-web-dist-check: OK: no desktop code in $dist"
