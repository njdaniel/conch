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
#   desktop-deb-lifecycle.sh --broken=userdata  the same fixture packages with a
#                                               postinst that appends to the
#                                               seeded settings.json in the test
#                                               user's data folder must FAIL at
#                                               the user-data check of the
#                                               upgrade (so that check can fail)
#
# The user's data folder (issue #1800) is where the app keeps its data:
# Electron's app.getPath('userData'), ~/.config/<app name> on Linux, where the
# app name is the productName of the package.json packed in
# resources/app.asar, or its name when it has none. The script reads that
# package.json from each .deb's app.asar on the host (python3) and fails unless
# both packages name the same folder and it is conch-desktop, the folder
# docs/desktop/linux.md tells users about: a rename would move every user's
# data, so it must fail here and be decided, not be followed silently. The
# fixture packages carry a small app.asar holding desktop/package.json with the
# electron-builder config's extraMetadata over it, as electron-builder packs it.
#
# What it checks, each failure one line naming the step and the file or library:
#   1 install <old>   `apt install ./old.deb` in a container with nothing of
#                     ours (apt resolves Depends from the archive): the files,
#                     their owners and modes (root-owned, nothing group- or
#                     other-writable, nothing setuid), the launcher link, the
#                     desktop file with MimeType=x-scheme-handler/conch;, the
#                     icons, `xdg-mime query default x-scheme-handler/conch`
#                     once xdg-utils is there (xdg-mime missing then is a
#                     failure, not a skip), postinst (it ran with no AppArmor in
#                     the container and did not fail; run again, it still
#                     succeeds), and `diff -r` of the installed /opt/conch-desktop
#                     against the .deb's own opt/conch-desktop (dpkg-deb -x)
#   2 seed            six files in a test user's ~/.config/conch-desktop, as the
#                     app keeps them: profiles.json, settings.json,
#                     profiles/p1/credential.bin (random bytes),
#                     profiles/p1/voice.toml and two files under
#                     "Local Storage/leveldb"; their SHA-256 sums are kept
#   3 upgrade <new>   `apt install ./new.deb`: the version is the higher one, the
#                     installed tree is the new .deb's tree (`diff -r` against its
#                     dpkg-deb -x; the stamp resources/version is not compared to
#                     the Debian version, which spells a pre-release with ~), no
#                     file of the old package that the new one drops is left,
#                     the user's data folder is there and every seeded file
#                     has its seeded SHA-256, and no file was added
#   4 ldd             every ELF file of the package, executable or not (.so files
#                     of mode 0644 too), resolves every library, after the
#                     install and after the upgrade. A .so may name a sibling
#                     library through $ORIGIN or its loader's search path: for
#                     a .so file only, a "not found" entry whose file name exists
#                     inside the package tree is accepted; every other file is
#                     checked with no allowance. With real packages the run also
#                     requires that /opt/conch-desktop/conch-desktop and
#                     resources/conch-voice were among the files checked.
#   5 helper          resources/conch-voice passes the trust rules of findHelper
#                     (desktop/main/voice/spawn.ts) as a script: a regular file,
#                     not a link, executable, root-owned, not group- or
#                     other-writable, inside resources/, and every directory
#                     above it root-owned and closed to group and others
#   6 remove, purge   remove: nothing of the package is left in /opt or /usr
#                     (the AppArmor profile, a conffile, stays until purge, as
#                     dpkg keeps conffiles); purge: nothing in /opt, /usr or
#                     /etc, the package is gone from dpkg; after each, the
#                     user's data folder is checked as after the upgrade
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
#
# The fixture packages use the host's own `curl` as the ELF stand-in for the
# app, and dpkg-shlibdeps on the host computes their Depends. So the fixture
# run needs a host no newer than Ubuntu 24.04 with curl installed (a newer
# host's curl needs a newer glibc than the container has and fails at the ldd
# check). Real packages (<old> <new>) have no such limit.
set -euo pipefail

