#!/usr/bin/env bash
# conch-agent.sh — unattended issue runner for the conch repo.
#
# Picks one open, unblocked GitHub issue (highest priority first) from those a
# human has approved with the "agent/todo" label, creates an isolated git
# worktree + branch, runs a headless Claude Code session to implement it per
# CLAUDE.md, and expects that session to push and open a PR.
# One issue = one branch = one PR = one session (CLAUDE.md rule 5).
#
# Work-state labels (two-tag model):
#   agent/todo     human-applied: approved / needs to be done (scope control —
#                  label just the current phase's issues)
#   agent/ready    auto-applied by sync: not labeled blocked and nothing in its
#                  "## Blocked by" section is still open (ready to work NOW)
#
# Usage:
#   conch-agent.sh            run one issue cycle (what the timer calls)
#   conch-agent.sh sync       refresh agent/ready labels from dependency state
#   conch-agent.sh queue      show the work queue: up for grabs, blocked, etc.
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
TODO_LABEL="agent/todo"
READY_LABEL="agent/ready"
WORKTREE_ROOT="$LOG_DIR/worktrees"
CLAIM_HELPER="$SCRIPT_DIR/issue-claim.sh"

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

# Make sure the labels the agent relies on exist (idempotent).
ensure_labels() {
    gh label create "$IN_PROGRESS_LABEL" \
        --description "conch-agent is working this issue" --color BFDADC \
        2>/dev/null || true
    if ! gh label create "$TODO_LABEL" \
        --description "approved for conch-agent — needs doing" --color 0E8A16 \
        2>/dev/null; then
        gh label edit "$TODO_LABEL" \
            --description "approved for conch-agent — needs doing" --color 0E8A16 >/dev/null
    fi
    if ! gh label create "$READY_LABEL" \
        --description "unblocked — ready to be worked now (auto-managed)" \
        --color BFD4F2 2>/dev/null; then
        gh label edit "$READY_LABEL" \
            --description "unblocked — ready to be worked now (auto-managed)" \
            --color BFD4F2 >/dev/null
    fi
}

fetch_open_issues() {
    gh issue list --state open --limit 100 --json number,title,labels,body
}

