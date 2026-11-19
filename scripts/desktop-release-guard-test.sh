#!/usr/bin/env bash
# Tests of scripts/desktop-release-guard.sh (issue #1598): a throwaway
# repository stands in for the checkout. Run by `make desktop-check`.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
guard=$PWD/scripts/desktop-release-guard.sh

fail() {
    echo "desktop-release-guard-test: FAIL: $*"
    exit 1
}

repo=$tmp/repo
mkdir -p "$repo/voice"
cat >"$repo/voice/Cargo.toml" <<'TOML'
[workspace]
members = ["a"]

[workspace.package]
version = "1.2.3-rc.1"
edition = "2024"

[workspace.dependencies]
version = "9.9.9"
TOML
git -C "$repo" init -q
git -C "$repo" config user.email t@example.invalid
git -C "$repo" config user.name t
git -C "$repo" add -A
git -C "$repo" commit -q -m one
git -C "$repo" branch -M main
# A bare repository stands in for origin: the guard fetches main from it.
git init -q --bare "$tmp/origin.git"
git -C "$repo" remote add origin "$tmp/origin.git"
onmain() { git -C "$repo" push -q origin HEAD:refs/heads/main; }
onmain

run() { # run [--dry-run] <tag>: sets out and rc
    set +e
    out=$(DESKTOP_GUARD_ROOT=$repo "$guard" "$@" 2>&1)
    rc=$?
    set -e
}
refuses() { # refuses <text the message holds> [--dry-run] <tag>
    local reason=$1
    shift
    run "$@"
    [ "$rc" != 0 ] || fail "$* was accepted: $out"
    case $out in *"$reason"*) ;; *) fail "$* was refused, but not for '$reason': $out" ;; esac
}

# Windows dispatch accepts a matching input before the checkout has a tag,
# but refuses a well-formed tag that differs from the workspace version.
run --dry-run v1.2.3-rc.1
[ "$rc" = 0 ] || fail "an untagged checkout with a matching dry-run input was refused: $out"
refuses "use v1.2.3-rc.1 as windows_preview_tag" --dry-run v1.2.3-rc.2
refuses "not a release tag" --dry-run 1.2.3-rc.1

refuses "not at a tag" v1.2.3-rc.1

git -C "$repo" tag -a v1.2.3-rc.1 -m rc
run v1.2.3-rc.1
[ "$rc" = 0 ] || fail "a tagged, clean checkout with a matching helper was refused: $out"

refuses "HEAD is tagged v1.2.3-rc.1, not v1.2.3-rc.2" v1.2.3-rc.2
refuses "not a release tag" 1.2.3

# Another tag on the same commit is not this tag.
git -C "$repo" tag v1.2.3-rc.2
refuses "says version 1.2.3-rc.1" v1.2.3-rc.2

# A dirty tree: untracked, then modified.
echo x >"$repo/new"
refuses "not clean" v1.2.3-rc.1
rm "$repo/new"
echo '# edit' >>"$repo/voice/Cargo.toml"
refuses "not clean" v1.2.3-rc.1
git -C "$repo" checkout -q -- voice/Cargo.toml

# A tag on an older commit than HEAD.
echo two >"$repo/two"
git -C "$repo" add -A
git -C "$repo" commit -q -m two
onmain
refuses "not at a tag" v1.2.3-rc.1

# The helper's version differs from the tag's, on the tagged commit.
sed -i 's/^version = "1.2.3-rc.1"/version = "1.2.3-rc.0"/' "$repo/voice/Cargo.toml"
git -C "$repo" commit -q -am three
onmain
git -C "$repo" tag -a v1.2.3-rc.1b -m b
refuses "says version 1.2.3-rc.0" v1.2.3-rc.1b

# A commit that is not on main is refused, though everything else is right
# (tag, clean tree, matching helper version); once it is on main it passes.
git -C "$repo" checkout -q -b side
sed -i 's/^version = "1.2.3-rc.0"/version = "1.2.3-rc.4"/' "$repo/voice/Cargo.toml"
git -C "$repo" commit -q -am four
git -C "$repo" tag -a v1.2.3-rc.4 -m four
refuses "not on origin/main" v1.2.3-rc.4
onmain
run v1.2.3-rc.4
[ "$rc" = 0 ] || fail "a tagged commit on main was refused: $out"

echo "desktop-release-guard-test: OK"