# ubuntu:24.04, the multi-arch index digest, as pulled on 2026-11-10.
image=${CONCH_LIFECYCLE_IMAGE:-ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852}

# --- inside the container ----------------------------------------------------
# The script runs itself in the container as `--inside <old> <new>`, with the
# packages in /debs. The host sets CONCH_LIFECYCLE_INSIDE=1 (and
# CONCH_LIFECYCLE_REAL=1 for real packages); --inside refuses to run anywhere
# else, as it creates a user, installs packages and purges them. step is the
# current step's name for the failure line.
step=start
die() {
    echo "desktop-deb-lifecycle: FAIL: step $step: $*"
    exit 1
}

app=/opt/conch-desktop
# The user's data folder, named by the host from the packed package.json.
data_dir=${CONCH_LIFECYCLE_DATA_DIR:-}
cfg=/home/tester/.config/$data_dir
# shellcheck disable=SC2088 # the label is for messages, not a path to expand
label="~/.config/$data_dir"
seeded=

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
    # existence first, so that find below never meets a missing path
    [ -d "$app" ] || die "$app is missing"
    [ -L /usr/bin/conch-desktop ] || die "/usr/bin/conch-desktop is not a link"
    [ -f /etc/apparmor.d/conch-desktop ] || die "/etc/apparmor.d/conch-desktop is missing"
    f=/usr/share/applications/chat.conch.desktop.desktop
    [ -f "$f" ] || die "$f is missing"
    bad=$(find "$app" /usr/bin/conch-desktop /etc/apparmor.d/conch-desktop "$f" -not -type l \( ! -user root -o ! -group root \) -print -quit) || die "find over $app failed"
    [ -z "$bad" ] || die "$bad is not owned by root:root"
    bad=$(find "$app" -perm /022 -print -quit) || die "find over $app failed"
    [ -z "$bad" ] || die "$bad is group- or other-writable"
    bad=$(find "$app" -perm /6000 -print -quit) || die "find over $app failed"
    [ -z "$bad" ] || die "$bad is setuid or setgid"
    [ ! -e "$app/chrome-sandbox" ] || die "$app/chrome-sandbox is installed; the AppArmor profile is the sandbox's answer"
    [ -x "$app/conch-desktop" ] || die "$app/conch-desktop is missing or not executable"
    [ "$(readlink /usr/bin/conch-desktop)" = "$app/conch-desktop" ] || die "/usr/bin/conch-desktop points to $(readlink /usr/bin/conch-desktop), not $app/conch-desktop"
    [ -x /usr/bin/conch-desktop ] || die "/usr/bin/conch-desktop does not resolve to an executable"
    [ -f "$app/resources/version" ] || die "$app/resources/version is missing"
    grep -qx 'MimeType=x-scheme-handler/conch;' "$f" || die "$f has no 'MimeType=x-scheme-handler/conch;' line"
    grep -qx 'Exec=conch-desktop %U' "$f" || die "$f has not 'Exec=conch-desktop %U'"
    for size in 48 128 256 512; do
        f=/usr/share/icons/hicolor/${size}x${size}/apps/chat.conch.desktop.png
        [ -f "$f" ] || die "$f is missing"
    done
    [ "$(dpkg-query -W -f '${Status}' conch-desktop 2>/dev/null)" = 'install ok installed' ] || die "dpkg does not list conch-desktop as installed"
}

