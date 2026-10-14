#!/bin/sh
# Fixture for conch-adapter: asks the adapter to raise an approval, then replies
# with the last line of the prompt. The marker is only acted on with
# CONCH_ADAPTER_FIXTURE=1; without it the line is part of the reply. The reply
# follows whatever the decision was.
prompt=$(cat)
printf '@approval %s\n' "${FIXTURE_APPROVAL_TITLE:-Approve the fixture action}"
printf '%s\n' "$prompt" | tail -n 1
