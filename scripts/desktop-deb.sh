#!/usr/bin/env bash
# Builds the x86_64 .deb of the Linux desktop app (issue #1597, D6-03 of #774;
# the decisions of #1595 are Nick's of 2026-11-09). `make desktop-deb` runs it.
#
# usage: desktop-deb.sh [<unpacked app tree> [<output directory>]]
#   tree     default desktop/dist/linux-unpacked (from `make desktop-build`)
#   output   default desktop/dist; the file is conch-desktop_<version>_amd64.deb
#
# environment:
#   CONCH_RELEASE_VERSION  the release tag (v0.1.0-alpha.18). The package
#                          version is scripts/desktop-version.sh's Debian
#                          spelling of it (0.1.0~alpha.18), and the bundled
#                          helper's --version must be the tag's. Unset, this is
#                          a development package, 0.0.0~dev+<short sha>.
#   SOURCE_DATE_EPOCH      the time stamped on every entry; default the commit
#                          time of HEAD. With it, two builds of one commit make
#                          the same bytes.
#
# What it makes (desktop/build/deb/ holds the static files):
#   /opt/conch-desktop/           the tree, root-owned, nothing group- or
#                                 other-writable; resources/conch-voice 0755
#   /usr/bin/conch-desktop        a symlink to /opt/conch-desktop/conch-desktop
#                                 (`conch` stays the CLI)
#   /usr/share/applications/chat.conch.desktop.desktop, and the icons in
#   /usr/share/icons/hicolor/<size>x<size>/apps/chat.conch.desktop.png
#   /etc/apparmor.d/conch-desktop the profile that lets the one executable make
#                                 a user namespace (a conffile): the sandbox
#                                 stays on and no setuid chrome-sandbox ships,
#                                 so chrome-sandbox is left out of the package
#   DEBIAN/{control,conffiles,postinst,postrm}
# and nothing else: no udev rule, no group, no setcap, no setuid. The
# talk-key permission stays an explicit step the app shows.
#
# Depends is computed by dpkg-shlibdeps from every ELF file in the tree, on the
# machine that builds. So it names that machine's library packages (on Ubuntu
# 24.04 the t64 names such as libasound2t64): the package installs where those
# exist. Build releases on the oldest system that should install them.
#
# Refuses: no tree, no conch-voice in it, a symlink in the tree, a bad tag, a
# helper that is not the tag's. Needs dpkg-deb and dpkg-shlibdeps (dpkg-dev).
# Runs offline and never as root.
set -euo pipefail

fail() {
    echo "desktop-deb: FAIL: $*" >&2
    exit 1
}

cd "$(dirname "$0")/.."
root=$PWD
umask 022