# check_ldd: every ELF file of the package, executable or not, resolves its
# libraries. The files it checked are kept in $elf_checked for require_checked.
elf_checked=
check_ldd() {
    local f out n=0 missing lib list
    list=$(find "$app" -type f -print0 | LC_ALL=C sort -z | tr '\0' '\n') || die "find over $app failed"
    elf_checked=
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        [ "$(head -c4 "$f" | od -An -tx1 | tr -d ' \n')" = 7f454c46 ] || continue
        n=$((n + 1))
        elf_checked+="$f"$'\n'
        out=$(ldd "$f" 2>&1) || {
            # a static executable has nothing to resolve
            case $out in *"not a dynamic executable"*) continue ;; esac
            die "ldd $f failed: $out"
        }
        while IFS= read -r missing; do
            [ -n "$missing" ] || continue
            lib=${missing%%=>*}
            lib=${lib//[[:space:]]/}
            case $f in
            *.so | *.so.*)
                # a .so may find a sibling in the tree through $ORIGIN or the
                # loader path of the binary that loads it: accept the entry
                # when a file of that name exists inside the tree
                [ -n "$(find "$app" -name "$lib" -print -quit)" ] && continue
                ;;
            esac
            die "ldd $f: library not found: $lib"
        done < <(printf '%s\n' "$out" | grep 'not found' || true)
    done <<<"$list"
    [ "$n" -gt 0 ] || die "no ELF file under $app"
    echo "  ldd: $n ELF file(s) resolve every library"
}

# require_checked <path>...: each file was among the ELF files check_ldd checked.
require_checked() {
    local p
    for p in "$@"; do
        printf '%s' "$elf_checked" | grep -qxF "$p" || die "$p was not among the ELF files that ldd checked (is it an ELF file?)"
    done
}