# Shared jq: still-open issues referenced in the "## Blocked by" section.
# The heading is anchored to a line start so a body that merely *mentions*
# "## Blocked by" in prose doesn't match. Expects $open (open issue numbers)
# bound in the caller's program.
# shellcheck disable=SC2016  # $open/$n are jq variables, not shell
JQ_BLOCKERS='
    (((.body // "") |
      capture("(?i)(^|\\n)##\\s*blocked by(?<s>[\\s\\S]*?)(\\n##|$)") | .s) // "")
    | [scan("#([0-9]+)") | .[0] | tonumber]
    | map(select(. as $n | $open | index($n) != null))'

# Reconcile the auto-managed $READY_LABEL with actual dependency state:
# present iff the issue is not labeled blocked and nothing in its
# "## Blocked by" section is still open. Prints each change; no-op when
# labels already match.
sync_ready() {
    local changes
    changes=$(fetch_open_issues | jq -r --arg ready "$READY_LABEL" "
        . as \$all
        | [ \$all[].number ] as \$open
        | \$all[]
        | (.labels | map(.name)) as \$lab
        | ((\$lab | index(\"blocked\") | not) and (($JQ_BLOCKERS) == [])) as \$should
        | if \$should and ((\$lab | index(\$ready)) | not) then \"add \(.number)\"
          elif (\$should | not) and (\$lab | index(\$ready)) then \"del \(.number)\"
          else empty end")
    if [ -z "$changes" ]; then
        printf 'sync: %s labels already match dependency state\n' "$READY_LABEL"
        return 0
    fi
    local op n
    while read -r op n; do
        case "$op" in
            add) gh issue edit "$n" --add-label "$READY_LABEL" >/dev/null
                 printf 'sync: +%s #%s\n' "$READY_LABEL" "$n" ;;
            del) gh issue edit "$n" --remove-label "$READY_LABEL" >/dev/null
                 printf 'sync: -%s #%s\n' "$READY_LABEL" "$n" ;;
        esac
    done <<<"$changes"
}

# Human view of the work queue: what is up for grabs (and parallel-safe),
# what is approved but blocked, what waits on approval, what is busy.
show_queue() {
    local issues linked claims n c
    issues=$(fetch_open_issues)
    # Include partially published claims whose label update failed. They are
    # reserved remotely even if issue metadata still looks unclaimed.
    claims=$(git ls-remote --refs origin 'refs/heads/agent-claims/issue-*' | \
        sed -n 's|.*refs/heads/agent-claims/issue-\([0-9][0-9]*\)$|\1|p' | jq -s .)
    linked="{}"
    for n in $(jq -r '.[].number' <<<"$issues"); do
        c=$(gh issue view "$n" --json closedByPullRequestsReferences \
            --jq '.closedByPullRequestsReferences | length')
        linked=$(jq --arg n "$n" --argjson c "$c" '. + {($n): $c}' <<<"$linked")
    done
    jq -r --arg todo "$TODO_LABEL" --arg wip "$IN_PROGRESS_LABEL" \
          --argjson linked "$linked" --argjson claims "$claims" "
        . as \$all
        | [ \$all[].number ] as \$open
        | [ \$all[]
            | (.labels | map(.name)) as \$lab
            | { number, title,
                prio: ((.title | capture(\"^P(?<p>[0-9])\") | .p) // \"9\"),
                areas: (\$lab | map(select(startswith(\"area/\")))),
                todo: ((\$lab | index(\$todo)) != null),
                wip: (((\$lab | index(\$wip)) != null)
                    or (.number as \$n | \$claims | index(\$n) != null)),
                haspr: ((\$linked[.number | tostring] // 0) > 0),
                blockers: ($JQ_BLOCKERS),
                blockedlab: ((\$lab | index(\"blocked\")) != null) }
            | . + {ready: ((.blockedlab | not) and (.blockers == []))}
          ]
        | sort_by(.prio, .number)
        | map(select((.wip or .haspr) | not)) as \$idle
        | def line: \"  #\(.number)  P\(.prio)  \(.title)\"
            + (if .areas != [] then \"  [\(.areas | join(\", \"))]\" else \"\" end);
          def blocked_by: if .blockedlab
              then \"  ← labeled blocked\"
                + (if .blockers != [] then \", blocked by \" + (.blockers | map(\"#\(.)\") | join(\" \")) else \"\" end)
              else \"  ← blocked by \" + (.blockers | map(\"#\(.)\") | join(\" \")) end;
          def section(name; items):
            \"\(name):\" + (if items == [] then \" (none)\" else \"\n\" + (items | join(\"\n\")) end);
          [ .[] | select(.todo and .ready and ((.wip or .haspr) | not)) ] as \$up
        | [ \$up[] as \$i
            | \$i + {clash: [ \$up[] | select(.number != \$i.number)
                | select([.areas[] | select(. as \$a | \$i.areas | index(\$a) != null)] != [])
                | .number ]}
          ] as \$upx
        | [
            section(\"Up for grabs (agent/todo + unblocked)\";
              [ \$upx[] | line
                + (if .clash != [] then \"  ⚠ same area as \" + (.clash | map(\"#\(.)\") | join(\" \")) + \" — serialize\" else \"\" end) ]),
            section(\"Approved, blocked (agent/todo, waiting on dependencies)\";
              [ \$idle[] | select(.todo and (.ready | not)) | line + blocked_by ]),
            section(\"Unblocked, awaiting approval (no agent/todo)\";
              [ \$idle[] | select((.todo | not) and .ready) | line ]),
            section(\"Blocked, awaiting approval\";
              [ \$idle[] | select((.todo | not) and (.ready | not)) | line + blocked_by ]),
            section(\"In progress / has open PR\";
              [ .[] | select(.wip or .haspr) | line
                + \"  (\" + ([(if .wip then \"in progress\" else empty end),
                             (if .haspr then \"has open PR\" else empty end)] | join(\", \")) + \")\" ])
          ]
        | join(\"\n\n\")" <<<"$issues"
}

case "${1:-run}" in
    install) install_units; exit 0 ;;
    status)  show_status;   exit 0 ;;
    sync)    cd "$REPO_DIR"; ensure_labels; sync_ready; exit 0 ;;
    queue)   cd "$REPO_DIR"; show_queue; exit 0 ;;
    run)     ;;
    *)       echo "usage: $0 [run|sync|queue|install|status]" >&2; exit 2 ;;
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

ensure_labels

# Keep the auto-managed ready label in step with dependency state each cycle.
sync_ready | tee -a "$LOG_FILE" >&2

# Candidate issues: opt-in only — a human must have applied $TODO_LABEL.
# Within that set: not labeled blocked, not already being worked, and no
# still-open issue referenced in the "## Blocked by" section. Priority from
# the "P<n>" title prefix (P0 first), then oldest. The gate is applied in jq
# (not gh --label) so $open still covers ALL open issues for the blocked-by
# check, including ones not yet approved.
CANDIDATES=$(fetch_open_issues |
    jq -r --arg wip "$IN_PROGRESS_LABEL" --arg todo "$TODO_LABEL" "
        . as \$all
        | [ \$all[].number ] as \$open
        | [ \$all[]
          | select(.labels | map(.name) | index(\$todo))
          | select(.labels | map(.name) | index(\"blocked\") | not)
          | select(.labels | map(.name) | index(\$wip) | not)
          | select(($JQ_BLOCKERS) == [])
          | . + {prio: ((.title | capture(\"^P(?<p>[0-9])\") | .p) // \"9\")}
        ]
        | sort_by(.prio, .number)
        | .[].number")

# Take the first candidate that no PR already links via a closing keyword
# ("Closes #12"), so an externally-opened PR doesn't stall the whole queue.
ISSUE=""
for cand in $CANDIDATES; do
    LINKED=$(gh issue view "$cand" --json closedByPullRequestsReferences \
        --jq '.closedByPullRequestsReferences | length')
    if [ "$LINKED" = "0" ]; then
        ISSUE=$cand
        break
    fi
    log "issue #$cand already has an open PR; trying next candidate"
done

if [ -z "$ISSUE" ]; then
    log "no eligible open issues; nothing to do"
    exit 0
fi

TITLE=$(gh issue view "$ISSUE" --json title --jq .title)
BRANCH="issue-$ISSUE/agent"
WORKTREE="$WORKTREE_ROOT/issue-$ISSUE"
CLAIM_AGENT="conch-agent:$CLAUDE_MODEL:$(date -u +%Y%m%dT%H%M%SZ):$$"
FILES=$(gh issue view "$ISSUE" --json body --jq .body | \
    awk '/^## Files/{p=1;next} /^## /{p=0} p')
[ -n "$FILES" ] || { log "issue #$ISSUE has no Files section; not dispatching"; exit 1; }
# Preserve a previous failed session for inspection. Never delete its work to
# make a new session fit; an explicit handoff/release must settle it first.
if [ -e "$WORKTREE" ] || git show-ref --verify --quiet "refs/heads/$BRANCH"; then
    log "existing work for #$ISSUE; inspect the claim and worktree before retrying"
    exit 1
fi

log "picked issue #$ISSUE: $TITLE"
bash "$CLAIM_HELPER" claim "$ISSUE" --agent "$CLAIM_AGENT" \
    --branch "$BRANCH" --files "$FILES" >>"$LOG_FILE" 2>&1 || {
    log "claim or Project update failed for #$ISSUE; no worker started; inspect issue-claim.sh status"
    exit 1
}
HAS_PR=false

cleanup() {
    if [ "$HAS_PR" = true ]; then
        git -C "$REPO_DIR" worktree remove "$WORKTREE" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Fresh branch off origin/main in an isolated worktree.
git worktree add "$WORKTREE" -b "$BRANCH" origin/main

PROMPT="You are running unattended in a dedicated git worktree on branch \
$BRANCH. Your task is GitHub issue #$ISSUE of this repository.
The dispatcher has already claimed it as $CLAIM_AGENT. The successful claim
receipt is in the runner log and issue comment. Do not acquire another claim,
remove its busy label, release it or change Project status yourself.

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
   'gh issue comment $ISSUE', add the 'blocked' label, remove the
   'agent/todo' label so the issue needs fresh approval once rewritten, and
   stop without pushing anything."

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
    HAS_PR=true
    bash "$CLAIM_HELPER" review "$ISSUE" --agent "$CLAIM_AGENT" >>"$LOG_FILE" 2>&1 || {
        log "PR exists but Review status failed; claim retained for reconciliation: $PR_URL"
        exit 1
    }
    log "success: $PR_URL (issue #$ISSUE)"
else
    log "no PR for #$ISSUE; claim and worktree retained, no automatic retry or branch deletion"
    log "inspect with: bash $CLAIM_HELPER status $ISSUE; session=$CLAIM_AGENT"
    exit 1
fi
