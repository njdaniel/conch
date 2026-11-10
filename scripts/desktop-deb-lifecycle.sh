#!/usr/bin/env bash
# Installs, upgrades, removes and purges the desktop .deb in a clean Ubuntu
# 24.04 container (issue #1599, D6-05 of #774). `make desktop-deb-lifecycle`
# runs it.
#
# usage:
#   desktop-deb-lifecycle.sh                    builds two packages from a small
#                                               fixture tree (v0.1.0-alpha.1 and
#                                               v0.1.0-alpha.2, with
#                                               scripts/desktop-deb.sh) and runs
#                                               the lifecycle on them
#   desktop-deb-lifecycle.sh <old.deb> <new.deb>   the same on real packages:
#                                               <new> must be a higher version
#   desktop-deb-lifecycle.sh --broken           the test of the test: the same
#                                               fixture packages with their
#                                               Depends removed must FAIL at the
#                                               ldd check (it passes when they do)
#
# What it checks, each failure one line naming the step and the file or library:
#   1 install <old>   `apt install ./old.deb` in a container with nothing of
#                     ours (apt resolves Depends from the archive): the files,
#                     their owners and modes (root-owned, nothing group- or
#                     other-writable, nothing setuid), the launcher link, the
#                     desktop file with MimeType=x-scheme-handler/conch;, the
#                     icons, `xdg-mime query default x-scheme-handler/conch`
#                     once xdg-utils is there, postinst (it ran with no AppArmor
#                     in the container and did not fail; run again, it still
#                     succeeds)
#   2 seed            a test user's ~/.config/Conch/profiles.json and settings.json
#   3 upgrade <new>   `apt install ./new.deb`: the version is the higher one, the
#                     installed stamp resources/version is the new one, no file
#                     of the old package that the new one drops is left, the
#                     user's files are byte-identical
#   4 ldd             every executable ELF file of the package resolves every
#                     library (Electron's executable, conch-voice, ...), after
#                     the install and after the upgrade
#   5 helper          resources/conch-voice passes the trust rules of findHelper
#                     (desktop/main/voice/spawn.ts) as a script: a regular file,
#                     not a link, executable, root-owned, not group- or
#                     other-writable, inside resources/, and every directory
#                     above it root-owned and closed to group and others
#   6 remove, purge   remove: nothing of the package is left in /opt or /usr
#                     (the AppArmor profile, a conffile, stays until purge, as
#                     dpkg keeps conffiles); purge: nothing in /opt, /usr or
#                     /etc, the package is gone from dpkg; the user's
#                     ~/.config/Conch is still there and byte-identical
#
# What it does not do: it never runs the app and needs no display. A container
# is not a clean machine: no display, portal, keyring, PipeWire or real GPU,
# and Chromium's sandbox under Docker's seccomp is not representative. This
# test checks the package. The app on a real machine is #1602 (D6-08).
#
# It needs Docker and network: `docker pull` of the image, then apt inside
# the container fetches the package index and every Depends (the packages are
# installed from Ubuntu's archive). Without Docker it prints SKIPPED and why;
# with CI set it fails instead. The container runs as root inside, which is
# fine; the host needs no root beyond Docker access. The image is pinned by
# digest (CONCH_LIFECYCLE_IMAGE overrides it).
set -euo pipefail

# ubuntu:24.04, the multi-arch index digest, as pulled on 2026-11-10.
image=${CONCH_LIFECYCLE_IMAGE:-ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852}

# --- inside the container ----------------------------------------------------
# The script runs itself in the container as `--inside <old> <new>`, with the
# packages in /debs. step is the current step's name for the failure line.
step=start
die() {
    echo "desktop-deb-lifecycle: FAIL: step $step: $*"
    exit 1
}

app=/opt/conch-desktop
cfg=/home/tester/.config/Conch

# apt_install <deb>: installs a package file with apt (Depends from the
# archive); on failure shows the tail of apt's output.
apt_install() {
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$1" >/var/tmp/apt.log 2>&1 || {
        tail -n 15 /var/tmp/apt.log
        die "apt install $1 failed"
    }
}

