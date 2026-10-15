#!/bin/sh
# Fixture for conch-adapter: stands in for `codex exec --json`. It reads the
# prompt from standard input and prints the events Codex prints for a run that
# runs one command and then answers, one JSON object per line. The answer is
# the last line of the prompt behind a label. It never starts a model.
prompt=$(cat)
last=$(printf '%s\n' "$prompt" | tail -n 1 | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\000-\037')
printf '%s\n' '{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"echo hello","status":"in_progress"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"echo hello","exit_code":0,"status":"completed"}}'
printf '{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"codex-stub: %s"}}\n' "$last"
