#!/usr/bin/env bash
# Docs-only pull requests (issue #1300). Reads the changed paths of a pull
# request on stdin, one per line, and prints "true" when every one is
# documentation, else "false". CI's changes job uses the answer to run a
# light check instead of the whole workflow.
#
# A path is documentation when it is under docs/ or ends in .md, except:
#   - anything under a testdata/ directory (test inputs);
#   - the docs a test reads (READ_BY_TESTS below; ci-docs-only-test.sh fails
#     when a test reads one that is not listed);
#   - anything under .github/ that is not Markdown (workflows, actions).
# No paths at all is "false": when in doubt, run everything.
set -euo pipefail

# Docs that tests read, as exact paths or as directory prefixes ending in /.
READ_BY_TESTS=(
    docs/deploy.md                 # scripts/deploy-scripts-test.sh
    docs/design/conch-voice.md     # pkg/schema/voice_devices_test.go
    docs/design/tui-voice.md       # pkg/schema/voice_control_test.go
    docs/p3-walkthrough.md         # e2e/p3-local/main_test.go
    docs/reports/                  # e2e/reviewpost/post_linux_test.go
)

read_by_tests() {
    local p=$1 r
    for r in "${READ_BY_TESTS[@]}"; do
        case $r in
            */) [[ $p == "$r"* ]] && return 0 ;;
            *) [[ $p == "$r" ]] && return 0 ;;
        esac
    done
    return 1
}

is_doc() {
    local p=$1
    [[ $p == testdata/* || $p == */testdata/* ]] && return 1
    read_by_tests "$p" && return 1
    [[ $p == docs/* || $p == *.md ]]
}

if [ "${1:-}" = "--read-by-tests" ]; then
    printf '%s\n' "${READ_BY_TESTS[@]}"
    exit 0
fi

seen=0
while IFS= read -r path; do
    [ -z "$path" ] && continue
    seen=1
    if ! is_doc "$path"; then
        echo false
        exit 0
    fi
done
if [ "$seen" -eq 0 ]; then echo false; else echo true; fi
