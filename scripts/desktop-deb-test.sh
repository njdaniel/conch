#!/usr/bin/env bash
# Checks the desktop .deb (issue #1597, D6-03 of #774).
#
# usage:
#   desktop-deb-test.sh <package.deb>...   full mode: the checks below, on the
#                                          real package (make desktop-deb)
#   desktop-deb-test.sh                    fixture mode (make desktop-check):
#                                          builds packages from a small fixture
#                                          tree with scripts/desktop-deb.sh and
#                                          checks them, then proves each check
#                                          can fail on a package made to break it
#
# A package fails on any of:
#   - an entry not owned by root:root;
#   - a group- or other-writable entry;
#   - a setuid or setgid entry (the AppArmor profile is the sandbox's answer,
#     #1595 Q-D6-3, so there is no chrome-sandbox to make setuid);
#   - a udev rule, anything in /etc but the AppArmor profile, or a path outside
#     the ones the package owns;
#   - a missing, non-regular or not 0755 /opt/conch-desktop/resources/conch-voice,
#     or a missing /opt/conch-desktop/conch-desktop or /usr/bin/conch-desktop link;
#   - a desktop file that desktop-file-validate rejects (when the tool is
#     installed; the run says when it is not), or one with the wrong Exec, Icon
#     or StartupWMClass;
#   - the text --no-sandbox in any text file, maintainer scripts included;
#   - a maintainer script that changes groups, capabilities, setuid bits or udev;
#   - a missing /opt/conch-desktop/resources/version (the installed stamp the
#     running app reads to notice an update, issue #1600), one not 0644
#     root-owned, or one whose app version is not the package's (the deb
#     Version is its Debian spelling: first '-' to '~', later '-' to '.');
#   - a control file with the wrong Package or Architecture, no Version, or an
#     empty or missing Depends.
# Fixture mode also builds twice, the second time from a copy of the tree with
# other times and permissions, and requires the two packages to be identical.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

fail() {
    echo "desktop-deb-test: FAIL: $*" >&2
    exit 1
}

for tool in dpkg-deb dpkg-shlibdeps; do
    command -v "$tool" >/dev/null 2>&1 || fail "$tool is not installed (apt install dpkg-dev)"
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

have_validator=0
command -v desktop-file-validate >/dev/null 2>&1 && have_validator=1
if [ "$have_validator" = 0 ]; then
    echo "desktop-deb-test: note: desktop-file-validate is not installed (apt install desktop-file-utils); the desktop file is checked by hand only"
fi

