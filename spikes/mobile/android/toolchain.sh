#!/usr/bin/env bash
# Spike-local Android toolchain for #1109 (M4-A), Linux x86-64. Installs JDK 17,
# Android command-line tools 23.0, Platform-Tools 37.0.3, SDK Platform 35 rev 2,
# Build-Tools 36.0.0, NDK 27.1.12297006 and CMake 3.22.1. The SDK and the
# Android CLI launcher download, bundle and state go under CONCH_CACHE
# (default ~/.cache/conch). No root or system packages.
#
# The script checks the JDK against a pinned SHA-256 and the command-line
# tools ZIP against a pinned SHA-1. Google's launcher fetches the CLI from a
# rolling latest URL with no published digest for that download. The CLI
# handles the Platform-Tools, Platform, Build-Tools, NDK, CMake, emulator and
# system image downloads and has no option to supply our own archive digests.
# The emulator and system image use Google's current stable revisions.
#
# Nick signed off on Google's Android SDK licence for this spike in #1389.
# CONCH_ACCEPT_ANDROID_LICENCES=1 confirms your acceptance of that agreement
# before any download. On first use the CLI shows the agreement and a metrics
# notice even with --no-metrics. It records the SDK package licences in
# android-sdk/licenses without prompting.
#
# CONCH_ANDROID_NO_EMULATOR=1 skips the emulator and the system image (CI sets
# it: building the APK needs neither). This script creates no AVD either way.
#
# usage: CONCH_ACCEPT_ANDROID_LICENCES=1 ./toolchain.sh
# then:  source "${CONCH_CACHE:-$HOME/.cache/conch}/android-env.sh"
set -euo pipefail

CACHE="${CONCH_CACHE:-$HOME/.cache/conch}"
JDK_VERSION="17.0.21+9"
JDK_URL="https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.21%2B9/OpenJDK17U-jdk_x64_linux_hotspot_17.0.21_9.tar.gz"
JDK_SHA256="8b9b611d917e3dfd8419d3566b5949226f69b052fed7e109c1368add1dfe8711"
CLT_VERSION="23.0"
CLT_URL="https://dl.google.com/android/repository/commandlinetools-linux-16111833_latest.zip"
CLT_SHA1="e025545c62a8e64c7559119566a569fb1dec5f60" # from Google's repository2-3.xml
PLATFORM_TOOLS_VERSION="37.0.3"
SYSTEM_IMAGE="system-images/android-35/google_apis/x86_64"
NDK="ndk/27.1.12297006"
CMAKE="cmake/3.22.1"

if [ "${CONCH_ACCEPT_ANDROID_LICENCES:-}" != 1 ]; then
    echo "toolchain: installing into $CACHE requires accepting Google's Android SDK License Agreement." >&2
    echo "toolchain: review https://developer.android.com/studio/terms" >&2
    echo "toolchain: re-run with CONCH_ACCEPT_ANDROID_LICENCES=1 to confirm." >&2
    exit 1
fi

