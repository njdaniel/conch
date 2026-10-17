#!/bin/sh
# Fixture for conch-adapter: reads the prompt and exits with status 3, so a run
# ends failed with the outcome "exit 3" and nothing is posted.
cat >/dev/null
exit 3
