#!/usr/bin/env bash
# conch-agent.sh — unattended issue runner for the conch repo.
#
# Picks one open, unblocked GitHub issue (highest priority first), creates an
# isolated git worktree + branch, runs a headless Claude Code session to
# implement it per CLAUDE.md, and expects that session to push and open a PR.
# One issue = one branch = one PR = one session (CLAUDE.md rule 5).
#
# Usage:
#   conch-agent.sh            run one issue cycle (what the timer calls)
#   conch-agent.sh install    install + enable the systemd user timer
#   conch-agent.sh status     show timer/service status and last log
#
# Environment overrides:
#   CONCH_REPO             repo checkout (default: this script's repo)
#   CONCH_AGENT_LOG_DIR    logs + lock  (default: ~/.local/state/conch-agent)
#   CONCH_AGENT_TIMEOUT    per-session timeout (default: 3600 seconds)
#   CONCH_AGENT_MODEL      model for the session (default: claude-opus-4-8;
#                          e.g. claude-fable-5 for schema/approval design work)
#   CLAUDE_BIN             claude binary (default: claude)

set -euo pipefail

SCRIPT_PATH="$(readlink -f "${BASH_SOURCE[0]}")"
SCRIPT_DIR="$(dirname "$SCRIPT_PATH")"

REPO_DIR="${CONCH_REPO:-$(cd "$SCRIPT_DIR/.." && pwd)}"
LOG_DIR="${CONCH_AGENT_LOG_DIR:-$HOME/.local/state/conch-agent}"
LOCK_FILE="$LOG_DIR/agent.lock"
TIMEOUT="${CONCH_AGENT_TIMEOUT:-3600}"
CLAUDE_MODEL="${CONCH_AGENT_MODEL:-claude-opus-4-8}"
CLAUDE_BIN="${CLAUDE_BIN:-claude}"
IN_PROGRESS_LABEL="agent/in-progress"
WORKTREE_ROOT="$LOG_DIR/worktrees"

mkdir -p "$LOG_DIR" "$WORKTREE_ROOT"
LOG_FILE="$LOG_DIR/run-$(date +%Y%m%d-%H%M%S).log"

log() { printf '%s %s\n' "$(date -Is)" "$*" | tee -a "$LOG_FILE" >&2; }

# ---------------------------------------------------------------- subcommands

install_units() {
    local unit_dir="$HOME/.config/systemd/user"
    mkdir -p "$unit_dir" "$HOME/bin"
    ln -sfn "$SCRIPT_PATH" "$HOME/bin/conch-agent"
    cp "$SCRIPT_DIR/systemd/conch-agent.service" \
       "$SCRIPT_DIR/systemd/conch-agent.timer" "$unit_dir/"
    systemctl --user daemon-reload
    systemctl --user enable --now conch-agent.timer
    systemctl --user list-timers conch-agent.timer --no-pager
    echo "Installed. Logs land in $LOG_DIR"
}

show_status() {
    systemctl --user status conch-agent.timer conch-agent.service --no-pager || true
    local last
    last=$(ls -t "$LOG_DIR"/run-*.log 2>/dev/null | head -1 || true)
    [ -n "$last" ] && { echo; echo "== last log: $last =="; tail -30 "$last"; }
}

case "${1:-run}" in
    install) install_units; exit 0 ;;
    status)  show_status;   exit 0 ;;
    run)     ;;
    *)       echo "usage: $0 [run|install|status]" >&2; exit 2 ;;
esac

# ------------------------------------------------------------------ run cycle

# Single-instance guard: exit quietly if a previous run is still going.
exec 9>"$LOCK_FILE"
if ! flock -n 9; then
    log "another run holds $LOCK_FILE; exiting"
    exit 0
fi

cd "$REPO_DIR"
git fetch origin --prune

# Make sure the label used as an in-progress marker exists (idempotent).
gh label create "$IN_PROGRESS_LABEL" \
    --description "conch-agent is working this issue" --color BFDADC \
    2>/dev/null || true