# check_files: the install's files, owners and modes.
check_files() {
    local bad f
    [ -d "$app" ] || die "$app is missing"
    bad=$(find "$app" /usr/bin/conch-desktop /etc/apparmor.d/conch-desktop /usr/share/applications/chat.conch.desktop.desktop -not -type l \( ! -user root -o ! -group root \) -print -quit)
    [ -z "$bad" ] || die "$bad is not owned by root:root"
    bad=$(find "$app" -perm /022 -print -quit)
    [ -z "$bad" ] || die "$bad is group- or other-writable"
    bad=$(find "$app" -perm /6000 -print -quit)
    [ -z "$bad" ] || die "$bad is setuid or setgid"
    [ ! -e "$app/chrome-sandbox" ] || die "$app/chrome-sandbox is installed; the AppArmor profile is the sandbox's answer"
    [ -x "$app/conch-desktop" ] || die "$app/conch-desktop is missing or not executable"
    [ -L /usr/bin/conch-desktop ] || die "/usr/bin/conch-desktop is not a link"
    [ "$(readlink /usr/bin/conch-desktop)" = "$app/conch-desktop" ] || die "/usr/bin/conch-desktop points to $(readlink /usr/bin/conch-desktop), not $app/conch-desktop"
    [ -x /usr/bin/conch-desktop ] || die "/usr/bin/conch-desktop does not resolve to an executable"
    [ -f "$app/resources/version" ] || die "$app/resources/version is missing"
    [ -f /etc/apparmor.d/conch-desktop ] || die "/etc/apparmor.d/conch-desktop is missing"
    f=/usr/share/applications/chat.conch.desktop.desktop
    [ -f "$f" ] || die "$f is missing"
    grep -qx 'MimeType=x-scheme-handler/conch;' "$f" || die "$f has no 'MimeType=x-scheme-handler/conch;' line"
    grep -qx 'Exec=conch-desktop %U' "$f" || die "$f has not 'Exec=conch-desktop %U'"
    for size in 48 128 256 512; do
        f=/usr/share/icons/hicolor/${size}x${size}/apps/chat.conch.desktop.png
        [ -f "$f" ] || die "$f is missing"
    done
    dpkg -s conch-desktop 2>/dev/null | grep -qx 'Status: install ok installed' || die "dpkg does not list conch-desktop as installed"
}

# check_ldd: every executable ELF file of the package resolves its libraries.
check_ldd() {
    local f out n=0 missing
    while IFS= read -r -d '' f; do
        [ "$(head -c4 "$f" | od -An -tx1 | tr -d ' \n')" = 7f454c46 ] || continue
        n=$((n + 1))
        out=$(ldd "$f" 2>&1) || die "ldd $f failed: $out"
        missing=$(printf '%s\n' "$out" | grep 'not found' | head -n 1 || true)
        [ -z "$missing" ] || die "ldd $f: library not found:${missing#*:}"
    done < <(find "$app" -type f -perm /111 -print0 | LC_ALL=C sort -z)
    [ "$n" -gt 0 ] || die "no executable ELF file under $app"
    echo "  ldd: $n executable ELF file(s) resolve every library"
}

