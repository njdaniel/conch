#!/bin/sh
# Fixture for conch-adapter: asks the adapter to wait, then replies with the
# last line of the prompt. The wait is the adapter's, through the @sleep marker,
# so the marker is only acted on with CONCH_ADAPTER_FIXTURE=1; the length is
# FIXTURE_SLEEP seconds (default 30), long enough for a test to cancel the run.
prompt=$(cat)
printf '@sleep %s\n' "${FIXTURE_SLEEP:-30}"
printf '%s\n' "$prompt" | tail -n 1
