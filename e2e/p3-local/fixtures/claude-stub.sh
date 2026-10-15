#!/bin/sh
# Fixture for conch-adapter: stands in for `claude -p`. It reads the prompt from
# standard input and prints the last line of it behind a label, so a reply says
# which runtime made it. It never starts a model.
prompt=$(cat)
printf 'claude-stub: %s\n' "$(printf '%s\n' "$prompt" | tail -n 1)"