# check_tree <dir> <label>: the installed /opt/conch-desktop is the package's own
# tree, the one dpkg-deb -x extracted into <dir>. Links are compared as links.
check_tree() {
    local out status=0
    out=$(diff -rq --no-dereference "$1/opt/conch-desktop" "$app" 2>&1) || status=$?
    [ "$status" -eq 0 ] || die "the installed tree is not the tree of $2: $(printf '%s\n' "$out" | sed -n 1p)"
    echo "  tree: $app is the tree of $2"
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

# user_sums: the checksum and name of every file in the user's data folder.
user_sums() { (cd "$cfg" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 -r sha256sum); }
# user_files: the name of every file in it, sorted.
user_files() { (cd "$cfg" && find . -type f | LC_ALL=C sort); }

# check_user_data <when>: the data folder is there, every seeded file is there
# with its seeded SHA-256, and no file was added. $seeded is the seed's
# user_sums. Each failure names the file.
check_user_data() {
    local now line f extra
    [ -d "$cfg" ] || die "the user's $label is gone after $1"
    now=$(user_sums) || die "cannot read the user's $label after $1"
    while IFS= read -r line; do
        f=${line#*  }
        f=${f#./}
        [ -f "$cfg/$f" ] || die "the user's $label/$f is gone after $1"
        printf '%s\n' "$now" | grep -qxF "$line" || die "the user's $label/$f changed in $1 (its SHA-256 is not the seeded one)"
    done <<<"$seeded"
    extra=$(comm -13 <(printf '%s\n' "$seeded" | sed 's/^[^ ]*  //' | LC_ALL=C sort) <(user_files) | sed -n 1p)
    [ -z "$extra" ] || die "the user's $label/${extra#./} is new after $1 (the package wrote into the user's data)"
    echo "  user data: the $(printf '%s\n' "$seeded" | wc -l) seeded file(s) of $label are unchanged after $1"
}

inside() {
    local old=$1 new=$2 oldv newv list_old stale p got n
    export DEBIAN_FRONTEND=noninteractive
    step="0 prepare"
    case $data_dir in
    '' | . | .. | */* | *[!A-Za-z0-9._-]*) die "CONCH_LIFECYCLE_DATA_DIR '$data_dir' is not a folder name" ;;
    esac
    # _apt must read the packages (an unreadable file makes apt fall back to root).
    mkdir -p /var/tmp/pkg /var/tmp/tree-old /var/tmp/tree-new
    chmod 755 /var/tmp/pkg
    cp "/debs/$old" "/debs/$new" /var/tmp/pkg/
    chmod 644 /var/tmp/pkg/*.deb
    apt-get update -q >/var/tmp/apt-update.log 2>&1 || { tail -n 10 /var/tmp/apt-update.log; die "apt-get update failed (apt needs network)"; }
    oldv=$(dpkg-deb -f "/var/tmp/pkg/$old" Version)
    newv=$(dpkg-deb -f "/var/tmp/pkg/$new" Version)
    dpkg --compare-versions "$newv" gt "$oldv" || die "$new has version $newv, not higher than $old's $oldv"
    useradd -m tester
    # the .deb's own trees, to compare the installed ones with
    dpkg-deb -x "/var/tmp/pkg/$old" /var/tmp/tree-old
    dpkg-deb -x "/var/tmp/pkg/$new" /var/tmp/tree-new

    step="1 install $oldv"
    apt_install "/var/tmp/pkg/$old"
    [ "$(dpkg-query -W -f '${Version}' conch-desktop)" = "$oldv" ] || die "installed version is not $oldv"
    check_files
    check_tree /var/tmp/tree-old "$old ($oldv)"
    [ "$(cat "$app/resources/version")" != "" ] || die "$app/resources/version is empty"
    check_postinst
    list_old=$(dpkg -L conch-desktop | sort)
    echo "  installed $oldv"
    step="1 install $oldv (desktop database)"
    apt-get install -y -q xdg-utils desktop-file-utils >/var/tmp/apt-tools.log 2>&1 || { tail -n 10 /var/tmp/apt-tools.log; die "apt install xdg-utils failed"; }
    update-desktop-database -q /usr/share/applications
    command -v xdg-mime >/dev/null 2>&1 || die "xdg-mime is missing after xdg-utils was installed"
    got=$(su tester -c 'xdg-mime query default x-scheme-handler/conch' 2>/dev/null || true)
    [ "$got" = chat.conch.desktop.desktop ] || die "xdg-mime query default x-scheme-handler/conch is '$got', not chat.conch.desktop.desktop"
    echo "  xdg-mime: x-scheme-handler/conch -> $got"
    desktop-file-validate /usr/share/applications/chat.conch.desktop.desktop || die "desktop-file-validate rejects chat.conch.desktop.desktop"
    step="4 ldd (after install)"
    check_ldd
    require_checked "$app/conch-desktop"
    [ -z "${CONCH_LIFECYCLE_REAL:-}" ] || require_checked "$app/resources/conch-voice"
    step="5 helper (after install)"
    check_helper

    step="2 seed"
    [ ! -e "$cfg" ] || die "$cfg exists before the seed"
    mkdir -p "$cfg/profiles/p1" "$cfg/Local Storage/leveldb"
    printf '{"version":1,"profiles":[{"id":"p1","name":"Work","server":"https://chat.example.test"}],"active":"p1"}\n' >"$cfg/profiles.json"
    printf '{"theme":"dark","pushToTalk":{"key":"F13"}}\n' >"$cfg/settings.json"
    head -c 512 /dev/urandom >"$cfg/profiles/p1/credential.bin"
    printf '[talk]\nkey = "F13"\n' >"$cfg/profiles/p1/voice.toml"
    printf 'fixture leveldb log\n' >"$cfg/Local Storage/leveldb/000003.log"
    printf 'MANIFEST-000001\n' >"$cfg/Local Storage/leveldb/CURRENT"
    chown -R tester:tester /home/tester/.config
    chmod 700 /home/tester/.config
    find "$cfg" -type d -exec chmod 700 {} +
    find "$cfg" -type f -exec chmod 600 {} +
    seeded=$(user_sums)
    n=$(printf '%s\n' "$seeded" | wc -l)
    [ "$n" -eq 6 ] || die "the seed made $n file(s) in $label, not 6"
    echo "  seeded $n file(s) in $label"

    step="3 upgrade $oldv -> $newv"
    apt_install "/var/tmp/pkg/$new"
    [ "$(dpkg-query -W -f '${Version}' conch-desktop)" = "$newv" ] || die "installed version is not $newv"
    check_files
    check_postinst
    check_tree /var/tmp/tree-new "$new ($newv)"
    check_user_data "the upgrade"
    # what the old package had and the new one drops is gone; /opt is replaced.
    stale=
    while IFS= read -r p; do
        [ -n "$p" ] || continue
        if [ -e "$p" ] || [ -L "$p" ]; then
            if [ -d "$p" ] && [ ! -L "$p" ]; then continue; fi
            stale=$p
            break
        fi
    done < <(comm -23 <(printf '%s\n' "$list_old") <(dpkg -L conch-desktop | sort))
    [ -z "$stale" ] || die "$stale of the old package is left after the upgrade"
    echo "  upgraded to $newv"
    step="4 ldd (after upgrade)"
    check_ldd
    require_checked "$app/conch-desktop"
    [ -z "${CONCH_LIFECYCLE_REAL:-}" ] || require_checked "$app/resources/conch-voice"
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
    check_user_data "the remove"

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
    check_user_data "the purge"
    echo "desktop-deb-lifecycle: container: OK (install, upgrade, remove, purge)"
}

if [ "${1:-}" = --inside ]; then
    if [ "${CONCH_LIFECYCLE_INSIDE:-}" != 1 ] || [ ! -e /.dockerenv ]; then
        echo "desktop-deb-lifecycle: --inside runs only in the container that this script starts (it adds a user and purges packages)" >&2
        exit 1
    fi
    inside "$2" "$3"
    exit 0
fi

# --- on the host -------------------------------------------------------------
fail() {
    echo "desktop-deb-lifecycle: FAIL: $*" >&2
    exit 1
}

# The folder the app's data is in: what docs/desktop/linux.md tells users.
expected_data_dir=conch-desktop

# asar_name <app.asar>: the name Electron gives the app, from the package.json
# packed in the asar: its productName, or its name when it has none. On Linux
# app.getPath('userData') is ~/.config/<that name>. The asar format: a 4-byte
# pickle whose value is the header pickle's size, then the header pickle (its
# payload size, the JSON's length, the JSON), then the files, at the offsets
# the JSON gives, counted from the end of the header.
asar_name() {
    python3 -I - "$1" <<'PY'
import json, struct, sys

path = sys.argv[1]


def fail(msg):
    sys.exit(f"{path}: {msg}")


with open(path, "rb") as f:
    data = f.read()
if len(data) < 16:
    fail("is not an asar archive (too short)")
size_len, header_size = struct.unpack_from("<II", data, 0)
_, json_len = struct.unpack_from("<II", data, 8)
if size_len != 4 or 16 + json_len > len(data) or 8 + header_size > len(data):
    fail("is not an asar archive")
try:
    header = json.loads(data[16 : 16 + json_len])
except ValueError:
    fail("is not an asar archive (its header is not JSON)")
entry = header.get("files", {}).get("package.json") if isinstance(header, dict) else None
if not isinstance(entry, dict) or "offset" not in entry or "size" not in entry or entry.get("unpacked"):
    fail("has no package.json packed at its top")
start = 8 + header_size + int(entry["offset"])
end = start + int(entry["size"])
if end > len(data):
    fail("is cut short (its package.json runs past the end)")
try:
    pkg = json.loads(data[start:end])
except ValueError:
    fail("has a package.json that is not JSON")
name = pkg.get("productName") or pkg.get("name") if isinstance(pkg, dict) else None
if not isinstance(name, str) or not name:
    fail("has a package.json with no productName and no name")
print(name)
PY
}

# fixture_asar <out> <version>: an asar holding one package.json, packed as
# electron-builder packs desktop/package.json: the config's extraMetadata over
# it, the version set, scripts and devDependencies left out.
fixture_asar() {
    python3 -I - desktop/package.json desktop/build/electron-builder.config.json "$2" "$1" <<'PY'
import json, struct, sys

pkg_path, cfg_path, version, out = sys.argv[1:5]
with open(pkg_path) as f:
    pkg = json.load(f)
with open(cfg_path) as f:
    cfg = json.load(f)
pkg.update(cfg.get("extraMetadata") or {})
pkg["version"] = version
for key in ("scripts", "devDependencies"):
    pkg.pop(key, None)
body = (json.dumps(pkg, indent=2) + "\n").encode()
header = json.dumps({"files": {"package.json": {"size": len(body), "offset": "0"}}}, separators=(",", ":")).encode()
payload = struct.pack("<I", len(header)) + header + b"\0" * (-len(header) % 4)
pickle = struct.pack("<I", len(payload)) + payload
with open(out, "wb") as f:
    f.write(struct.pack("<II", 4, len(pickle)) + pickle + body)
PY
}

broken=
case "${1:-}" in
--broken) broken=ldd; shift ;;
--broken=ldd | --broken=userdata) broken=${1#--broken=}; shift ;;
esac
[ $# -eq 0 ] || [ $# -eq 2 ] || fail "usage: desktop-deb-lifecycle.sh [<old.deb> <new.deb> | --broken[=ldd|=userdata]]"
[ -z "$broken" ] || [ $# -eq 0 ] || fail "--broken takes no package"

# Package paths are resolved before the cd below.
real=0
if [ $# -eq 2 ]; then
    if ! [ -f "$1" ] || ! [ -f "$2" ]; then fail "$1 and $2 must be package files"; fi
    pkg_old=$(realpath "$1")
    pkg_new=$(realpath "$2")
    real=1
fi

cd "$(dirname "$0")/.."
root=$PWD

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
command -v python3 >/dev/null 2>&1 || fail "python3 is not installed (it reads the package.json packed in each .deb's app.asar)"

work=$(mktemp -d)
cname=conch-deb-lifecycle-$$
trap 'docker rm -f "$cname" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
mkdir "$work/debs"

if [ "$real" = 1 ]; then
    cp "$pkg_old" "$work/debs/old.deb"
    cp "$pkg_new" "$work/debs/new.deb"
else
    # A fixture tree: a real dynamically linked ELF file as the app (the host's
    # curl, which needs libcurl from the archive, so a missing Depends shows;
    # the header says which hosts work), a shell script as the helper (it prints the tag's version) and,
    # in the old tree only, a file the new package drops.
    elf=$(command -v curl) || fail "curl is not installed; the fixture packages use it as the ELF stand-in"
    mk_tree() { # mk_tree <dir> <tag>
        mkdir -p "$1/resources" "$1/locales"
        cp "$elf" "$1/conch-desktop"
        printf '#!/bin/sh\necho "conch-voice %s (fixture)"\n' "${2#v}" >"$1/resources/conch-voice"
        chmod 755 "$1/resources/conch-voice"
        fixture_asar "$1/resources/app.asar" "${2#v}" || fail "cannot write the fixture app.asar"
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
fi

# The user's data folder, from the package.json packed in each package's
# app.asar (the header says why). Both must name the same folder, and it must
# be the documented one.
# deb_app_name <old|new>: asar_name of that package's app.asar.
deb_app_name() {
    dpkg-deb --fsys-tarfile "$work/debs/$1.deb" | tar -xOf - ./opt/conch-desktop/resources/app.asar >"$work/$1.asar" ||
        fail "the $1 package has no /opt/conch-desktop/resources/app.asar"
    asar_name "$work/$1.asar" || fail "cannot read the package.json packed in the $1 package's app.asar"
}
data_dir_old=$(deb_app_name old)
data_dir_new=$(deb_app_name new)
[ "$data_dir_old" = "$data_dir_new" ] ||
    fail "the old package's app is named '$data_dir_old' and the new one's '$data_dir_new': the upgrade would leave every user's data behind in ~/.config/$data_dir_old"
data_dir=$data_dir_new
[ "$data_dir" = "$expected_data_dir" ] ||
    fail "the package.json packed in app.asar names the app '$data_dir', so its data is in ~/.config/$data_dir, not ~/.config/$expected_data_dir (docs/desktop/linux.md): a rename moves every user's data. Decide it, migrate it, and update the docs and this script"
echo "desktop-deb-lifecycle: the app's data folder is ~/.config/$data_dir (from the package.json packed in app.asar)"

if [ "$real" = 0 ]; then
    if [ -n "$broken" ]; then
        for n in old new; do
            dpkg-deb -R "$work/debs/$n.deb" "$work/x-$n"
            if [ "$broken" = ldd ]; then
                # Remove Depends from both packages.
                grep -v '^Depends:' "$work/x-$n/DEBIAN/control" >"$work/x-$n/control.new"
                mv "$work/x-$n/control.new" "$work/x-$n/DEBIAN/control"
                chmod 644 "$work/x-$n/DEBIAN/control"
            else
                # postinst appends to the seeded settings.json in the test
                # user's data folder, once it is there (so at the upgrade).
                sed "/^set -e\$/a\\
f=/home/tester/.config/$data_dir/settings.json; if [ -f \"\$f\" ]; then date +%s%N >>\"\$f\"; fi" "$work/x-$n/DEBIAN/postinst" >"$work/x-$n/postinst.new"
                grep -q 'settings.json' "$work/x-$n/postinst.new" || fail "cannot add the user-data write to the fixture postinst"
                mv "$work/x-$n/postinst.new" "$work/x-$n/DEBIAN/postinst"
                chmod 755 "$work/x-$n/DEBIAN/postinst"
            fi
            dpkg-deb --root-owner-group -Zxz --build "$work/x-$n" "$work/debs/$n.deb" >/dev/null
        done
    fi
fi

echo "desktop-deb-lifecycle: image $image"
docker pull -q "$image" >/dev/null || fail "cannot pull $image (this needs network)"

real_env=
if [ "$real" = 1 ]; then real_env=1; fi
# The names inside are fixed: old.deb and new.deb.
set +e
docker run --rm --name "$cname" -e CONCH_LIFECYCLE_INSIDE=1 -e "CONCH_LIFECYCLE_REAL=$real_env" \
    -e "CONCH_LIFECYCLE_DATA_DIR=$data_dir" \
    -v "$work/debs:/debs:ro" -v "$root/scripts/desktop-deb-lifecycle.sh:/lifecycle.sh:ro" \
    "$image" bash /lifecycle.sh --inside old.deb new.deb >"$work/out" 2>&1
status=$?
set -e
cat "$work/out"

if [ "$broken" = ldd ]; then
    if [ "$status" -ne 0 ] && grep -q 'FAIL: step 4 ldd (after install): ldd .*library not found' "$work/out"; then
        echo "desktop-deb-lifecycle: OK: a package without Depends fails at the ldd check, as it must"
        exit 0
    fi
    fail "the package without Depends did not fail at the ldd check (the test cannot catch a missing Depends)"
fi
if [ "$broken" = userdata ]; then
    if [ "$status" -ne 0 ] && grep -qF "FAIL: step 3 upgrade " "$work/out" && grep -qF ": the user's ~/.config/$data_dir/settings.json changed in the upgrade" "$work/out"; then
        echo "desktop-deb-lifecycle: OK: a postinst that writes under ~/.config/$data_dir fails at the user-data check, as it must"
        exit 0
    fi
    fail "the package whose postinst writes under ~/.config/$data_dir did not fail at the user-data check (the test cannot catch a changed user file)"
fi
[ "$status" -eq 0 ] || fail "the container run failed (see above)"
echo "desktop-deb-lifecycle: OK (the app was not run; #1602 covers the running app)"
