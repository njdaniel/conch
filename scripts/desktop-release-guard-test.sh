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

run() { # run <tag>: sets out and rc
    set +e
    out=$(DESKTOP_GUARD_ROOT=$repo "$guard" "$1" 2>&1)
    rc=$?
    set -e
}
refuses() { # refuses <tag> <text the message holds>
    run "$1"
    [ "$rc" != 0 ] || fail "$1 was accepted: $out"
    case $out in *"$2"*) ;; *) fail "$1 was refused, but not for '$2': $out" ;; esac
}

refuses v1.2.3-rc.1 "not at a tag"

git -C "$repo" tag -a v1.2.3-rc.1 -m rc
run v1.2.3-rc.1
[ "$rc" = 0 ] || fail "a tagged, clean checkout with a matching helper was refused: $out"

refuses v1.2.3-rc.2 "HEAD is tagged v1.2.3-rc.1, not v1.2.3-rc.2"
refuses 1.2.3 "not a release tag"

# Another tag on the same commit is not this tag.
git -C "$repo" tag v1.2.3-rc.2
refuses v1.2.3-rc.2 "says version 1.2.3-rc.1"

# A dirty tree: untracked, then modified.
echo x >"$repo/new"
refuses v1.2.3-rc.1 "not clean"
rm "$repo/new"
echo '# edit' >>"$repo/voice/Cargo.toml"
refuses v1.2.3-rc.1 "not clean"
git -C "$repo" checkout -q -- voice/Cargo.toml

# A tag on an older commit than HEAD.
echo two >"$repo/two"
git -C "$repo" add -A
git -C "$repo" commit -q -m two
refuses v1.2.3-rc.1 "not at a tag"

# The helper's version differs from the tag's, on the tagged commit.
sed -i 's/^version = "1.2.3-rc.1"/version = "1.2.3-rc.0"/' "$repo/voice/Cargo.toml"
git -C "$repo" commit -q -am three
git -C "$repo" tag -a v1.2.3-rc.1b -m b
refuses v1.2.3-rc.1b "says version 1.2.3-rc.0"

echo "desktop-release-guard-test: OK"
