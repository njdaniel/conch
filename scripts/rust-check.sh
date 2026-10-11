#!/usr/bin/env bash
# The Rust leg of `make check` (ADR-006): everything that must pass for a change
# under voice/. CI runs the same script. It fails, naming
# scripts/voice-toolchain.sh, if the pinned toolchain has not been fetched; it
# never skips.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
. scripts/voice-env.sh

# The libwebrtc that scripts/voice-toolchain.sh verified must be the one the
# locked SDK expects; a livekit upgrade that moves to another build has to
# change scripts/voice-pins.sh too.
webrtc_build=$(cd voice && cargo metadata --locked --format-version 1 |
    jq -r '.packages[] | select(.name == "webrtc-sys-build") | .manifest_path' | xargs dirname)
want=$(sed -n 's/^pub const WEBRTC_TAG: &str = "\(.*\)";$/\1/p' "$webrtc_build/src/lib.rs")
if [ "$want" != "$CONCH_WEBRTC_TAG" ]; then
    echo "rust-check: FAIL: the locked SDK expects libwebrtc '$want' but scripts/voice-pins.sh pins '$CONCH_WEBRTC_TAG'"
    exit 1
fi

echo "rust-check: cargo fmt"
(cd voice && cargo fmt --all --check)

echo "rust-check: cargo clippy"
(cd voice && cargo clippy --locked --workspace --all-targets -- -D warnings)

echo "rust-check: cargo test"
(cd voice && cargo test --locked --workspace)

echo "rust-check: dependency gate"
./scripts/voice-depgate.sh
./scripts/voice-gates-test.sh

echo "rust-check: cargo deny (sources, licences, bans)"
(cd voice && cargo deny --locked check sources licenses bans)

echo "rust-check: OK"