# check_deb <deb>: prints one line per problem; succeeds only if there are none.
check_deb() {
    local deb=$1 problems=$tmp/problems listing=$tmp/listing x=$tmp/extract
    : >"$problems"
    rm -rf "$x"
    mkdir -p "$x"
    dpkg-deb -c "$deb" >"$listing" || { echo "cannot list $deb"; return 1; }
    dpkg-deb -x "$deb" "$x"
    dpkg-deb -e "$deb" "$x/DEBIAN"

    # Ownership, writability, setuid, udev, and paths: from the listing, whose
    # columns are mode, owner/group, size, date, time and name [-> target].
    awk '
        { mode = $1; own = $2; name = $6 }
        own != "root/root"                         { print "not owned by root:root: " name " (" own ")" }
        mode !~ /^l/ && substr(mode, 6, 1) == "w"  { print "group-writable: " name }
        mode !~ /^l/ && substr(mode, 9, 1) == "w"  { print "other-writable: " name }
        substr(mode, 4, 1) ~ /[sS]/                { print "setuid: " name }
        substr(mode, 7, 1) ~ /[sS]/                { print "setgid: " name }
        name ~ /udev/ || name ~ /\.rules$/         { print "udev rule: " name }
        name ~ /^\.\/etc\// && name != "./etc/" && name != "./etc/apparmor.d/" && name != "./etc/apparmor.d/conch-desktop" {
            print "in /etc beyond the AppArmor profile: " name
        }
        name !~ /^\.\/(opt\/conch-desktop|usr\/bin\/conch-desktop|usr\/share\/applications\/chat\.conch\.desktop\.desktop|usr\/share\/icons\/hicolor|usr\/share\/doc\/conch-desktop|etc\/apparmor\.d\/conch-desktop)(\/|$)/ \
            && name !~ /^\.\/(opt|usr|usr\/bin|usr\/share|usr\/share\/applications|usr\/share\/icons|usr\/share\/doc|etc|etc\/apparmor\.d)\/?$/ \
            && name != "./" {
            print "outside the package'"'"'s own paths: " name
        }
    ' "$listing" >>"$problems"

    # The helper and the launcher.
    local helper
    helper=$(awk '$6 == "./opt/conch-desktop/resources/conch-voice" {print $1}' "$listing")
    if [ -z "$helper" ]; then
        echo "missing /opt/conch-desktop/resources/conch-voice" >>"$problems"
    elif [ "$helper" != "-rwxr-xr-x" ]; then
        echo "resources/conch-voice is $helper, not -rwxr-xr-x" >>"$problems"
    fi
    awk '$6 == "./opt/conch-desktop/conch-desktop" && $1 ~ /^-rwx/ {f = 1} END {exit !f}' "$listing" ||
        echo "missing or not executable /opt/conch-desktop/conch-desktop" >>"$problems"
    awk '$6 == "./usr/bin/conch-desktop" && $7 == "->" && $8 == "/opt/conch-desktop/conch-desktop" {f = 1} END {exit !f}' "$listing" ||
        echo "/usr/bin/conch-desktop is not a link to /opt/conch-desktop/conch-desktop" >>"$problems"

    # The installed stamp (issue #1600): the app version the build wrote beside
    # the asar. The deb Version is its Debian spelling, so the stamp is mapped
    # the same way as scripts/desktop-version.sh does (it takes only tags, and
    # a development version has build metadata, so the mapping is repeated).
    local stamp_line stamp_file=$x/opt/conch-desktop/resources/version
    stamp_line=$(awk '$6 == "./opt/conch-desktop/resources/version" {print $1 " " $2}' "$listing")
    if [ -z "$stamp_line" ]; then
        echo "missing /opt/conch-desktop/resources/version" >>"$problems"
    else
        [ "$stamp_line" = "-rw-r--r-- root/root" ] || echo "resources/version is '$stamp_line', not '-rw-r--r-- root/root'" >>"$problems"
        local app mapped debv
        app=$(tr -d '\n' <"$stamp_file")
        mapped=$app
        if [[ $app == *-* ]]; then
            local pre=${app#*-}
            mapped="${app%%-*}~${pre//-/.}"
        fi
        debv=$(dpkg-deb -f "$deb" Version)
        [ "$mapped" = "$debv" ] || echo "resources/version says '$app', which is not the package's Version '$debv'" >>"$problems"
    fi

    # The desktop file.
    local df=$x/usr/share/applications/chat.conch.desktop.desktop
    if [ ! -f "$df" ]; then
        echo "missing chat.conch.desktop.desktop" >>"$problems"
    else
        if [ "$have_validator" = 1 ]; then
            desktop-file-validate "$df" >"$tmp/dfv" 2>&1 || sed 's/^/desktop-file-validate: /' "$tmp/dfv" >>"$problems"
        fi
        grep -qx 'Exec=conch-desktop %U' "$df" || echo "desktop file: Exec is not 'conch-desktop %U'" >>"$problems"
        grep -qx 'Icon=chat.conch.desktop' "$df" || echo "desktop file: Icon is not chat.conch.desktop" >>"$problems"
        grep -qx 'StartupWMClass=conch-desktop' "$df" || echo "desktop file: StartupWMClass is not the executable's name" >>"$problems"
        grep -qx 'Name=Conch' "$df" || echo "desktop file: Name is not Conch" >>"$problems"
    fi

    # The sandbox flag, in any text file (grep -I skips binaries: Chromium's own
    # binaries name their switches, and are not ours to rewrite).
    local hit
    hit=$(grep -rIl -e '--no-sandbox' "$x" 2>/dev/null || true)
    [ -z "$hit" ] || echo "--no-sandbox in: ${hit//$x/}" >>"$problems"

    # Maintainer scripts: only the reload and the caches.
    local s
    for s in preinst postinst prerm postrm; do
        [ -f "$x/DEBIAN/$s" ] || continue
        if grep -nE 'setcap|usermod|adduser|addgroup|gpasswd|groupadd|udevadm|/udev|chmod[^#]*[ugoa]*[+=][rwx]*s|chmod[^#]* [2467][0-7]{3}|chown|dpkg-statoverride' "$x/DEBIAN/$s" >"$tmp/ms"; then
            sed "s|^|DEBIAN/$s: |" "$tmp/ms" >>"$problems"
        fi
    done

    # The control file.
    local f
    [ "$(dpkg-deb -f "$deb" Package)" = conch-desktop ] || echo "Package is not conch-desktop" >>"$problems"
    [ "$(dpkg-deb -f "$deb" Architecture)" = amd64 ] || echo "Architecture is not amd64" >>"$problems"
    f=$(dpkg-deb -f "$deb" Version)
    [[ $f =~ ^[0-9]+\.[0-9]+\.[0-9]+([~+.][0-9A-Za-z.+~]+)?$ ]] || echo "Version '$f' is not a release version" >>"$problems"
    f=$(dpkg-deb -f "$deb" Depends)
    [[ $f =~ [^[:space:]] ]] || echo "Depends is empty or missing" >>"$problems"
    [[ $f == *libc6* ]] || echo "Depends does not name libc6 ($f)" >>"$problems"
    [ -n "$(dpkg-deb -f "$deb" Maintainer)" ] || echo "Maintainer is empty" >>"$problems"
    [ -n "$(dpkg-deb -f "$deb" Homepage)" ] || echo "Homepage is empty" >>"$problems"
    [ "$(cat "$x/DEBIAN/conffiles" 2>/dev/null)" = /etc/apparmor.d/conch-desktop ] || echo "conffiles is not exactly the AppArmor profile" >>"$problems"

    if [ -s "$problems" ]; then
        cat "$problems"
        return 1
    fi
}

