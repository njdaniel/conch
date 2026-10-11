#!/usr/bin/env bash
# Fetches what building voice/ needs beyond rustup, into ~/.cache/conch:
#
#   - clang from one pinned LLVM release. The LiveKit SDK's prebuilt libwebrtc
#     needs clang 21 or newer and Ubuntu 24.04 ships 18 (ADR-006).
#   - cargo-deny at a pinned version, for `cargo deny check`.
#   - the prebuilt libwebrtc the livekit crate links, so that it is verified
#     before it is used (the crate's own download checks nothing).
#
# Versions and hashes are in scripts/voice-pins.sh. Each download is verified
# against its SHA-256 before anything is unpacked. Nothing is installed
# system-wide and no root is needed. Running it again does nothing once all
# three are in place.
#
# With the argument `deny` it fetches cargo-deny only (the advisories job in CI
# needs nothing else).
#
# CONCH_CACHE moves the cache (default ~/.cache/conch). CONCH_VOICE_LLVM_URL,
# CONCH_VOICE_DENY_URL and CONCH_VOICE_WEBRTC_URL replace a download address,
# for a mirror or a local file:// copy; the expected hashes do not change.
set -euo pipefail

. "$(dirname "$0")/voice-pins.sh"

only="${1:-all}"
case "$only" in
all | deny) ;;
*)
    echo "usage: voice-toolchain.sh [deny]" >&2
    exit 2
    ;;
esac

LLVM_VERSION=$CONCH_LLVM_VERSION
LLVM_ARCHIVE="LLVM-${LLVM_VERSION}-Linux-X64.tar.xz"
LLVM_SHA256=$CONCH_LLVM_SHA256
LLVM_URL="${CONCH_VOICE_LLVM_URL:-https://github.com/llvm/llvm-project/releases/download/llvmorg-${LLVM_VERSION}/${LLVM_ARCHIVE}}"

DENY_VERSION=$CONCH_DENY_VERSION
DENY_ARCHIVE="cargo-deny-${DENY_VERSION}-x86_64-unknown-linux-musl.tar.gz"
DENY_SHA256=$CONCH_DENY_SHA256
DENY_URL="${CONCH_VOICE_DENY_URL:-https://github.com/EmbarkStudios/cargo-deny/releases/download/${DENY_VERSION}/${DENY_ARCHIVE}}"

WEBRTC_TAG=$CONCH_WEBRTC_TAG
WEBRTC_TRIPLE=linux-x64-release
WEBRTC_ARCHIVE="webrtc-${WEBRTC_TRIPLE}.zip"
WEBRTC_SHA256=$CONCH_WEBRTC_SHA256
WEBRTC_URL="${CONCH_VOICE_WEBRTC_URL:-https://github.com/livekit/rust-sdks/releases/download/${WEBRTC_TAG}/${WEBRTC_ARCHIVE}}"

cache="${CONCH_CACHE:-$HOME/.cache/conch}"
llvm_dir="$cache/llvm-$LLVM_VERSION"
deny_dir="$cache/cargo-deny-$DENY_VERSION"
webrtc_dir="$cache/$WEBRTC_TAG"

if [ "$(uname -s)-$(uname -m)" != "Linux-x86_64" ]; then
    echo "voice-toolchain: FAIL: only Linux x86_64 is supported (ADR-006), this is $(uname -s) $(uname -m)" >&2
    exit 1
fi

# fetch <url> <sha256> <destination file>: download, then verify. The file is
# removed again if the hash does not match, so a bad download is never kept.
fetch() {
    local url=$1 want=$2 dest=$3 got
    echo "voice-toolchain: downloading $url"
    curl --fail --silent --show-error --location --output "$dest" "$url"
    got=$(sha256sum "$dest" | cut -d' ' -f1)
    if [ "$got" != "$want" ]; then
        rm -f "$dest"
        echo "voice-toolchain: FAIL: SHA-256 of $url is $got, expected $want; nothing was unpacked" >&2
        return 1
    fi
}

mkdir -p "$cache"

# One run at a time. Two started together (two worktrees, a hook and a shell)
# would otherwise each replace the directory the other had just put in place,
# under a build that may already be using it. The second waits, then finds
# everything present.
exec 9>"$cache/.toolchain.lock"
flock 9

work=$(mktemp -d "$cache/.fetch.XXXXXX")
trap 'rm -rf "$work"' EXIT

if [ "$only" = deny ]; then
    : # clang is not needed to read the advisory database
elif [ -x "$llvm_dir/bin/clang" ]; then
    echo "voice-toolchain: clang $LLVM_VERSION is already in $llvm_dir"
else
    fetch "$LLVM_URL" "$LLVM_SHA256" "$work/$LLVM_ARCHIVE"
    # Only the compiler, the linker and the compiler's own headers: about a
    # third of the release.
    mkdir "$work/llvm"
    tar -xJf "$work/$LLVM_ARCHIVE" -C "$work/llvm" --strip-components=1 --wildcards \
        '*/bin/clang' '*/bin/clang++' "*/bin/clang-${LLVM_VERSION%%.*}" \
        '*/bin/lld' '*/bin/ld.lld' '*/bin/llvm-ar' '*/lib/clang/*'
    rm -f "$work/$LLVM_ARCHIVE"
    "$work/llvm/bin/clang" --version | head -n1
    rm -rf "$llvm_dir"
    mv "$work/llvm" "$llvm_dir" # the directory appears complete or not at all
    echo "voice-toolchain: clang $LLVM_VERSION unpacked into $llvm_dir"
fi

if [ -x "$deny_dir/cargo-deny" ]; then
    echo "voice-toolchain: cargo-deny $DENY_VERSION is already in $deny_dir"
else
    fetch "$DENY_URL" "$DENY_SHA256" "$work/$DENY_ARCHIVE"
    mkdir "$work/deny"
    tar -xzf "$work/$DENY_ARCHIVE" -C "$work/deny" --strip-components=1 --wildcards '*/cargo-deny'
    "$work/deny/cargo-deny" --version
    rm -rf "$deny_dir"
    mv "$work/deny" "$deny_dir"
    echo "voice-toolchain: cargo-deny $DENY_VERSION unpacked into $deny_dir"
fi

if [ "$only" = deny ]; then
    : # nor is libwebrtc
elif [ -f "$webrtc_dir/$WEBRTC_TRIPLE/webrtc.ninja" ]; then
    echo "voice-toolchain: libwebrtc $WEBRTC_TAG is already in $webrtc_dir"
else
    fetch "$WEBRTC_URL" "$WEBRTC_SHA256" "$work/$WEBRTC_ARCHIVE"
    mkdir "$work/webrtc"
    # The archive's root is the one directory $WEBRTC_TRIPLE. It has been
    # verified above; zipfile also drops absolute paths and ".." components
    # from member names, so nothing is written outside the directory given.
    python3 -c 'import sys, zipfile; zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])' "$work/$WEBRTC_ARCHIVE" "$work/webrtc"
    rm -f "$work/$WEBRTC_ARCHIVE"
    test -f "$work/webrtc/$WEBRTC_TRIPLE/webrtc.ninja"
    rm -rf "$webrtc_dir"
    mv "$work/webrtc" "$webrtc_dir"
    echo "voice-toolchain: libwebrtc $WEBRTC_TAG unpacked into $webrtc_dir"
fi
