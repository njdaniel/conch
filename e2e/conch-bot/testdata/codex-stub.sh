#!/bin/sh
# Stand-in for `codex exec --json` for the end-to-end check: reads the prompt
# from standard input as codex does, ignores its arguments, and prints the
# events recorded from one real run (internal/adapter/testdata/codex-events.jsonl).
# It never starts anything else.
cat >/dev/null
exec cat "$(dirname "$0")/../../../internal/adapter/testdata/codex-events.jsonl"
