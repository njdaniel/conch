#!/usr/bin/env bash
# Installs the system packages the voice build needs on a GitHub-hosted runner
# (issue #1352), for CI's rust and voice-check jobs:
#
#   scripts/ci-voice-apt.sh DIR
#
# DIR is apt's archive cache, the .debs it downloads, which the workflow keeps
# between runs with actions/cache, keyed on this file (the package list below)
# and the runner image. With a warm cache nothing is downloaded but the package
# indexes. Whatever is missing or out of date is fetched, then the cache is
# pruned to what the current indexes still name, so it does not grow.
#
# The packages: GLib's headers for the LiveKit SDK's build, and PipeWire's
# headers and libclang for dmn-audio's (bindgen generates its PipeWire bindings
# with the system libclang; the pinned clang from scripts/voice-toolchain.sh
# ships no libclang.so). scripts/voice-env.sh says which is missing.
#
# A slow mirror must not hold the job: each connection gives up after 30
# seconds without data and is retried, and since a download that trickles
# never trips that, each download attempt is also cut off after
# APT_DOWNLOAD_SECONDS (default 180; the usual 41 MB takes a few seconds) and
# retried, resuming what it already has, up to three times, the retries from
# the primary archive rather than the runner's mirror.
set -euo pipefail

packages=(libglib2.0-dev libpipewire-0.3-dev libclang-dev pkg-config)

if [ $# -ne 1 ]; then
    echo "usage: $0 ARCHIVE-DIR" >&2
    exit 2
fi
archives=$1
download_seconds=${APT_DOWNLOAD_SECONDS:-180}

apt_opts=(
    -o Acquire::http::Timeout=30
    -o Acquire::https::Timeout=30
    -o Acquire::Retries=3
    -o Dir::Cache::Archives="$archives"
    # Kept after installing, which is what fills the cache.
    -o APT::Keep-Downloaded-Packages=true
    # The cache lives in the runner's home, which apt's download user (_apt)
    # cannot enter; without this apt warns and downloads as root anyway.
    -o APT::Sandbox::User=root
)

mkdir -p "$archives/partial"
echo "ci-voice-apt: $(find "$archives" -maxdepth 1 -name '*.deb' | wc -l) cached .debs in $archives"

sudo apt-get "${apt_opts[@]}" update

attempt=1
until sudo timeout "$download_seconds" apt-get "${apt_opts[@]}" install -y --no-install-recommends --download-only "${packages[@]}"; do
    if [ "$attempt" -ge 3 ]; then
        echo "ci-voice-apt: FAIL: the packages did not download in 3 attempts of ${download_seconds}s" >&2
        exit 1
    fi
    attempt=$((attempt + 1))
    # The runner's Azure mirror is the one that trickles (#1352, twice on
    # 2026-11-05): a retry goes to the primary archive instead.
    if [ "$attempt" -eq 2 ]; then
        echo "ci-voice-apt: switching from the runner's mirror to archive.ubuntu.com" >&2
        sudo find /etc/apt -name '*.list' -o -name '*.sources' | xargs -r sudo sed -i 's#http://azure\.archive\.ubuntu\.com/ubuntu#http://archive.ubuntu.com/ubuntu#g'
        sudo apt-get "${apt_opts[@]}" update
    fi
    echo "ci-voice-apt: download attempt $attempt of 3" >&2
done

sudo apt-get "${apt_opts[@]}" install -y --no-install-recommends --no-download "${packages[@]}"

# Drop .debs the indexes no longer name (an older version after an update),
# and hand the directory back to the runner's user so actions/cache can read
# all of it when it saves.
sudo apt-get "${apt_opts[@]}" autoclean
sudo rm -rf "$archives/partial" "$archives/lock"
sudo chown -R "$(id -u):$(id -g)" "$archives"
echo "ci-voice-apt: $(find "$archives" -maxdepth 1 -name '*.deb' | wc -l) .debs to cache, $(du -sh "$archives" | cut -f1)"
