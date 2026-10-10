# Sourced by scripts/rust-check.sh and by anyone building voice/ by hand:
#
#   . scripts/voice-env.sh && (cd voice && cargo build --locked)
#
# It puts the pinned clang and cargo-deny that scripts/voice-toolchain.sh
# fetched on PATH, points the C and C++ compilers at that clang and the livekit
# crate at the verified libwebrtc, and gives
# every checkout one shared cargo build directory, so a new git worktree does
# not rebuild every crate. It fails, and says what to run, if the toolchain is
# missing: building voice/ never falls back to the system compiler.

# BASH_SOURCE: this file is sourced, so $0 is the caller.
. "$(dirname "${BASH_SOURCE[0]}")/voice-pins.sh"

_conch_cache="${CONCH_CACHE:-$HOME/.cache/conch}"
_conch_llvm="$_conch_cache/llvm-$CONCH_LLVM_VERSION"
_conch_deny="$_conch_cache/cargo-deny-$CONCH_DENY_VERSION"
_conch_webrtc="$_conch_cache/$CONCH_WEBRTC_TAG/linux-x64-release"

if [ ! -x "$_conch_llvm/bin/clang" ] || [ ! -x "$_conch_deny/cargo-deny" ] || [ ! -f "$_conch_webrtc/webrtc.ninja" ]; then
    echo "voice: FAIL: the pinned clang $CONCH_LLVM_VERSION, cargo-deny $CONCH_DENY_VERSION or libwebrtc $CONCH_WEBRTC_TAG is not in $_conch_cache; run scripts/voice-toolchain.sh" >&2
    return 1 2>/dev/null || exit 1
fi
if ! command -v cargo >/dev/null 2>&1; then
    echo "voice: FAIL: cargo is not on PATH; install rustup (https://rustup.rs), which reads voice/rust-toolchain.toml" >&2
    return 1 2>/dev/null || exit 1
fi

# The livekit crate's build reads GLib's headers and finds them with pkg-config.
if ! command -v pkg-config >/dev/null 2>&1 || ! pkg-config --exists glib-2.0 gobject-2.0 gio-2.0; then
    echo "voice: FAIL: GLib's development headers are not installed (Ubuntu: sudo apt install libglib2.0-dev pkg-config)" >&2
    return 1 2>/dev/null || exit 1
fi

# clang uses GCC's C++ standard library. Left to itself it picks the newest
# GCC directory it finds, whose C++ headers may not be installed, so name the
# newest one that has them.
_conch_gcc=""
for _conch_dir in $(ls -d /usr/lib/gcc/x86_64-linux-gnu/[0-9]* 2>/dev/null | sort -V -r); do
    if [ -d "/usr/include/c++/${_conch_dir##*/}" ]; then
        _conch_gcc=$_conch_dir
        break
    fi
done
if [ -z "$_conch_gcc" ]; then
    echo "voice: FAIL: no GCC with C++ headers under /usr/lib/gcc/x86_64-linux-gnu; install g++ (Ubuntu: sudo apt install g++)" >&2
    return 1 2>/dev/null || exit 1
fi

export PATH="$_conch_llvm/bin:$_conch_deny:$PATH"
export CC=clang CXX=clang++
export CFLAGS="--gcc-install-dir=$_conch_gcc"
export CXXFLAGS="$CFLAGS"
# The livekit crate links this copy, which scripts/voice-toolchain.sh verified,
# and does not download its own.
export LK_CUSTOM_WEBRTC="$_conch_webrtc"
# One build directory for every checkout: the 300 dependencies are compiled
# once and a new git worktree reuses them. Only cargo's intermediate files go
# there. What a build finally produces (the conch-voice binary above all) stays
# in the checkout's own voice/target, so two worktrees building at once cannot
# replace each other's binary under a running test.
export CARGO_BUILD_BUILD_DIR="${CARGO_BUILD_BUILD_DIR:-$_conch_cache/voice-build}"

unset _conch_cache _conch_llvm _conch_deny _conch_webrtc _conch_gcc _conch_dir
