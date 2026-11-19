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

# ... and refuses the three ways cargo can swap a listed crate for another copy.
cat >>"$tmp/ws/Cargo.toml" <<'TOML'

[patch.crates-io]
serde = { path = "member" }
TOML
if out=$(./scripts/voice-depgate.sh "$tmp/ws" "$tmp/allow.txt" 2>&1); then
    fail "the dependency gate passed a workspace with a [patch] section"
fi
grep -q 'has a \[patch\] or \[replace\] section' <<<"$out" || fail "the dependency gate failed a [patch] for another reason: $out"
sed -i 's/^\[patch.crates-io\]$/[replace]/' "$tmp/ws/Cargo.toml"
if ./scripts/voice-depgate.sh "$tmp/ws" "$tmp/allow.txt" >/dev/null 2>&1; then
    fail "the dependency gate passed a workspace with a [replace] section"
fi
sed -i '/^\[replace\]$/,$d' "$tmp/ws/Cargo.toml"
mkdir "$tmp/ws/.cargo"
printf '[source.crates-io]\nreplace-with = "elsewhere"\n' >"$tmp/ws/.cargo/config.toml"
if out=$(./scripts/voice-depgate.sh "$tmp/ws" "$tmp/allow.txt" 2>&1); then
    fail "the dependency gate passed a workspace whose cargo configuration replaces a source"
fi
grep -q 'replaces a crate or a crate source' <<<"$out" || fail "the dependency gate failed a source replacement for another reason: $out"
rm -r "$tmp/ws/.cargo"
./scripts/voice-depgate.sh "$tmp/ws" "$tmp/allow.txt" >/dev/null || fail "the dependency gate still refuses the workspace after the swaps were removed"

# 2. The toolchain script refuses a download whose SHA-256 is not the pinned
#    one, and leaves nothing behind.
head -c 4096 /dev/urandom >"$tmp/not-llvm.tar.xz"
mkdir "$tmp/cache"
if out=$(CONCH_CACHE="$tmp/cache" CONCH_VOICE_LLVM_URL="file://$tmp/not-llvm.tar.xz" ./scripts/voice-toolchain.sh 2>&1); then
    fail "the toolchain script accepted an archive with the wrong hash"
fi
grep -q 'nothing was unpacked' <<<"$out" || fail "the toolchain script failed for another reason: $out"
# Its lock file is all that may remain.
if [ -n "$(find "$tmp/cache" -mindepth 1 -not -name .toolchain.lock -print -quit)" ]; then
    fail "the toolchain script left files in the cache after refusing a download: $(ls -A "$tmp/cache")"
fi

# 3. Exercise real archive extraction and cache verification on both hosts,
# with small release fixtures and a fake Darwin uname. No network or Xcode.
fixture="$tmp/toolchain"
mkdir -p "$fixture"/{scripts,bin,download/deny,download/mac-arm64-release/lib}
cp scripts/voice-{toolchain,env,cache,pins}.sh "$fixture/scripts/"
cat >"$fixture/bin/uname" <<'SH'
#!/usr/bin/env bash
case "$1" in
    -s) echo Darwin ;;
    -m) echo arm64 ;;
    *) exec /usr/bin/uname "$@" ;;
esac
SH
cat >"$fixture/bin/curl" <<'SH'
#!/usr/bin/env bash
if [ -n "${VOICE_TEST_HOLD:-}" ]; then
    printf '%s\n' "$$" >"$VOICE_TEST_HOLD/downloader"
    while [ ! -e "$VOICE_TEST_HOLD/release" ]; do sleep 0.02; done
fi
exec /usr/bin/curl "$@"
SH
chmod +x "$fixture/bin/uname" "$fixture/bin/curl"
printf '#!/bin/sh\necho cargo-deny fixture\n' >"$fixture/download/deny/cargo-deny"
chmod +x "$fixture/download/deny/cargo-deny"
tar -czf "$fixture/download/deny.tar.gz" -C "$fixture/download" deny
printf 'webrtc build fixture\n' >"$fixture/download/mac-arm64-release/webrtc.ninja"
printf 'webrtc archive fixture\n' >"$fixture/download/mac-arm64-release/lib/libwebrtc.a"
(cd "$fixture/download" && python3 -m zipfile -c webrtc.zip mac-arm64-release)
deny_pin=$(sha256sum "$fixture/download/deny.tar.gz" | cut -d' ' -f1)
mac_pin=$(sha256sum "$fixture/download/webrtc.zip" | cut -d' ' -f1)
linked_pin=$(sha256sum "$fixture/download/mac-arm64-release/lib/libwebrtc.a" | cut -d' ' -f1)
cat >>"$fixture/scripts/voice-pins.sh" <<EOF
CONCH_DENY_SHA256=$deny_pin
CONCH_DENY_MAC_SHA256=$deny_pin
CONCH_WEBRTC_MAC_SHA256=$mac_pin
CONCH_WEBRTC_LINKED_SHA256=$linked_pin
EOF

