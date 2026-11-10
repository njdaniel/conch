#!/usr/bin/env bash
# The release job's guard (issue #1598, D6-04 of #774; the review of #1617).
# A release build is stamped with a tag (CONCH_RELEASE_VERSION), and nothing
# else ties that tag to the files being packed: v9.9.9 would build a "release"
# of any commit or of a dirty tree. This refuses unless all of these hold:
#
#   - the tag is a release tag (scripts/desktop-version.sh's rules);
#   - HEAD is exactly that tag (`git describe --tags --exact-match`), so the
#     tag names the commit being built, not an older or newer one;
#   - the working tree is clean: no modified, staged or untracked file, so
#     nothing was added to the checkout after it was made;
#   - the voice helper's version, [workspace.package] `version` in
#     voice/Cargo.toml, is the tag's app version. This is the early form of
#     the check scripts/desktop-version.sh helper makes on the built binary:
#     it fails in seconds, before a twenty-minute Rust build, with the fix.
#
# usage: desktop-release-guard.sh <tag>
# DESKTOP_GUARD_ROOT moves the repository root, for the script's test.
set -euo pipefail

fail() {
    echo "desktop-release-guard: FAIL: $*" >&2
    exit 1
}

[ $# -eq 1 ] || fail "usage: desktop-release-guard.sh <tag>"
tag=$1

here=$(cd "$(dirname "$0")" && pwd)
root=${DESKTOP_GUARD_ROOT:-$(cd "$here/.." && pwd)}

app=$("$here/desktop-version.sh" app "$tag") || exit 1

cd "$root"

# --tags: describe looks at annotated tags only without it, and a lightweight
# one must still name the commit. The tag it finds must be this tag: another
# tag on the same commit is not the one being released.
described=$(git describe --tags --exact-match HEAD 2>/dev/null) ||
    fail "HEAD is not at a tag: $tag must point at the commit being built"
[ "$described" = "$tag" ] || {
    # More than one tag may point at HEAD; accept this one among them.
    git tag --points-at HEAD | grep -qxF -- "$tag" ||
        fail "HEAD is tagged $described, not $tag"
}

dirty=$(git status --porcelain --untracked-files=all)
[ -z "$dirty" ] || fail "the working tree is not clean:
$dirty"

cargo=voice/Cargo.toml
[ -f "$cargo" ] || fail "$cargo is missing"
# The version line of the [workspace.package] table, and no other.
got=$(awk '
    /^\[/ { in_pkg = ($0 == "[workspace.package]"); next }
    in_pkg && /^version[ \t]*=/ {
        v = $0
        sub(/^version[ \t]*=[ \t]*"/, "", v)
        sub(/".*$/, "", v)
        print v
        exit
    }
' "$cargo")
[ -n "$got" ] || fail "no version in the [workspace.package] table of $cargo"
[ "$got" = "$app" ] ||
    fail "$cargo says version $got, but the tag $tag is $app; set [workspace.package] version to $app on main before tagging (.claude/skills/release/SKILL.md)"

echo "desktop-release-guard: OK: $tag is HEAD ($(git rev-parse --short HEAD)), the tree is clean and the helper version is $app"
