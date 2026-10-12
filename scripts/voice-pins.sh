# The exact versions building voice/ depends on beyond what Cargo.lock pins,
# with the SHA-256 of each download. Sourced by scripts/voice-toolchain.sh
# (which fetches them) and scripts/voice-env.sh (which uses them). Changing a
# line here changes what is compiled into conch-voice: it needs the same care
# as a dependency change.

# clang: the LiveKit SDK's prebuilt libwebrtc needs clang 21 or newer, and
# Ubuntu 24.04 ships 18 (ADR-006).
CONCH_LLVM_VERSION=22.1.8
CONCH_LLVM_SHA256=df0e1ecf16caf3489a272a5eea4eec9b0d82878f6477fa309504f918a0006384

# cargo-deny, for `cargo deny check`.
CONCH_DENY_VERSION=0.20.2
CONCH_DENY_SHA256=9f12ed4c49936e09b48bf862b595cde2fe64fcbd9d74dfacac6131ca824c8d5f

# The prebuilt libwebrtc the livekit crate links. Left alone, the crate's build
# script downloads this file itself and checks nothing about it; fetching it
# here means it is verified first. The tag must be the one the locked
# webrtc-sys-build crate names (scripts/rust-check.sh compares them).
CONCH_WEBRTC_TAG=webrtc-89d790b
CONCH_WEBRTC_SHA256=b167adad5291cea0e4d66a0454d9d52d2ad714e6b0ed70f4410317d3ebde70c5