[ $# -le 2 ] || fail "usage: desktop-deb.sh [<unpacked app tree> [<output directory>]]"
tree=${1:-desktop/dist/linux-unpacked}
out=${2:-desktop/dist}

[ "$(id -u)" != 0 ] || fail "do not build as root; the package is made root-owned by dpkg-deb --root-owner-group"
for tool in dpkg-deb dpkg-shlibdeps; do
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is not installed (apt install dpkg-dev)"
done

[ -d "$tree" ] || fail "$tree is not a directory; run \`make desktop-build\` (make desktop-deb does)"
tree=$(cd "$tree" && pwd)
if ! [ -f "$tree/conch-desktop" ] || ! [ -x "$tree/conch-desktop" ]; then
    fail "$tree/conch-desktop is missing or not executable"
fi
helper=$tree/resources/conch-voice
if ! [ -f "$helper" ] || ! [ -x "$helper" ]; then
    fail "$helper is missing or not executable; the package is not built without the voice helper (make rust-build, then make desktop-build)"
fi
if [ -n "$(find "$tree" -type l -print -quit)" ]; then
    fail "$tree holds a symbolic link ($(find "$tree" -type l -print -quit)); the package takes a plain tree"
fi

# The version.
release=${CONCH_RELEASE_VERSION:-}
if [ -n "$release" ]; then
    version=$(./scripts/desktop-version.sh deb "$release")
    ./scripts/desktop-version.sh helper "$release" "$helper"
else
    version="0.0.0~dev+$(git rev-parse --short HEAD 2>/dev/null || echo nogit)"
    echo "desktop-deb: development package (no CONCH_RELEASE_VERSION): version $version"
fi
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
    SOURCE_DATE_EPOCH=$(git log -1 --format=%ct 2>/dev/null) || fail "no SOURCE_DATE_EPOCH, and no commit to take it from"
fi
export SOURCE_DATE_EPOCH

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
stage=$work/pkg
app=$stage/opt/conch-desktop
mkdir -p "$stage/DEBIAN" "$app"

# The tree, with modes made by this script and not inherited: directories
# 0755, anything executable 0755, the rest 0644. cp -r (not -a) takes no
# ownership or time from the source; dpkg-deb stamps SOURCE_DATE_EPOCH.
cp -r "$tree/." "$app/"
rm -f "$app/chrome-sandbox"
find "$app" -type d -exec chmod 755 {} +
find "$app" -type f -perm /111 -exec chmod 755 {} +
find "$app" -type f ! -perm /111 -exec chmod 644 {} +

# Launcher, desktop file, icons, AppArmor profile, copyright.
mkdir -p "$stage/usr/bin" "$stage/usr/share/applications" "$stage/etc/apparmor.d" "$stage/usr/share/doc/conch-desktop"
ln -s /opt/conch-desktop/conch-desktop "$stage/usr/bin/conch-desktop"
install -m 644 desktop/build/deb/chat.conch.desktop.desktop "$stage/usr/share/applications/chat.conch.desktop.desktop"
install -m 644 desktop/build/deb/conch-desktop.apparmor "$stage/etc/apparmor.d/conch-desktop"
install -m 644 desktop/build/deb/copyright "$stage/usr/share/doc/conch-desktop/copyright"
for size in 48 128 256 512; do
    dir=$stage/usr/share/icons/hicolor/${size}x${size}/apps
    mkdir -p "$dir"
    install -m 644 "desktop/build/icons/$size.png" "$dir/chat.conch.desktop.png"
done
find "$stage" -type d -exec chmod 755 {} +

# Depends, from the ELF files, as installed (the DEBIAN directory lets
# dpkg-shlibdeps resolve $ORIGIN). Its warnings are noise unless it fails.
elfs=()
while IFS= read -r -d '' f; do
    if [ "$(head -c4 "$f" | od -An -tx1 | tr -d ' \n')" = 7f454c46 ]; then
        elfs+=("-e$f")
    fi
done < <(find "$app" -type f -print0 | LC_ALL=C sort -z)
[ "${#elfs[@]}" -gt 0 ] || fail "no ELF file in $tree"
mkdir "$work/shl" "$work/shl/debian"
printf 'Source: conch-desktop\n\nPackage: conch-desktop\nArchitecture: amd64\n' >"$work/shl/debian/control"
if ! (cd "$work/shl" && dpkg-shlibdeps -O "-l$app" "${elfs[@]}" >"$work/shl.out" 2>"$work/shl.err"); then
    cat "$work/shl.err" >&2
    fail "dpkg-shlibdeps failed"
fi
depends=$(sed -n 's/^shlibs:Depends=//p' "$work/shl.out")
[ -n "$depends" ] || fail "dpkg-shlibdeps found no dependency; a tree with an ELF file always has libc6"

# Installed-Size: KiB of the regular files, rounded up; the same on any file
# system, unlike du.
bytes=$(find "$stage" -type f ! -path "$stage/DEBIAN/*" -printf '%s\n' | awk '{s += $1} END {print s + 0}')
installed=$(((bytes + 1023) / 1024))

# DEBIAN.
sed -e "s|@VERSION@|$version|" -e "s|@INSTALLED_SIZE@|$installed|" -e "s|@DEPENDS@|$depends|" \
    desktop/build/deb/control.in >"$stage/DEBIAN/control"
echo /etc/apparmor.d/conch-desktop >"$stage/DEBIAN/conffiles"
install -m 755 desktop/build/deb/postinst desktop/build/deb/postrm "$stage/DEBIAN/"
chmod 644 "$stage/DEBIAN/control" "$stage/DEBIAN/conffiles"

mkdir -p "$out"
deb=$out/conch-desktop_${version}_amd64.deb
rm -f "$deb"
dpkg-deb --root-owner-group -Zxz --build "$stage" "$deb" >/dev/null

size=$(stat -c %s "$deb")
echo "desktop-deb: OK: ${deb#"$root"/} (version $version, package $((size / 1024)) KiB, installed $installed KiB)"
