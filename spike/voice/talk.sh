#!/usr/bin/env bash
# Live voice test: ./talk.sh <identity> [server-host] [ptt]
#   ptt = always (default) or /dev/input/eventN:KEYCODE for hold-to-talk (97 = Right Ctrl)
# Run it as a different identity on each machine/terminal. Wear headphones unless apm=1.
cd "$(dirname "$0")"
id=${1:?identity}; host=${2:-127.0.0.1}; ptt=${3:-always}
exec lkspike/target/release/lkspike talk url="ws://$host:7880" \
  token="$(lkctl/lkctl token -room talk -identity "$id")" ptt="$ptt" secs=${SECS:-120} apm=${APM:-0}