JDK_DIR="$CACHE/jdk-$JDK_VERSION"
SDK="$CACHE/android-sdk"
mkdir -p "$CACHE" "$SDK"
tmp=$(mktemp -d "$CACHE/dl.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

if [ ! -x "$JDK_DIR/bin/java" ]; then
    curl -fsSL -o "$tmp/jdk.tgz" "$JDK_URL"
    echo "$JDK_SHA256  $tmp/jdk.tgz" | sha256sum -c -
    mkdir "$tmp/jdk"
    tar -xzf "$tmp/jdk.tgz" -C "$tmp/jdk" --strip-components=1
    mv "$tmp/jdk" "$JDK_DIR"
fi

# Reuse a matching installation from before we used versioned directories.
CLT_DIR="$SDK/cmdline-tools/$CLT_VERSION"
legacy_clt_dir="$SDK/cmdline-tools/latest"
if [ ! -e "$CLT_DIR" ] && [ -x "$legacy_clt_dir/bin/android" ] &&
    grep -qxF "Pkg.Revision=$CLT_VERSION" "$legacy_clt_dir/source.properties"; then
    CLT_DIR="$legacy_clt_dir"
fi
ANDROID_CLI="$CLT_DIR/bin/android"
if [ ! -e "$CLT_DIR" ]; then
    curl -fsSL -o "$tmp/clt.zip" "$CLT_URL"
    echo "$CLT_SHA1  $tmp/clt.zip" | sha1sum -c -
    mkdir "$tmp/clt"
    unzip -q "$tmp/clt.zip" -d "$tmp/clt"
    mkdir -p "$SDK/cmdline-tools"
    mv -T "$tmp/clt/cmdline-tools" "$CLT_DIR"
fi
if [ ! -x "$ANDROID_CLI" ] || ! grep -qxF "Pkg.Revision=$CLT_VERSION" "$CLT_DIR/source.properties"; then
    echo "toolchain: expected command-line tools $CLT_VERSION in $CLT_DIR; use a fresh CONCH_CACHE." >&2
    exit 1
fi

export JAVA_HOME="$JDK_DIR"
export PATH="$JAVA_HOME/bin:$PATH"
# Both the native launcher (bin/android-cli) and its bundle (cli/bundles)
# resolve their paths from ANDROID_USER_HOME before falling back to ~/.android.
export ANDROID_USER_HOME="$CACHE/android-user-home"

packages=("platform-tools@$PLATFORM_TOOLS_VERSION" "platforms/android-35@2" "build-tools/36.0.0" "$NDK" "$CMAKE")
if [ "${CONCH_ANDROID_NO_EMULATOR:-}" != 1 ]; then
    packages+=("emulator" "$SYSTEM_IMAGE")
fi

# In command-line tools 23.0, sdkmanager is a shim that rejects --no-metrics
# and treats --licenses as a no-op. Pass --no-metrics to the launcher on the
# very first invocation so the CLI receives it even on a cold cache.
# --force lets the CLI downgrade Platform-Tools in an existing cache.

# One flaky download should not fail the run (#2007: a CI run lost the system
# image to a closed connection). The tools' exit status does not say why an
# install failed, so any failure is retried, at most three attempts in all.
attempt=1
until TMPDIR="$tmp" "$ANDROID_CLI" --no-metrics --sdk="$SDK" sdk install --force "${packages[@]}"; do
    if [ "$attempt" -ge 3 ]; then
        echo "toolchain: SDK package install failed $attempt times; giving up" >&2
        exit 1
    fi
    echo "toolchain: SDK package install failed (attempt $attempt of 3); retrying in $((attempt * 15))s" >&2
    sleep $((attempt * 15))
    attempt=$((attempt + 1))
done

platform_tools_dir="$SDK/platform-tools"
platform_tools_version=
if [ -f "$platform_tools_dir/source.properties" ]; then
    platform_tools_version=$(sed -n 's/^Pkg\.Revision[[:space:]]*=[[:space:]]*//p' "$platform_tools_dir/source.properties")
fi
if [ "$platform_tools_version" != "$PLATFORM_TOOLS_VERSION" ] || [ ! -x "$platform_tools_dir/adb" ]; then
    echo "toolchain: expected usable Platform-Tools $PLATFORM_TOOLS_VERSION in $platform_tools_dir." >&2
    exit 1
fi
echo "toolchain: verified Platform-Tools $platform_tools_version"

cat >"$CACHE/android-env.sh" <<ENV
# Source this: the spike's JDK, Android SDK and Gradle cache, all under $CACHE.
export JAVA_HOME="$JDK_DIR"
export ANDROID_HOME="$SDK"
export ANDROID_SDK_ROOT="$SDK"
export ANDROID_AVD_HOME="$CACHE/avd"
export GRADLE_USER_HOME="$CACHE/gradle-home"
export PATH="\$JAVA_HOME/bin:\$ANDROID_HOME/platform-tools:\$ANDROID_HOME/emulator:$CLT_DIR/bin:\$PATH"
ENV
echo "toolchain: OK; source $CACHE/android-env.sh"
