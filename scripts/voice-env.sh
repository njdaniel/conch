# Sourced by scripts/rust-check.sh and by anyone building voice/ by hand:
#
#   . scripts/voice-env.sh && (cd voice && cargo build --locked)
#
# It puts the pinned clang and cargo-deny that scripts/voice-toolchain.sh
# fetched on PATH, and points the C and C++ compilers at that clang and the
# livekit crate at the verified libwebrtc. It fails, and says what to run, if
# the toolchain is missing: building voice/ never falls back to the system
# compiler.

# BASH_SOURCE: this file is sourced, so $0 is the caller.
. "$(dirname "${BASH_SOURCE[0]}")/voice-pins.sh"

_conch_cache="${CONCH_CACHE:-$HOME/.cache/conch}"
_conch_llvm="$_conch_cache/llvm-$CONCH_LLVM_VERSION"
_conch_deny="$_conch_cache/cargo-deny-$CONCH_DENY_VERSION"
# What is linked is not the download itself but the copy beside it without its PipeWire
# stand-ins (scripts/voice-pins.sh says why), in a directory named by its archive's hash.
_conch_webrtc="$_conch_cache/$CONCH_WEBRTC_TAG-linked-${CONCH_WEBRTC_LINKED_SHA256:0:12}/linux-x64-release"

if [ ! -x "$_conch_llvm/bin/clang" ] || [ ! -x "$_conch_deny/cargo-deny" ] || [ ! -f "$_conch_webrtc/webrtc.ninja" ]; then
    echo "voice: FAIL: the pinned clang $CONCH_LLVM_VERSION, cargo-deny $CONCH_DENY_VERSION or libwebrtc $CONCH_WEBRTC_TAG is not in $_conch_cache; run scripts/voice-toolchain.sh" >&2
    return 1 2>/dev/null || exit 1
fi
# The archive that is linked must be the pinned one. With the download's own, conch-voice
# builds and then dies on its first call into PipeWire.
if [ "$(sha256sum "$_conch_webrtc/lib/libwebrtc.a" | cut -d' ' -f1)" != "$CONCH_WEBRTC_LINKED_SHA256" ]; then
    echo "voice: FAIL: $_conch_webrtc/lib/libwebrtc.a is not the archive scripts/voice-pins.sh pins for linking; remove $(dirname "$_conch_webrtc") and run scripts/voice-toolchain.sh" >&2
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

# dmn-audio's build generates the Rust bindings to PipeWire with bindgen. That needs the
# PipeWire headers, which pkg-config finds, and a libclang to parse them with. It is the
# system's libclang (Ubuntu: libclang-dev), not the pinned clang above, which is only the
# compiler of the SDK's C++ and ships no libclang.so: the two do not meet, since bindgen
# only reads the headers and writes Rust (docs/design/conch-voice.md §13 row 7).
if ! pkg-config --exists libpipewire-0.3 libspa-0.2; then
    echo "voice: FAIL: PipeWire's development headers are not installed (Ubuntu: sudo apt install libpipewire-0.3-dev)" >&2
    return 1 2>/dev/null || exit 1
fi
if [ -z "${LIBCLANG_PATH:-}" ] && ! ls /usr/lib/llvm-*/lib/libclang.so* /usr/lib/x86_64-linux-gnu/libclang*.so* >/dev/null 2>&1; then
    echo "voice: FAIL: libclang is not installed, and the PipeWire bindings need it (Ubuntu: sudo apt install libclang-dev), or set LIBCLANG_PATH to the directory holding libclang.so" >&2
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
# The livekit crate links this copy, which scripts/voice-toolchain.sh made from
# the verified download and verified again, and does not download its own.
export LK_CUSTOM_WEBRTC="$_conch_webrtc"
# Cargo's build directory is left where cargo puts it, voice/target in this
# checkout, and must not be shared between checkouts. Cargo names a workspace
# crate's build by its path inside the workspace, which is the same in every
# checkout, and decides whether it is up to date by file times. With one
# directory for two git worktrees, the second is told that the first one's
# build of conch-voice is "fresh" and links it: its tests then run another
# checkout's code and say they passed. (Measured, issue #185. A shared
# CARGO_TARGET_DIR does the same, and replaces the binary under a running test
# as well.) A new worktree therefore compiles the dependencies once for itself.
if [ -n "${CARGO_BUILD_BUILD_DIR:-}${CARGO_TARGET_DIR:-}" ]; then
    echo "voice: note: CARGO_BUILD_BUILD_DIR or CARGO_TARGET_DIR is set; make sure no other checkout builds into the same directory" >&2
fi

unset _conch_cache _conch_llvm _conch_deny _conch_webrtc _conch_gcc _conch_dir
