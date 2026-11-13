#!/usr/bin/env bash
# A release build of the desktop app and its .deb, with a stand-in voice helper
# (issue #1598, from the review of #1617: nothing else tested that a release
# build stamps the tag's version, because CI only made development builds).
#
# It runs what the release job runs (CONCH_RELEASE_VERSION, then
# desktop-build.sh, desktop-deb.sh and desktop-deb-test.sh), but with a shell
# script as conch-voice, so it needs no Rust build, and with a made-up tag, so
# nothing it makes can be mistaken for a release. Then it asserts that the tag's
# version, and not the development one, is everywhere the app and the package
# report one:
#   - the package.json inside the asar (what app.getVersion() returns);
#   - resources/version, the installed stamp the app reads (#1600);
#   - out/compat.json, which lists the tag as a tested server;
#   - the .deb's file name and its control Version (the Debian spelling);
#   - the bundled helper, which the build ran with --version.
# And that a helper which does not say the tag's version fails the build.
#
# Writes desktop/dist and desktop/out, as `make desktop-build` does. Needs the
# desktop Node (desktop/.nvmrc, on PATH), dpkg-dev, and the pinned Electron
# (fetched and verified by scripts/desktop-electron.sh). Run by CI's
# desktop-smoke job, which already has all of those; not part of `make check`.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
root=$PWD

fail() {
    echo "desktop-release-build-test: FAIL: $*" >&2
    exit 1
}

tag=v9.9.9-test.1
app=9.9.9-test.1
deb=9.9.9~test.1

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# A helper that says what the real one says (`conch-voice <version> (...)`).
stand_in() { # stand_in <path> <version>
    printf '#!/bin/sh\necho "conch-voice %s (stand-in for the release build test)"\n' "$2" >"$1"
    chmod 0755 "$1"
}

# 1. A helper of the wrong version must fail the build, whole.
stand_in "$tmp/wrong" 0.0.1
if CONCH_RELEASE_VERSION=$tag CONCH_VOICE_BIN=$tmp/wrong ./scripts/desktop-build.sh >"$tmp/wrong.log" 2>&1; then
    fail "a release build with a helper that says 0.0.1 succeeded"
fi
grep -q "not conch-voice $app" "$tmp/wrong.log" ||
    fail "the build with a wrong helper failed, but not for its version: $(tail -n 3 "$tmp/wrong.log")"

# 2. The right one builds, packs and passes the package test.
stand_in "$tmp/helper" "$app"
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}
CONCH_RELEASE_VERSION=$tag CONCH_VOICE_BIN=$tmp/helper ./scripts/desktop-build.sh
CONCH_RELEASE_VERSION=$tag ./scripts/desktop-deb.sh
debfile=desktop/dist/conch-desktop_${deb}_amd64.deb
[ -f "$debfile" ] || fail "no $debfile; found: $(ls desktop/dist)"
./scripts/desktop-deb-test.sh "$debfile"

# 3. The version is the tag's, in every place.
tree=desktop/dist/linux-unpacked
./desktop/node_modules/.bin/asar extract "$tree/resources/app.asar" "$tmp/asar"
asar_version=$(node -p 'require(process.argv[1]).version' "$tmp/asar/package.json")
[ "$asar_version" = "$app" ] || fail "the asar's package.json says '$asar_version', not $app"
[ "$(git status --porcelain -- desktop/package.json)" = "" ] || fail "the build changed the checked-in desktop/package.json"

[ "$(cat "$tree/resources/version")" = "$app" ] || fail "resources/version says '$(cat "$tree/resources/version")', not $app"

node -e '
  const c = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
  const s = JSON.stringify(c);
  if (!s.includes(process.argv[2])) { console.error("compat.json does not list " + process.argv[2] + ": " + s); process.exit(1); }
' desktop/out/compat.json "$app" || fail "out/compat.json does not name the tag"

control_version=$(dpkg-deb -f "$debfile" Version)
[ "$control_version" = "$deb" ] || fail "the package's Version is '$control_version', not $deb"
helper_says=$(dpkg-deb --fsys-tarfile "$debfile" | tar -xOf - ./opt/conch-desktop/resources/conch-voice | grep -c "conch-voice $app" || true)
[ "$helper_says" = 1 ] || fail "the packaged helper is not the stand-in for $app"

echo "desktop-release-build-test: OK: $tag built as $app / $deb"
