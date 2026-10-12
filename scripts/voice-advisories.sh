#!/usr/bin/env bash
# Checks the crates conch-voice links against the RustSec advisory database.
# Not part of `make check`: it needs the network, and its result can change
# with no change in this repository. CI runs it as its own job.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
. scripts/voice-pins.sh
./scripts/voice-toolchain.sh deny
deny="${CONCH_CACHE:-$HOME/.cache/conch}/cargo-deny-$CONCH_DENY_VERSION/cargo-deny"
cd voice
"$deny" --locked check advisories