# check_helper: findHelper's trust rules (desktop/main/voice/spawn.ts) as a script.
check_helper() {
    local h=$app/resources/conch-voice real dir
    [ -e "$h" ] || die "$h is missing"
    [ ! -L "$h" ] || die "$h is a link"
    [ -f "$h" ] || die "$h is not a regular file"
    [ -x "$h" ] || die "$h is not executable"
    [ "$(stat -c %u "$h")" = 0 ] || die "$h is not owned by root"
    [ $((0$(stat -c %a "$h") & 022)) -eq 0 ] || die "$h may be changed by other users"
    real=$(realpath "$h")
    case $real in "$(realpath "$app/resources")"/*) ;; *) die "$h is not inside the app's resources" ;; esac
    dir=$(dirname "$real")
    while :; do
        [ -d "$dir" ] || die "$dir above the helper is not a directory"
        [ "$(stat -c %u "$dir")" = 0 ] || die "$dir above the helper is not owned by root"
        if [ $((0$(stat -c %a "$dir") & 022)) -ne 0 ]; then
            # a root-owned sticky directory is accepted
            [ $((0$(stat -c %a "$dir") & 01000)) -ne 0 ] || die "$dir above the helper can be changed by other users"
        fi
        [ "$dir" != / ] || break
        dir=$(dirname "$dir")
    done
    echo "  helper: $h passes the trust rules"
}

# check_postinst: with no AppArmor in the container the maintainer script must
# still succeed, also when it runs again.
check_postinst() {
    if [ -d /sys/kernel/security/apparmor ]; then
        echo "  note: this container sees AppArmor; the no-AppArmor path is not exercised"
    else
        echo "  postinst ran with no AppArmor in the container"
    fi
    /var/lib/dpkg/info/conch-desktop.postinst configure >/var/tmp/postinst.log 2>&1 || {
        cat /var/tmp/postinst.log
        die "postinst configure fails with no AppArmor"
    }
}

user_sums() { (cd "$cfg" && sha256sum profiles.json settings.json); }

inside() {
    local old=$1 new=$2 oldv newv sum0 list_old stale
    export DEBIAN_FRONTEND=noninteractive
    step="0 prepare"
    # _apt must read the packages (an unreadable file makes apt fall back to root).
    mkdir -p /var/tmp/pkg
    chmod 755 /var/tmp/pkg
    cp "/debs/$old" "/debs/$new" /var/tmp/pkg/
    chmod 644 /var/tmp/pkg/*.deb
    apt-get update -q >/var/tmp/apt-update.log 2>&1 || { tail -n 10 /var/tmp/apt-update.log; die "apt-get update failed (apt needs network)"; }
    oldv=$(dpkg-deb -f "/var/tmp/pkg/$old" Version)
    newv=$(dpkg-deb -f "/var/tmp/pkg/$new" Version)
    dpkg --compare-versions "$newv" gt "$oldv" || die "$new has version $newv, not higher than $old's $oldv"
    useradd -m tester

    step="1 install $oldv"
    apt_install "/var/tmp/pkg/$old"
    [ "$(dpkg-query -W -f '${Version}' conch-desktop)" = "$oldv" ] || die "installed version is not $oldv"
    check_files
    [ "$(cat "$app/resources/version")" != "" ] || die "$app/resources/version is empty"
    check_postinst
    list_old=$(dpkg -L conch-desktop | sort)
    echo "  installed $oldv"
    step="1 install $oldv (desktop database)"
    apt-get install -y -q xdg-utils desktop-file-utils >/var/tmp/apt-tools.log 2>&1 || { tail -n 10 /var/tmp/apt-tools.log; die "apt install xdg-utils failed"; }
    update-desktop-database -q /usr/share/applications
    if command -v xdg-mime >/dev/null 2>&1; then
        got=$(su tester -c 'xdg-mime query default x-scheme-handler/conch' 2>/dev/null || true)
        [ "$got" = chat.conch.desktop.desktop ] || die "xdg-mime query default x-scheme-handler/conch is '$got', not chat.conch.desktop.desktop"
        echo "  xdg-mime: x-scheme-handler/conch -> $got"
    fi
    desktop-file-validate /usr/share/applications/chat.conch.desktop.desktop || die "desktop-file-validate rejects chat.conch.desktop.desktop"
    step="4 ldd (after install)"
    check_ldd
    step="5 helper (after install)"
    check_helper

    step="2 seed"
    mkdir -p "$cfg"
    printf '{"profiles":[{"id":"p1","name":"Work","server":"https://chat.example.test"}],"last":"p1"}\n' >"$cfg/profiles.json"
    printf '{"theme":"dark","pushToTalk":{"key":"F13"}}\n' >"$cfg/settings.json"
    chown -R tester:tester /home/tester/.config
    sum0=$(user_sums)

    step="3 upgrade $oldv -> $newv"
    apt_install "/var/tmp/pkg/$new"
    [ "$(dpkg-query -W -f '${Version}' conch-desktop)" = "$newv" ] || die "installed version is not $newv"
    check_files
    check_postinst
    [ "$(user_sums)" = "$sum0" ] || die "the user's profiles.json or settings.json changed in the upgrade"
    # what the old package had and the new one drops is gone; /opt is replaced.
    stale=$(comm -23 <(printf '%s\n' "$list_old") <(dpkg -L conch-desktop | sort) | while IFS= read -r p; do
        if [ -e "$p" ] || [ -L "$p" ]; then [ -d "$p" ] && [ ! -L "$p" ] || echo "$p"; fi
    done | head -n 1)
    [ -z "$stale" ] || die "$stale of the old package is left after the upgrade"
    echo "  upgraded to $newv; $(basename "$cfg") untouched"
    step="4 ldd (after upgrade)"
    check_ldd
    step="5 helper (after upgrade)"
    check_helper

    step="6 remove"
    list_old=$(dpkg -L conch-desktop | sort)
    DEBIAN_FRONTEND=noninteractive apt-get remove -y -q conch-desktop >/var/tmp/apt-rm.log 2>&1 || { tail -n 10 /var/tmp/apt-rm.log; die "apt remove failed"; }
    [ ! -e "$app" ] || die "$app is left after remove"
    [ ! -L /usr/bin/conch-desktop ] || die "/usr/bin/conch-desktop is left after remove"
    [ -z "$(find /usr -name 'chat.conch.desktop*' -print -quit)" ] || die "$(find /usr -name 'chat.conch.desktop*' -print -quit) is left after remove"
    [ -f /etc/apparmor.d/conch-desktop ] || die "the conffile /etc/apparmor.d/conch-desktop is gone after remove (it stays until purge)"
    ! grep -qs 'chat.conch.desktop' /usr/share/applications/mimeinfo.cache || die "mimeinfo.cache still lists chat.conch.desktop.desktop after remove"
    [ "$(user_sums)" = "$sum0" ] || die "the user's files changed in the remove"

    step="6 purge"
    DEBIAN_FRONTEND=noninteractive apt-get purge -y -q conch-desktop >/var/tmp/apt-purge.log 2>&1 || { tail -n 10 /var/tmp/apt-purge.log; die "apt purge failed"; }
    ! dpkg -s conch-desktop >/dev/null 2>&1 || die "dpkg still knows conch-desktop after purge"
    while IFS= read -r p; do
        if [ -d "$p" ] && [ ! -L "$p" ]; then continue; fi
        if [ -e "$p" ] || [ -L "$p" ]; then die "$p is left after purge"; fi
    done <<<"$list_old"
    [ ! -e "$app" ] || die "$app is left after purge"
    [ ! -e /etc/apparmor.d/conch-desktop ] || die "/etc/apparmor.d/conch-desktop is left after purge"
    [ -z "$(find /usr /etc -name '*conch-desktop*' -print -quit)" ] || die "$(find /usr /etc -name '*conch-desktop*' -print -quit) is left after purge"
    [ "$(user_sums)" = "$sum0" ] || die "the user's ~/.config/Conch is changed or gone after purge"
    echo "desktop-deb-lifecycle: container: OK (install, upgrade, remove, purge)"
}

if [ "${1:-}" = --inside ]; then
    inside "$2" "$3"
    exit 0
fi

# --- on the host -------------------------------------------------------------
fail() {
    echo "desktop-deb-lifecycle: FAIL: $*" >&2
    exit 1
}

cd "$(dirname "$0")/.."
root=$PWD

broken=0
if [ "${1:-}" = --broken ]; then
    broken=1
    shift
fi
[ $# -eq 0 ] || [ $# -eq 2 ] || fail "usage: desktop-deb-lifecycle.sh [<old.deb> <new.deb> | --broken]"
[ "$broken" = 0 ] || [ $# -eq 0 ] || fail "--broken takes no package"

if ! command -v docker >/dev/null 2>&1; then
    if [ -n "${CI:-}" ]; then fail "docker is not installed (CI is set)"; fi
    echo "desktop-deb-lifecycle: SKIPPED: docker is not installed; the lifecycle runs in an ubuntu:24.04 container"
    exit 0
fi
if ! docker info >/dev/null 2>&1; then
    if [ -n "${CI:-}" ]; then fail "the Docker daemon is not reachable (CI is set)"; fi
    echo "desktop-deb-lifecycle: SKIPPED: the Docker daemon is not reachable (is it running, and is this user in the docker group?)"
    exit 0
fi

for tool in dpkg-deb dpkg-shlibdeps; do
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is not installed (apt install dpkg-dev)"
done

work=$(mktemp -d)
cname=conch-deb-lifecycle-$$
trap 'docker rm -f "$cname" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
mkdir "$work/debs"

if [ $# -eq 2 ]; then
    if ! [ -f "$1" ] || ! [ -f "$2" ]; then fail "$1 and $2 must be package files"; fi
    cp "$1" "$work/debs/old.deb"
    cp "$2" "$work/debs/new.deb"
else
    # A fixture tree: a real dynamically linked ELF file as the app (curl, which
    # needs libcurl from the archive, so a missing Depends shows; ls when curl is
    # not here), a shell script as the helper (it prints the tag's version) and,
    # in the old tree only, a file the new package drops.
    elf=$(command -v curl || command -v ls)
    mk_tree() { # mk_tree <dir> <tag>
        mkdir -p "$1/resources" "$1/locales"
        cp "$elf" "$1/conch-desktop"
        printf '#!/bin/sh\necho "conch-voice %s (fixture)"\n' "${2#v}" >"$1/resources/conch-voice"
        chmod 755 "$1/resources/conch-voice"
        echo fixture >"$1/resources/app.asar"
        printf '%s\n' "${2#v}" >"$1/resources/version"
        echo fixture >"$1/locales/en-US.pak"
    }
    export SOURCE_DATE_EPOCH=1794259228
    mk_tree "$work/tree-old" v0.1.0-alpha.1
    echo only-in-the-old-package >"$work/tree-old/resources/old-only.txt"
    mk_tree "$work/tree-new" v0.1.0-alpha.2
    CONCH_RELEASE_VERSION=v0.1.0-alpha.1 ./scripts/desktop-deb.sh "$work/tree-old" "$work/o" >/dev/null || fail "cannot build the fixture package (old)"
    CONCH_RELEASE_VERSION=v0.1.0-alpha.2 ./scripts/desktop-deb.sh "$work/tree-new" "$work/n" >/dev/null || fail "cannot build the fixture package (new)"
    cp "$work"/o/*.deb "$work/debs/old.deb"
    cp "$work"/n/*.deb "$work/debs/new.deb"
    if [ "$broken" = 1 ]; then
        # Remove Depends from both packages.
        for n in old new; do
            dpkg-deb -R "$work/debs/$n.deb" "$work/x-$n"
            grep -v '^Depends:' "$work/x-$n/DEBIAN/control" >"$work/x-$n/control.new"
            mv "$work/x-$n/control.new" "$work/x-$n/DEBIAN/control"
            chmod 644 "$work/x-$n/DEBIAN/control"
            dpkg-deb --root-owner-group -Zxz --build "$work/x-$n" "$work/debs/$n.deb" >/dev/null
        done
    fi
fi

echo "desktop-deb-lifecycle: image $image"
docker pull -q "$image" >/dev/null || fail "cannot pull $image (this needs network)"

# The names inside are fixed: old.deb and new.deb.
set +e
docker run --rm --name "$cname" \
    -v "$work/debs:/debs:ro" -v "$root/scripts/desktop-deb-lifecycle.sh:/lifecycle.sh:ro" \
    "$image" bash /lifecycle.sh --inside old.deb new.deb >"$work/out" 2>&1
status=$?
set -e
cat "$work/out"

if [ "$broken" = 1 ]; then
    if [ "$status" -ne 0 ] && grep -q 'FAIL: step 4 ldd (after install): ldd .*library not found' "$work/out"; then
        echo "desktop-deb-lifecycle: OK: a package without Depends fails at the ldd check, as it must"
        exit 0
    fi
    fail "the package without Depends did not fail at the ldd check (the test cannot catch a missing Depends)"
fi
[ "$status" -eq 0 ] || fail "the container run failed (see above)"
echo "desktop-deb-lifecycle: OK (the app was not run; #1602 covers the running app)"