# --- full mode ---------------------------------------------------------------
if [ $# -gt 0 ]; then
    for deb in "$@"; do
        [ -f "$deb" ] || fail "$deb is not a file"
        if ! check_deb "$deb" >"$tmp/out" 2>&1; then
            sed 's/^/  /' "$tmp/out" >&2
            fail "$deb"
        fi
        echo "desktop-deb-test: $deb: $(dpkg-deb -f "$deb" Package Version Architecture | tr '\n' ' ')"
        echo "desktop-deb-test: Depends: $(dpkg-deb -f "$deb" Depends)"
    done
    echo "desktop-deb-test: OK"
    exit 0
fi

# --- fixture mode ------------------------------------------------------------
tree=$tmp/tree
mkdir -p "$tree/resources" "$tree/locales"
cp /bin/true "$tree/conch-desktop"
cp /bin/true "$tree/chrome-sandbox"
chmod 4755 "$tree/chrome-sandbox" 2>/dev/null || chmod 755 "$tree/chrome-sandbox"
printf '#!/bin/sh\necho "conch-voice 0.1.0-alpha.18 (fixture)"\n' >"$tree/resources/conch-voice"
chmod 777 "$tree/resources/conch-voice"
echo fixture >"$tree/resources/app.asar"
printf '0.1.0-alpha.18\n' >"$tree/resources/version"
echo fixture >"$tree/locales/en-US.pak"
echo fixture >"$tree/LICENSE"
chmod 666 "$tree/LICENSE"

export SOURCE_DATE_EPOCH=1794259228
tag=v0.1.0-alpha.18

build() { # build <tree> <out dir>; extra environment passes through
    ./scripts/desktop-deb.sh "$1" "$2" >"$tmp/build.out" 2>&1 || { cat "$tmp/build.out" >&2; return 1; }
}

# The builder refuses what it must.
expect_refusal() { # expect_refusal <what> <tree> <want text> [env...]
    local what=$1 t=$2 want=$3
    shift 3
    if env "$@" ./scripts/desktop-deb.sh "$t" "$tmp/refused" >"$tmp/refusal" 2>&1; then
        fail "desktop-deb.sh built a package despite $what"
    fi
    grep -q -e "$want" "$tmp/refusal" || { cat "$tmp/refusal" >&2; fail "desktop-deb.sh refused $what, but not with '$want'"; }
}
expect_refusal "no tree" "$tmp/nothing" "is not a directory"
cp -r "$tree" "$tmp/nohelper" && rm "$tmp/nohelper/resources/conch-voice"
expect_refusal "no helper" "$tmp/nohelper" "voice helper"
cp -r "$tree" "$tmp/oldhelper" && printf '#!/bin/sh\necho "conch-voice 0.0.1 (old)"\n' >"$tmp/oldhelper/resources/conch-voice"
expect_refusal "a helper of another version" "$tmp/oldhelper" "not conch-voice 0.1.0-alpha.18" CONCH_RELEASE_VERSION=$tag
expect_refusal "a malformed tag" "$tree" "is not a release tag" CONCH_RELEASE_VERSION=0.1.0
cp -r "$tree" "$tmp/linked" && ln -s /etc/passwd "$tmp/linked/passwd"
expect_refusal "a symbolic link in the tree" "$tmp/linked" "symbolic link"

# A release package.
CONCH_RELEASE_VERSION=$tag build "$tree" "$tmp/a"
deb=$tmp/a/conch-desktop_0.1.0~alpha.18_amd64.deb
[ -f "$deb" ] || fail "no $deb; got $(ls "$tmp/a")"
[ "$(dpkg-deb -f "$deb" Version)" = 0.1.0~alpha.18 ] || fail "Version is $(dpkg-deb -f "$deb" Version)"
if ! check_deb "$deb" >"$tmp/out" 2>&1; then
    sed 's/^/  /' "$tmp/out" >&2
    fail "the fixture package does not pass its own checks"
fi
dpkg-deb -c "$deb" | grep -q 'chrome-sandbox' && fail "chrome-sandbox is in the package"
dpkg-deb -c "$deb" | grep -q 'resources/conch-voice$' || fail "conch-voice is not in the package"
# The fixture's loose modes (0777 helper, 0666 file, setuid sandbox) were made safe.
echo "desktop-deb-test: fixture package: $(dpkg-deb -f "$deb" Package Version Architecture | tr '\n' ' ')"

# Two builds of one commit: the same bytes, from a copy of the tree with other
# times and permissions.
cp -r "$tree" "$tmp/tree2"
find "$tmp/tree2" -exec touch -d '2001-02-03 04:05:06' {} +
chmod 775 "$tmp/tree2/locales"
CONCH_RELEASE_VERSION=$tag build "$tmp/tree2" "$tmp/b"
if ! cmp -s "$deb" "$tmp/b/$(basename "$deb")"; then
    diff <(dpkg-deb -c "$deb") <(dpkg-deb -c "$tmp/b/$(basename "$deb")") || true
    diff <(dpkg-deb -I "$deb") <(dpkg-deb -I "$tmp/b/$(basename "$deb")") || true
    fail "two builds of the same tree are not byte-identical"
fi
echo "desktop-deb-test: two builds are byte-identical"

# A development package builds too, and is a valid one.
# Its stamp is the development version scripts/desktop-build.sh writes.
cp -r "$tree" "$tmp/devtree"
printf '0.0.0-dev+%s\n' "$(git rev-parse --short HEAD 2>/dev/null || echo nogit)" >"$tmp/devtree/resources/version"
build "$tmp/devtree" "$tmp/dev"
dev=$(ls "$tmp/dev"/conch-desktop_0.0.0~dev+*_amd64.deb) || fail "no development package"
check_deb "$dev" >"$tmp/out" 2>&1 || { sed 's/^/  /' "$tmp/out" >&2; fail "the development package fails its checks"; }

# Each check can fail: break the good package one way at a time and require the
# right complaint.
good=$tmp/good
dpkg-deb -R "$deb" "$good"
broken=0
break_it() { # break_it <name> <want text> <root-owner? 1|0> <mutation function>
    local name=$1 want=$2 rootown=$3 mut=$4 d=$tmp/broken-$1
    rm -rf "$d"
    cp -a "$good" "$d"
    "$mut" "$d"
    local flags=(-Zgzip)
    [ "$rootown" = 1 ] && flags+=(--root-owner-group)
    dpkg-deb "${flags[@]}" --build "$d" "$tmp/broken-$name.deb" >/dev/null 2>&1 || fail "cannot build the broken package '$name'"
    if check_deb "$tmp/broken-$name.deb" >"$tmp/out" 2>&1; then
        fail "check passed a package with: $name"
    fi
    grep -q -e "$want" "$tmp/out" || { cat "$tmp/out" >&2; fail "check failed '$name' but did not say '$want'"; }
    broken=$((broken + 1))
}
m_none() { :; }
m_gw() { chmod 775 "$1/opt/conch-desktop/locales"; }
m_ow() { chmod 666 "$1/opt/conch-desktop/LICENSE"; }
m_suid() { chmod 4755 "$1/opt/conch-desktop/conch-desktop"; }
m_sgid() { chmod 2755 "$1/opt/conch-desktop/conch-desktop"; }
m_udev() { mkdir -p "$1/etc/udev/rules.d" && echo 'KERNEL=="event*", MODE="0660"' >"$1/etc/udev/rules.d/70-conch.rules"; }
m_etc() { echo x >"$1/etc/conch.conf"; }
m_nohelper() { rm "$1/opt/conch-desktop/resources/conch-voice"; }
m_nonexec() { chmod 644 "$1/opt/conch-desktop/resources/conch-voice"; }
m_baddesktop() { sed -i 's/^Type=.*/Type=Bogus/' "$1/usr/share/applications/chat.conch.desktop.desktop"; }
m_wmclass() { sed -i 's/^StartupWMClass=.*/StartupWMClass=Other/' "$1/usr/share/applications/chat.conch.desktop.desktop"; }
m_nosandbox() { printf '#!/bin/sh\nexec conch-desktop --no-sandbox\n' >"$1/opt/conch-desktop/run.sh"; chmod 755 "$1/opt/conch-desktop/run.sh"; }
m_nosandbox_post() { echo '# --no-sandbox' >>"$1/DEBIAN/postinst"; }
m_post_group() { echo 'adduser nick input' >>"$1/DEBIAN/postinst"; }
m_post_setcap() { echo 'setcap cap_dac_override+ep /opt/conch-desktop/conch-desktop' >>"$1/DEBIAN/postinst"; }
m_nodeps() { sed -i '/^Depends:/d' "$1/DEBIAN/control"; }
m_emptydeps() { sed -i 's/^Depends:.*/Depends: /' "$1/DEBIAN/control"; }
m_arch() { sed -i 's/^Architecture:.*/Architecture: arm64/' "$1/DEBIAN/control"; }
m_stray() { mkdir -p "$1/usr/lib/stray" && echo x >"$1/usr/lib/stray/x"; }
m_nostamp() { rm "$1/opt/conch-desktop/resources/version"; }
m_stampmismatch() { printf '0.1.0-alpha.19\n' >"$1/opt/conch-desktop/resources/version"; }
m_stampmode() { chmod 600 "$1/opt/conch-desktop/resources/version"; }
m_nolink() { rm "$1/usr/bin/conch-desktop"; }

break_it group-writable "group-writable" 1 m_gw
break_it other-writable "other-writable" 1 m_ow
break_it setuid "setuid" 1 m_suid
break_it setgid "setgid" 1 m_sgid
break_it udev-rule "udev" 1 m_udev
break_it etc-file "in /etc beyond" 1 m_etc
break_it no-helper "missing /opt/conch-desktop/resources/conch-voice" 1 m_nohelper
break_it helper-not-executable "not -rwxr-xr-x" 1 m_nonexec
break_it no-stamp "missing /opt/conch-desktop/resources/version" 1 m_nostamp
break_it stamp-of-another-version "which is not the package's Version" 1 m_stampmismatch
break_it stamp-not-0644 "resources/version is" 1 m_stampmode
break_it no-launcher-link "not a link" 1 m_nolink
if [ "$have_validator" = 1 ]; then
    break_it bad-desktop-file "desktop-file-validate" 1 m_baddesktop
fi
break_it wm-class "StartupWMClass" 1 m_wmclass
break_it no-sandbox-in-app "--no-sandbox in" 1 m_nosandbox
break_it no-sandbox-in-postinst "--no-sandbox in" 1 m_nosandbox_post
break_it postinst-group-change "DEBIAN/postinst" 1 m_post_group
break_it postinst-setcap "DEBIAN/postinst" 1 m_post_setcap
break_it no-depends "Depends is empty" 1 m_nodeps
break_it empty-depends "Depends is empty" 1 m_emptydeps
break_it wrong-arch "Architecture" 1 m_arch
break_it stray-path "outside the package" 1 m_stray
break_it not-root-owned "not owned by root:root" 0 m_none

echo "desktop-deb-test: $broken broken packages, each refused for the right reason"
echo "desktop-deb-test: OK"