(
    export PATH="$fixture/bin:$PATH" CONCH_CACHE="$fixture/mac-cache"
    export CONCH_VOICE_DENY_URL="file://$fixture/download/deny.tar.gz"
    export CONCH_VOICE_WEBRTC_URL="file://$fixture/download/webrtc.zip"
    toolchain="$fixture/scripts/voice-toolchain.sh"
    env_script="$fixture/scripts/voice-env.sh"
    . "$fixture/scripts/voice-pins.sh"
    "$toolchain"
    bash -c '. "$1"' _ "$env_script"
    # Cache hits must work without access to the download URLs.
    CONCH_VOICE_DENY_URL=file:///missing CONCH_VOICE_WEBRTC_URL=file:///missing "$toolchain"
    deny="$CONCH_CACHE/cargo-deny-$CONCH_DENY_VERSION-aarch64-apple-darwin-${deny_pin:0:12}/cargo-deny"
    webrtc="$CONCH_CACHE/$CONCH_WEBRTC_TAG-mac-${mac_pin:0:12}/mac-arm64-release"
    for file in "$deny" "$webrtc/lib/libwebrtc.a"; do
        printf '#!/bin/sh\ntouch "%s"\n' "$fixture/executed-corrupt-file" >"$file"
        if bash -c '. "$1"' _ "$env_script"; then
            fail "macOS exposed a corrupted cache file: $file"
        fi
        "$toolchain"
        bash -c '. "$1"' _ "$env_script"
    done
    [ ! -e "$fixture/executed-corrupt-file" ] || fail "macOS executed the corrupted cargo-deny"
    printf 'corrupted\n' >>"$(dirname "$webrtc")/release.zip"
    if bash -c '. "$1"' _ "$env_script"; then
        fail "macOS used an unverified cached libwebrtc download as the reference"
    fi
    "$toolchain"

    # A waiter must stay behind a live holder, then acquire the same inode
    # when the holder exits normally or is killed without running cleanup.
    lock="$CONCH_CACHE/.toolchain-mac.flock"
    for outcome in normal killed; do
        hold="$fixture/$outcome"
        mkdir "$hold"
        printf 'corrupted\n' >"$deny"
        VOICE_TEST_HOLD="$hold" "$toolchain" deny >"$hold/holder.log" 2>&1 &
        holder=$!
        for _ in {1..100}; do
            [ -s "$hold/downloader" ] && break
            sleep 0.02
        done
        [ -s "$hold/downloader" ] || fail "holder did not start downloading: $(cat "$hold/holder.log")"
        downloader=$(cat "$hold/downloader")
        [ "$(cat "$lock")" = "$holder" ] || fail "fixture did not take the macOS lock"
        timeout 15 "$toolchain" deny >"$hold/waiter.log" 2>&1 &
        waiter=$!
        sleep 0.3
        before_release=$(cat "$lock")
        if [ "$outcome" = killed ]; then
            kill -KILL "$holder"
            wait "$holder" 2>/dev/null || true
            # The downloader is still running. It must not inherit the lock.
            result=0
            wait "$waiter" || result=$?
            kill "$downloader"
            [ "$result" -eq 0 ] || fail "macOS lock remained held after SIGKILL: $(cat "$hold/waiter.log")"
        else
            touch "$hold/release"
            wait "$holder" || fail "holder did not finish: $(cat "$hold/holder.log")"
            wait "$waiter" || fail "macOS lock handoff failed: $(cat "$hold/waiter.log")"
        fi
        [ "$before_release" = "$holder" ] || fail "waiter ran before the macOS lock was released"
        [ "$(cat "$lock")" != "$holder" ] || fail "macOS lock did not record its new owner"
    done
)

(
    export CONCH_CACHE="$fixture/linux-cache"
    export CONCH_VOICE_DENY_URL="file://$fixture/download/deny.tar.gz"
    toolchain="$fixture/scripts/voice-toolchain.sh"
    env_script="$fixture/scripts/voice-env.sh"
    . "$fixture/scripts/voice-pins.sh"
    llvm="$CONCH_CACHE/llvm-$CONCH_LLVM_VERSION"
    linked="$CONCH_CACHE/$CONCH_WEBRTC_TAG-linked-${linked_pin:0:12}/linux-x64-release"
    mkdir -p "$llvm/bin" "$linked/lib"
    ln -s /bin/true "$llvm/bin/clang"
    cp "$fixture/download/mac-arm64-release/webrtc.ninja" "$linked/"
    cp "$fixture/download/mac-arm64-release/lib/libwebrtc.a" "$linked/lib/"
    "$toolchain" deny
    bash -c '. "$1"' _ "$env_script"
    CONCH_VOICE_DENY_URL=file:///missing "$toolchain" deny
    printf '#!/bin/sh\ntouch "%s"\n' "$fixture/executed-corrupt-file" >"$CONCH_CACHE/cargo-deny-$CONCH_DENY_VERSION/cargo-deny"
    if bash -c '. "$1"' _ "$env_script"; then
        fail "Linux exposed a corrupted cached cargo-deny"
    fi
    "$toolchain" deny
    bash -c '. "$1"' _ "$env_script"
    [ ! -e "$fixture/executed-corrupt-file" ] || fail "Linux executed the corrupted cargo-deny"
    printf 'corrupted\n' >>"$CONCH_CACHE/cargo-deny-$CONCH_DENY_VERSION/release.tar.gz"
    if bash -c '. "$1"' _ "$env_script"; then
        fail "Linux used an unverified cached cargo-deny download as the reference"
    fi
    "$toolchain" deny
    printf 'corrupted\n' >"$linked/lib/libwebrtc.a"
    if bash -c '. "$1"' _ "$env_script"; then
        fail "Linux exposed a corrupted cached libwebrtc"
    fi
)

echo "voice-gates-test: OK"
