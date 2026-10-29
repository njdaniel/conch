#!/usr/bin/env bash
# Builds the unpacked Linux desktop app, desktop/dist/linux-unpacked/ (issue
# #940, `make desktop-build`):
#
#   1. npm ci in web/ and desktop/ (the lockfiles, nothing else installs);
#   2. the Electron binary, fetched and verified by scripts/desktop-electron.sh;
#   3. web's own Vite build, at base "/", into desktop/out/ui. No file under
#      web/ is edited and web/dist is not touched, so the browser build stays
#      what it was;
#   4. main and the preload, bundled into desktop/out/;
#   5. the asset manifest of the files in desktop/out/ui;
#   6. electron-builder, packing them into an asar and setting the fuses,
#      around the Electron of step 2 (`electronDist`), so it downloads nothing.
#
# Needs the Node desktop/.nvmrc names on PATH (the Makefile arranges it).
set -euo pipefail

cd "$(dirname "$0")/.."
root=$PWD

(cd web && npm ci --no-audit)
(cd desktop && npm ci --no-audit)
./scripts/desktop-electron.sh

. ./scripts/desktop-pins.sh
electron_dist="${CONCH_CACHE:-$HOME/.cache/conch}/electron-$CONCH_ELECTRON_VERSION"

rm -rf desktop/out desktop/dist
(cd web && ./node_modules/.bin/vite build --base / --outDir "$root/desktop/out/ui" --emptyOutDir)
(cd desktop && npm run bundle && node build/manifest.ts)

# CI=true keeps electron-builder from asking the registry for a newer
# electron-builder; --publish never keeps it from publishing anything.
(cd desktop && CI=true ./node_modules/.bin/electron-builder --linux dir --publish never \
    --config build/electron-builder.config.json \
    "-c.electronDist=$electron_dist" "-c.electronVersion=$CONCH_ELECTRON_VERSION")

echo "desktop-build: OK: desktop/dist/linux-unpacked/"
