#!/usr/bin/env bash
# The one place a release tag becomes the desktop app's versions (issue #1596,
# D6-02 of #774; decision Q-D6-6 of #1595: the release tag is the version).
# Used by scripts/desktop-build.sh, and by the .deb packaging (#1597, #1598).
#
# usage:
#   desktop-version.sh app    <tag>            the app's version: v0.1.0-alpha.18 -> 0.1.0-alpha.18
#   desktop-version.sh deb    <tag>            the Debian version: v0.1.0-alpha.18 -> 0.1.0~alpha.18
#   desktop-version.sh helper <tag> <binary>   fails unless <binary> --version names the app version
#
# A tag is `v<major>.<minor>.<patch>` with an optional `-<prerelease>` of
# dot-separated [0-9A-Za-z-] identifiers (no build metadata, no leading zeros
# in the numbers). Anything else is one line on stderr and exit 1.
#
# The Debian mapping: the first `-` becomes `~`, so a pre-release sorts before
# its release under dpkg (0.1.0~alpha.9 < 0.1.0~alpha.18 < 0.1.0); any later
# `-` becomes `.`, because a Debian version without a revision may hold no `-`.
#
# The helper (conch-voice) prints `conch-voice <version> (...)`; <version> must
# be the app version exactly. The helper takes it from the workspace version in
# voice/Cargo.toml, so a release build must have that set to the tag's version.
set -euo pipefail

fail() {
    echo "desktop-version: FAIL: $*" >&2
    exit 1
}

num='(0|[1-9][0-9]*)'
ident='[0-9A-Za-z-]+'
tag_re="^v$num\\.$num\\.$num(-$ident(\\.$ident)*)?\$"

# check_tag <tag>: succeeds only for a well-formed release tag.
check_tag() {
    [[ $1 =~ $tag_re ]] || fail "'$1' is not a release tag (want v<major>.<minor>.<patch>[-<prerelease>], for example v0.1.0-alpha.18)"
}

# deb_version <app version>: the Debian spelling of a semver.
deb_version() {
    local v=$1
    if [[ $v == *-* ]]; then
        local base=${v%%-*} pre=${v#*-}
        v="$base~${pre//-/.}"
    fi
    printf '%s\n' "$v"
}

[ $# -ge 2 ] || fail "usage: desktop-version.sh app|deb <tag> | helper <tag> <binary>"
cmd=$1
tag=$2
check_tag "$tag"
app=${tag#v}

case $cmd in
app)
    [ $# -eq 2 ] || fail "usage: desktop-version.sh app <tag>"
    printf '%s\n' "$app"
    ;;
deb)
    [ $# -eq 2 ] || fail "usage: desktop-version.sh deb <tag>"
    deb_version "$app"
    ;;
helper)
    [ $# -eq 3 ] || fail "usage: desktop-version.sh helper <tag> <binary>"
    bin=$3
    [ -x "$bin" ] || fail "$bin is not an executable file"
    line=$(env -i "$bin" --version 2>&1) || fail "$bin --version failed: $line"
    read -r name got _ <<<"$line"
    [ "$name" = conch-voice ] && [ "$got" = "$app" ] ||
        fail "the bundled helper says '$line', not conch-voice $app (the tag $tag); set the workspace version in voice/Cargo.toml to $app and rebuild"
    ;;
*)
    fail "unknown command '$cmd' (app, deb or helper)"
    ;;
esac
