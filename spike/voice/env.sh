# Source this before building lkspike. The LiveKit Rust SDK's prebuilt libwebrtc
# needs clang >= 21; Ubuntu/Pop 24.04 ships 18. This uses a clang extracted to
# ~/.cache/conch-spike/llvm (see README) and points it at GCC 13's C++ headers.
export PATH="$HOME/.cache/conch-spike/llvm/bin:$PATH"
export CC=clang CXX=clang++
export CFLAGS="--gcc-install-dir=/usr/lib/gcc/x86_64-linux-gnu/13"
export CXXFLAGS="$CFLAGS"
