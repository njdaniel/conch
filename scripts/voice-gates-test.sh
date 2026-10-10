#!/usr/bin/env bash
# Tests of the two gates that guard what goes into conch-voice. A gate that
# cannot fail is not a gate, so each is shown failing on what it exists to stop.
# Run by scripts/rust-check.sh. Needs no network.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
    echo "voice-gates-test: FAIL: $*"
    exit 1
}

# 1. The dependency gate refuses a crate that is not on the list, and names it.
mkdir -p "$tmp/ws/member/src"
cat >"$tmp/ws/Cargo.toml" <<'TOML'
[workspace]
resolver = "3"
members = ["member"]
TOML
cat >"$tmp/ws/member/Cargo.toml" <<'TOML'
[package]
name = "member"
version = "0.0.0"
edition = "2024"

[dependencies]
serde = "1"
not-on-the-list = "1"

[dev-dependencies]
also-not-on-the-list = "1"
TOML
: >"$tmp/ws/member/src/lib.rs"
printf '# test list\nserde\n' >"$tmp/allow.txt"
if out=$(./scripts/voice-depgate.sh "$tmp/ws" "$tmp/allow.txt" 2>&1); then
    fail "the dependency gate passed a manifest naming unlisted crates"
fi
grep -q 'not-on-the-list is not in' <<<"$out" || fail "the dependency gate did not name the unlisted crate: $out"
grep -q 'also-not-on-the-list is not in' <<<"$out" || fail "the dependency gate ignored a dev-dependency: $out"
grep -q 'serde is not in' <<<"$out" && fail "the dependency gate refused a listed crate: $out"

# ... and passes the same manifest once both are listed.
printf 'serde\nnot-on-the-list\nalso-not-on-the-list\n' >"$tmp/allow.txt"
./scripts/voice-depgate.sh "$tmp/ws" "$tmp/allow.txt" >/dev/null || fail "the dependency gate refused a manifest whose crates are all listed"

# 2. The toolchain script refuses a download whose SHA-256 is not the pinned
#    one, and leaves nothing behind.
head -c 4096 /dev/urandom >"$tmp/not-llvm.tar.xz"
mkdir "$tmp/cache"
if out=$(CONCH_CACHE="$tmp/cache" CONCH_VOICE_LLVM_URL="file://$tmp/not-llvm.tar.xz" ./scripts/voice-toolchain.sh 2>&1); then
    fail "the toolchain script accepted an archive with the wrong hash"
fi
grep -q 'nothing was unpacked' <<<"$out" || fail "the toolchain script failed for another reason: $out"
if [ -n "$(find "$tmp/cache" -mindepth 1 -print -quit)" ]; then
    fail "the toolchain script left files in the cache after refusing a download: $(ls -A "$tmp/cache")"
fi

echo "voice-gates-test: OK"
