# The Electron the desktop shell (desktop/, ADR-007) is built and run with, and
# the SHA-256 of its Linux download. Sourced by scripts/desktop-electron.sh,
# which fetches it. Changing a line here changes the browser engine and the
# Node that ship in the app: it needs the same care as a dependency change
# (Nick's sign-off; CLAUDE.md).
#
# The version must equal "electron" in desktop/package.json (the script
# compares them). The hash must equal the one Electron publishes in its own npm
# package, desktop/node_modules/electron/checksums.json, which npm ci verifies
# against desktop/package-lock.json's integrity hash; the script checks the
# download against both.
#
# 44.10.0 (2026-10-27; Chromium 152.0.7977.169, Node 24.22.1): chosen by Nick on
# 2026-10-29 (#770, Q14) over the newest release at least 14 days old, 44.7.2,
# to take the Chromium and V8 backports and the window-opener and asar fixes of
# 44.8 to 44.10.
CONCH_ELECTRON_VERSION=44.10.0
CONCH_ELECTRON_LINUX_X64_SHA256=62df9375ef035f2835a18cd7f9bff1959a185da22f2ecba1daa37cd9caee1dc5
