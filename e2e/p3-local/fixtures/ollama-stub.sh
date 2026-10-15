#!/bin/sh
# Fixture for conch-adapter: stands in for `ollama run <model>`. It reads the
# prompt from standard input and prints plain text, as ollama does: the last
# line of the prompt behind a label, so a reply says which runtime made it.
# It never starts a model or reaches a network.
prompt=$(cat)
printf 'ollama-stub: %s\n' "$(printf '%s\n' "$prompt" | tail -n 1)"
