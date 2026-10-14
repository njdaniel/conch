#!/bin/sh
# Fixture for conch-adapter: reads the prompt, then keeps a process of its own
# alive for FIXTURE_SLEEP seconds (default 30) before it replies with the last
# line of the prompt. Unlike slow.sh, whose wait is the adapter's, this leaves
# a real child process for a cancel to kill. Used by the automated suite only.
prompt=$(cat)
sleep "${FIXTURE_SLEEP:-30}"
printf '%s\n' "$prompt" | tail -n 1
