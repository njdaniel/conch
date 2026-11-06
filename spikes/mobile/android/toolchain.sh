#!/usr/bin/env bash
# Spike-local Android toolchain for #1109 (M4-A): JDK 17, the Android command
# line tools, SDK Platform 35, Build-Tools 36.0.0, the emulator and one system
# image, all under ~/.cache/conch. No root, no system packages, nothing outside
# that directory. Every download is checked against a pinned digest.
#
# Running this accepts Google's Android SDK licence (`sdkmanager --licenses`)
# for the SDK under ~/.cache/conch only. Nick accepted it for this spike on
# #1389. Set CONCH_ACCEPT_ANDROID_LICENCES=1 to confirm you have, too.
#
# usage: toolchain.sh        then:  source "$HOME/.cache/conch/android-env.sh"
set -euo pipefail

CACHE="${CONCH_CACHE:-$HOME/.cache/conch}"
JDK_VERSION="17.0.21+9"
JDK_URL="https://github.com/adoptium/temurin17-binaries/releases/download/jdk-17.0.21%2B9/OpenJDK17U-jdk_x64_linux_hotspot_17.0.21_9.tar.gz"
JDK_SHA256="8b9b611d917e3dfd8419d3566b5949226f69b052fed7e109c1368add1dfe8711"
CLT_URL="https://dl.google.com/android/repository/commandlinetools-linux-16111833_latest.zip"
CLT_SHA1="e025545c62a8e64c7559119566a569fb1dec5f60" # from Google's repository2-3.xml
SYSTEM_IMAGE="system-images;android-35;google_apis;x86_64"
NDK="ndk;27.1.12297006"
CMAKE="cmake;3.22.1"

if [ "${CONCH_ACCEPT_ANDROID_LICENCES:-}" != 1 ]; then
    echo "toolchain: this accepts Google's Android SDK licence for a toolchain under $CACHE." >&2
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

SDKMANAGER="$SDK/cmdline-tools/latest/bin/sdkmanager"
if [ ! -x "$SDKMANAGER" ]; then
    curl -fsSL -o "$tmp/clt.zip" "$CLT_URL"
    echo "$CLT_SHA1  $tmp/clt.zip" | sha1sum -c -
    mkdir "$tmp/clt"
    unzip -q "$tmp/clt.zip" -d "$tmp/clt"
    mkdir -p "$SDK/cmdline-tools"
    mv "$tmp/clt/cmdline-tools" "$SDK/cmdline-tools/latest"
fi

export JAVA_HOME="$JDK_DIR"
export PATH="$JAVA_HOME/bin:$PATH"
yes | "$SDKMANAGER" --sdk_root="$SDK" --licenses >/dev/null || true
"$SDKMANAGER" --sdk_root="$SDK" \
    "platform-tools" "platforms;android-35" "build-tools;36.0.0" \
    "emulator" "$SYSTEM_IMAGE" "$NDK" "$CMAKE"

cat >"$CACHE/android-env.sh" <<ENV
# Source this: the spike's JDK, Android SDK and Gradle cache, all under $CACHE.
export JAVA_HOME="$JDK_DIR"
export ANDROID_HOME="$SDK"
export ANDROID_SDK_ROOT="$SDK"
export ANDROID_AVD_HOME="$CACHE/avd"
export GRADLE_USER_HOME="$CACHE/gradle-home"
export PATH="\$JAVA_HOME/bin:\$ANDROID_HOME/platform-tools:\$ANDROID_HOME/emulator:\$ANDROID_HOME/cmdline-tools/latest/bin:\$PATH"
ENV
echo "toolchain: OK; source $CACHE/android-env.sh"
