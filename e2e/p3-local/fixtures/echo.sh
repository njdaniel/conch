#!/bin/sh
# Fixture for conch-adapter: replies with the last line of the prompt, which the
# adapter writes to standard input (set CONCH_ADAPTER_CMD to this script alone,
# without {prompt}). It is a deterministic stand-in for a model and never
# starts anything else.
prompt=$(cat)
printf '%s\n' "$prompt" | tail -n 1
