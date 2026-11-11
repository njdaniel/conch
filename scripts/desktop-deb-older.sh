#!/usr/bin/env bash
# Makes the "old" package for the lifecycle test from one real .deb (#1652).
#
# usage: desktop-deb-older.sh <new.deb> <output .deb>
#
# The lifecycle test (scripts/desktop-deb-lifecycle.sh <old> <new>) upgrades
# old to new and needs new's version to be higher. A main build has one
# version, 0.0.0~dev+<sha>, and scripts/desktop-deb.sh refuses a helper whose
# --version is not the tag's, so a second, lower build of the tree cannot be
# made. This copies the package with the Debian version <version>~older, which
# dpkg sorts just below <version> (~ sorts before the end of a string), and
# changes nothing else: the same tree, scripts and Depends. Why not the
# previous release's .deb: no release has published one yet (#1598), a download
# would add a network and rate-limit failure to CI, and it could not be
# checked against this build's helper. The cost: the upgrade is between
# identical trees, so "files the old package has and the new one drops" is not
# exercised on a real package (the fixture run covers it).
set -euo pipefail

fail() {
    echo "desktop-deb-older: FAIL: $*" >&2
    exit 1
}

[ $# -eq 2 ] || fail "usage: desktop-deb-older.sh <new.deb> <output .deb>"
new=$1
out=$2
[ -f "$new" ] || fail "$new is not a file"
for tool in dpkg-deb dpkg; do
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is not installed (apt install dpkg-dev)"
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
umask 022
version=$(dpkg-deb -f "$new" Version)
[ -n "$version" ] || fail "$new has no Version"
older="${version}~older"
dpkg --compare-versions "$older" lt "$version" || fail "$older does not sort below $version"

dpkg-deb -R "$new" "$work/x"
sed -i "s/^Version: .*/Version: $older/" "$work/x/DEBIAN/control"
grep -qxF "Version: $older" "$work/x/DEBIAN/control" || fail "cannot set the version in the control file"
mkdir -p "$(dirname "$out")"
dpkg-deb --root-owner-group -Zxz --build "$work/x" "$out" >/dev/null
echo "desktop-deb-older: OK: $out (version $older, below $version)"