# Pick one issue: open, not labeled blocked, not already being worked, and no
# still-open issue referenced in its "## Blocked by" section. Priority from
# the "P<n>" title prefix (P0 first), then oldest.
ISSUE=$(gh issue list --state open --limit 100 \
        --json number,title,labels,body |
    jq -r --arg wip "$IN_PROGRESS_LABEL" '
        . as $all
        | [ $all[].number ] as $open
        | [ $all[]
          | select(.labels | map(.name) | index("blocked") | not)
          | select(.labels | map(.name) | index($wip) | not)
          | select(
              (((.body // "") |
                capture("(?i)##\\s*blocked by(?<s>[\\s\\S]*?)(\\n##|$)") | .s) // "")
              | [scan("#([0-9]+)") | .[0] | tonumber]
              | map(. as $n | $open | index($n) != null) | any | not
            )
          | . + {prio: ((.title | capture("^P(?<p>[0-9])") | .p) // "9")}
        ]
        | sort_by(.prio, .number)
        | .[0].number // empty')

if [ -z "$ISSUE" ]; then
    log "no eligible open issues; nothing to do"
    exit 0
fi

# Skip if a PR already links this issue via a closing keyword ("Closes #12").
OPEN_PRS=$(gh issue view "$ISSUE" --json closedByPullRequestsReferences \
    --jq '.closedByPullRequestsReferences | length')
if [ "$OPEN_PRS" != "0" ]; then
    log "issue #$ISSUE already has an open PR; skipping"
    exit 0
fi

TITLE=$(gh issue view "$ISSUE" --json title --jq .title)
BRANCH="issue-$ISSUE/agent"
WORKTREE="$WORKTREE_ROOT/issue-$ISSUE"

log "picked issue #$ISSUE: $TITLE"
gh issue edit "$ISSUE" --add-label "$IN_PROGRESS_LABEL"

cleanup() {
    git -C "$REPO_DIR" worktree remove --force "$WORKTREE" 2>/dev/null || true
    git -C "$REPO_DIR" branch -D "$BRANCH" 2>/dev/null || true
}
trap cleanup EXIT

# Fresh branch off origin/main in an isolated worktree.
git worktree remove --force "$WORKTREE" 2>/dev/null || true
git branch -D "$BRANCH" 2>/dev/null || true
git worktree add "$WORKTREE" -b "$BRANCH" origin/main

PROMPT="You are running unattended in a dedicated git worktree on branch \
$BRANCH. Your task is GitHub issue #$ISSUE of this repository.

1. Read the issue: gh issue view $ISSUE --comments
2. Read CLAUDE.md and follow every rule there. Stay strictly within the
   issue's scope — one issue, one branch, one PR.
3. Implement the issue, including the tests its acceptance criteria require.
4. Run 'make check' and fix anything it reports until it passes.
5. Commit with a clear message, push the branch with
   'git push -u origin $BRANCH', and open a PR with 'gh pr create' whose body
   starts with 'Closes #$ISSUE', explains the change, and applies the issue's
   area/* labels (plus 'approval-path' if the approval path is touched).
6. If the issue is not executable as written (missing acceptance criteria,
   actually blocked), do NOT guess: comment your findings on the issue with
   'gh issue comment $ISSUE', add the 'blocked' label, and stop without
   pushing anything."

log "starting claude session (timeout ${TIMEOUT}s), log: $LOG_FILE"
set +e
( cd "$WORKTREE" &&
  timeout "$TIMEOUT" "$CLAUDE_BIN" -p "$PROMPT" \
      --model "$CLAUDE_MODEL" \
      --dangerously-skip-permissions \
      --output-format text ) >>"$LOG_FILE" 2>&1
STATUS=$?
set -e
log "claude session exited with status $STATUS"

# Verify the outcome: a PR from our branch must exist.
PR_URL=$(gh pr list --state open --head "$BRANCH" --json url --jq '.[0].url // empty')
if [ -n "$PR_URL" ]; then
    log "success: $PR_URL (issue #$ISSUE)"
else
    log "no PR created for issue #$ISSUE; releasing it for a future run"
    git push origin --delete "$BRANCH" 2>/dev/null || true
    gh issue edit "$ISSUE" --remove-label "$IN_PROGRESS_LABEL" || true
    exit 1
fi
