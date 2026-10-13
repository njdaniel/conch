#!/usr/bin/env bash
# principal-review.sh — the principal-review loop on a schedule, reporting
# into Conch.
#
# `run` starts a headless Claude Code session that follows the
# principal-review skill (.claude/skills/principal-review/SKILL.md) in its own
# git worktree, on a branch principal-review/YYYY-MM-DD off origin/main. The
# session writes docs/reports/YYYY-MM-DD-principal-review.md; this script then
# commits that one file, pushes the branch, opens a PR ("Refs #26") and posts
# a summary of the report into Conch with `conch send`. It never commits to
# main and never merges.
#
# `post` is the last step on its own: it posts the summary of one report. The
# summary is the report's title, one line per "## " section with its item
# count, the "Decisions needed from Nick" section in full, and the PR link.
#
# The timer is a convenience. Everything here works by hand, and conchd
# neither knows nor cares that this script exists (ADR-002).
#
# Usage:
#   principal-review.sh                  run one review (what the timer calls)
#   principal-review.sh run              the same
#   principal-review.sh post <report.md> [<pr-url>]
#                                        post one report's summary into Conch
#   principal-review.sh install          install + enable the systemd user timer
#   principal-review.sh status           show timer/service status and last log
#
# Exit status:
#   0  done; also "a report for today already exists", which is not an error
#   1  the review failed: the lock stayed busy, the session failed or timed
#      out, it wrote no report, or the commit, push or PR failed
#   2  usage
#   3  the summary was not posted (conchd unreachable, no login, or the
#      channel refused it). After `run` the report and its PR still exist;
#      nothing is retried, the next review posts its own report.
#
# Environment overrides:
#   CONCH_REPO              repo checkout (default: this script's repo)
#   CONCH_REVIEW_LOG_DIR    logs + worktrees
#                           (default: ~/.local/state/conch-principal-review)
#   CONCH_REVIEW_TIMEOUT    session timeout (default: 3600 seconds)
#   CONCH_REVIEW_MODEL      model for the session (default: claude-fable-5-1)
#   CONCH_REVIEW_CHANNEL    channel the summary is posted to
#                           (default: principal-review)
#   CONCH_REVIEW_LOCK_WAIT  how long `run` waits for the lock it shares with
#                           conch-agent (default: 5400 seconds; an issue run's
#                           session is limited to 3600)
#   CONCH_AGENT_LOG_DIR     where conch-agent keeps that lock
#                           (default: ~/.local/state/conch-agent)
#   CLAUDE_BIN              claude binary (default: claude)
#   CONCH_BIN               conch binary (default: conch)
#   CONCH_SERVER            read by conch itself: the server to post to. The
#                           summary is posted as whatever principal
#                           `conch login` stored for that server.

set -euo pipefail

SCRIPT_PATH="$(readlink -f "${BASH_SOURCE[0]}")"
SCRIPT_DIR="$(dirname "$SCRIPT_PATH")"

REPO_DIR="${CONCH_REPO:-$(cd "$SCRIPT_DIR/.." && pwd)}"
LOG_DIR="${CONCH_REVIEW_LOG_DIR:-$HOME/.local/state/conch-principal-review}"
# conch-agent's lock, at the path conch-agent.sh derives it from: a review and
# an issue run must never work in the repository at once.
LOCK_FILE="${CONCH_AGENT_LOG_DIR:-$HOME/.local/state/conch-agent}/agent.lock"
LOCK_WAIT="${CONCH_REVIEW_LOCK_WAIT:-5400}"
TIMEOUT="${CONCH_REVIEW_TIMEOUT:-3600}"
CLAUDE_MODEL="${CONCH_REVIEW_MODEL:-claude-fable-5-1}"
CHANNEL="${CONCH_REVIEW_CHANNEL:-principal-review}"
CLAUDE_BIN="${CLAUDE_BIN:-claude}"
CONCH_BIN="${CONCH_BIN:-conch}"
WORKTREE_ROOT="$LOG_DIR/worktrees"
EXIT_NOT_POSTED=3

# The size the summary is cut to, counted as conch will encode it: the bytes
# of the JSON string that carries the text. Three limits apply, and the
# smallest decides:
#   - conchd refuses a request body over maxMessageBodyBytes, 1 MiB
#     (internal/server/messages.go). That is the JSON request, not the text.
#   - `conch send` takes the text as one argument, and Linux passes at most
#     MAX_ARG_STRLEN = 131072 bytes in one argument.
#   - conch tail and the TUI read each WebSocket message with the library's
#     default limit of 32768 bytes (internal/cli sets none), and a message
#     over it closes their connection.
# 30000 leaves the rest of 32768 for the envelope around the body. The first
# two limits hold whatever the report contains: the text is never longer than
# its encoding, and no byte encodes to more than six.
SUMMARY_BUDGET=30000

mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/review-$(date +%Y%m%d-%H%M%S).log"

log() { printf '%s %s\n' "$(date -Is)" "$*" | tee -a "$LOG_FILE" >&2; }

usage() {
    echo "usage: $0 [run|post <report.md> [<pr-url>]|install|status]" >&2
    exit 2
}

# ---------------------------------------------------------------- subcommands

install_units() {
    local unit_dir="$HOME/.config/systemd/user"
    mkdir -p "$unit_dir" "$HOME/bin"
    ln -sfn "$SCRIPT_PATH" "$HOME/bin/conch-principal-review"
    cp "$SCRIPT_DIR/systemd/conch-principal-review.service" \
       "$SCRIPT_DIR/systemd/conch-principal-review.timer" "$unit_dir/"
    systemctl --user daemon-reload
    systemctl --user enable --now conch-principal-review.timer
    systemctl --user list-timers conch-principal-review.timer --no-pager
    echo "Installed. Logs land in $LOG_DIR"
}

show_status() {
    systemctl --user status conch-principal-review.timer \
        conch-principal-review.service --no-pager || true
    # Log names carry their start time, so the last in name order is the latest.
    local f last=""
    for f in "$LOG_DIR"/review-*.log; do
        if [ -e "$f" ]; then last=$f; fi
    done
    if [ -n "$last" ]; then
        echo
        echo "== last log: $last =="
        tail -30 "$last"
    fi
}

# ------------------------------------------------------------------- summary

# The report is text someone else wrote, and it ends up as one argument of
# `conch send`. Nothing in this file may let it be read as code or as options:
# it reaches awk on standard input only (an awk file operand containing "="
# is an assignment, and -v interprets backslashes), the other strings reach
# awk through the environment, and the result is only ever expanded inside
# double quotes.
#
# Counting is in bytes (LC_ALL=C). cost() is the size of a line and its
# newline inside a JSON string as Go's encoding/json writes it: two bytes for
# a quote, a backslash or the newline, six for <, > and & (written \u003c and
# so on), for a control character, and for U+2028 and U+2029. Tab and carriage
# return are over-counted. Bytes that are not valid UTF-8 are not seen here;
# conch replaces each with U+FFFD, so such a report is not delivered byte for
# byte and can exceed the budget, though never the two hard limits above.
# shellcheck disable=SC2016  # $0 and the rest are awk's, not the shell's
SUMMARY_AWK='
function cost(s,    t, n) {
    t = s
    n = length(t) + 2
    n += gsub(/["\\]/, "&", t)
    n += 5 * gsub(/[<>&]/, "&", t)
    n += 5 * gsub(/[\001-\037]/, "&", t)
    n += 3 * gsub(/\342\200[\250\251]/, "&", t)
    return n
}
function trimmed(s) {
    sub(/[ \t]+$/, "", s)
    return s
}
function counted(n) {
    if (n == 0) return "no list items"
    if (n == 1) return "1 item"
    return n " items"
}
function cut_notice(n) {
    return "[cut here to fit one Conch message; lines left in the report: " n "]"
}
{
    line[NR] = $0
    probe = $0
    sub(/\r$/, "", probe)
    # A fenced block can hold anything, "## " lines included.
    if (probe ~ /^(```|~~~)/) { fence = !fence; rows = 0; next }
    if (fence) next
    if (probe ~ /^# /) {
        if (title == "") title = trimmed(substr(probe, 3))
        if (ns > 0 && !(ns in last)) last[ns] = NR - 1
        next
    }
    if (probe ~ /^## /) {
        if (ns > 0 && !(ns in last)) last[ns] = NR - 1
        ns++
        name[ns] = trimmed(substr(probe, 4))
        first[ns] = NR + 1
        items[ns] = 0
        rows = 0
        next
    }
    if (ns == 0 || (ns in last)) next
    # An item is a top-level list entry or a table row below the header and
    # its rule. Nested entries belong to the item above them.
    if (probe ~ /^([-*+]|[0-9]+[.)])[ \t]/) items[ns]++
    if (probe ~ /^\|/) { if (++rows > 2) items[ns]++ } else rows = 0
}
END {
    if (ns > 0 && !(ns in last)) last[ns] = NR
    if (title == "") title = ENVIRON["REVIEW_FALLBACK_TITLE"]

    # The skill names the section "Decisions needed from Nick"; the first
    # report (2026-07-12) has "action items" instead. "Decisions taken" in
    # that same report must not match.
    d = 0
    for (i = 1; i <= ns && !d; i++) if (tolower(name[i]) ~ /decisions needed/) d = i
    for (i = 1; i <= ns && !d; i++) if (tolower(name[i]) ~ /action items/) d = i

    n = 0
    out[++n] = title
    if (ns > 0) {
        out[++n] = ""
        out[++n] = "Sections:"
        for (i = 1; i <= ns; i++) out[++n] = "- " name[i] ": " counted(items[i])
    }
    out[++n] = ""
    if (d) {
        out[++n] = name[d] ":"
        a = first[d]
        b = last[d]
        while (a <= b && line[a] ~ /^[ \t\r]*$/) a++
        while (b >= a && line[b] ~ /^[ \t\r]*$/) b--
        if (a > b) out[++n] = "(none listed)"
        for (j = a; j <= b; j++) out[++n] = line[j]
    } else {
        out[++n] = "Decisions needed from Nick: the report has no such section"
    }

    pr = ENVIRON["REVIEW_PR_URL"]
    budget = ENVIRON["REVIEW_BUDGET"] + 0
    if (pr != "") budget -= cost("") + cost("PR: " pr)

    total = 0
    for (k = 1; k <= n; k++) total += cost(out[k])
    keep = n
    if (total > budget) {
        # Cut on a line boundary, last lines first, which is the end of the
        # decisions list. The notice is sized for the largest count it can name.
        budget -= cost(cut_notice(n))
        used = 0
        for (keep = 0; keep < n; keep++) {
            used += cost(out[keep + 1])
            if (used > budget) break
        }
    }
    for (k = 1; k <= keep; k++) print out[k]
    if (keep < n) print cut_notice(n - keep)
    if (pr != "") { print ""; print "PR: " pr }
}'

# summarise <report.md> [<pr-url>] prints the summary. NUL bytes cannot be in
# an argument, so they are dropped here rather than by the shell with a warning.
summarise() {
    local fallback
    fallback=$(basename -- "$1")
    tr -d '\000' <"$1" |
        REVIEW_PR_URL="${2:-}" REVIEW_BUDGET="$SUMMARY_BUDGET" \
        REVIEW_FALLBACK_TITLE="$fallback" LC_ALL=C awk "$SUMMARY_AWK"
}

# ---------------------------------------------------------------------- post

# What conch said, as one printable line: its text can quote a server's own
# words, which are not safe to put on a terminal as they are.
one_line() {
    printf '%s' "$1" | tr '\n' ' ' | tr -d '\000-\037\177' | head -c 300
}

not_posted() { log "not posted: $*"; }

# post_report <report.md> [<pr-url>] posts the summary and returns 0, or says
# in one line why it could not and returns $EXIT_NOT_POSTED. It reads no
# credential and prints none: conch finds the stored login itself, and only
# conch's error text is ever shown.
post_report() {
    local report=$1 pr_url=${2:-} server summary err
    server=${CONCH_SERVER:-http://127.0.0.1:8080}

    if ! summary=$(summarise "$report" "$pr_url") || [ -z "$summary" ]; then
        not_posted "could not summarise $report"
        return "$EXIT_NOT_POSTED"
    fi
    if ! command -v "$CONCH_BIN" >/dev/null 2>&1; then
        not_posted "'$CONCH_BIN' is not on PATH"
        return "$EXIT_NOT_POSTED"
    fi
    # Ask who we are first, so the three ways of failing can be told apart:
    # conch answers a missing or rejected login with its "not logged in to
    # <server>" line (internal/cli, UnauthenticatedError), and anything else
    # that stops whoami is the server not answering. e2e/reviewpost holds
    # this to the real binaries.
    if ! err=$("$CONCH_BIN" whoami 2>&1 >/dev/null </dev/null); then
        case "$err" in
            *"not logged in to"*)
                not_posted "no stored login for $server (run 'conch login')" ;;
            *)
                not_posted "conchd unreachable at $server: $(one_line "$err")" ;;
        esac
        return "$EXIT_NOT_POSTED"
    fi
    # "--" ends conch's options, so neither a channel nor a summary that
    # starts with "-" can be taken for one.
    if ! err=$("$CONCH_BIN" send -- "$CHANNEL" "$summary" 2>&1 </dev/null); then
        not_posted "cannot post to channel '$CHANNEL' as the stored login: $(one_line "$err")"
        return "$EXIT_NOT_POSTED"
    fi
    log "posted the summary of $report to channel '$CHANNEL'"
}

post_command() {
    local report=$1 pr_url=${2:-}
    if [ ! -f "$report" ] || [ ! -r "$report" ]; then
        echo "$0: post: cannot read report '$report'" >&2
        exit 2
    fi
    # The link is one line of the summary; anything else is a mistake.
    case "$pr_url" in
        *[[:space:]]*) echo "$0: post: <pr-url> must not contain whitespace" >&2; exit 2 ;;
    esac
    if [ "${#pr_url}" -gt 2000 ]; then
        echo "$0: post: <pr-url> is too long" >&2
        exit 2
    fi
    post_report "$report" "$pr_url" || exit "$EXIT_NOT_POSTED"
}

# ----------------------------------------------------------------- run cycle

fail() { log "$*"; exit 1; }

# After the session a report exists that an hour of work went into. If it
# cannot be committed or pushed, the worktree still goes, so keep the file.
keep_report() {
    local kept="$LOG_DIR/$DATE-principal-review.md"
    if cp "$WORKTREE/$REPORT_REL" "$kept" 2>/dev/null; then
        log "the report is kept at $kept"
    fi
}

cleanup() {
    git -C "$REPO_DIR" worktree remove --force "$WORKTREE" 2>/dev/null || true
    git -C "$REPO_DIR" branch -D "$BRANCH" 2>/dev/null || true
}

run_review() {
    mkdir -p "$(dirname "$LOCK_FILE")" "$WORKTREE_ROOT"

    # conch-agent runs three times a day and skips a firing when the lock is
    # held. A review that did the same would be gone for a week, so it waits
    # for the issue run to finish instead.
    exec 9>"$LOCK_FILE"
    if ! flock -w "$LOCK_WAIT" 9; then
        fail "another run still holds $LOCK_FILE after ${LOCK_WAIT}s; no review"
    fi

    cd "$REPO_DIR"
    git fetch origin --prune >>"$LOG_FILE" 2>&1 || fail "git fetch origin failed; no review"

    DATE=$(date +%F)
    BRANCH="principal-review/$DATE"
    REPORT_REL="docs/reports/$DATE-principal-review.md"
    WORKTREE="$WORKTREE_ROOT/principal-review-$DATE"

    # One review a day. Only main and the pushed branch count: a local branch
    # left by a run that died is not a report anyone can read.
    local ref
    for ref in origin/main "origin/$BRANCH"; do
        if git cat-file -e "$ref:$REPORT_REL" 2>/dev/null; then
            log "$REPORT_REL already exists on $ref; nothing to do"
            exit 0
        fi
    done

    trap cleanup EXIT

    # Fresh branch off origin/main in an isolated worktree.
    git worktree remove --force "$WORKTREE" 2>/dev/null || true
    git branch -D "$BRANCH" 2>/dev/null || true
    git worktree add "$WORKTREE" -b "$BRANCH" origin/main >>"$LOG_FILE" 2>&1 ||
        fail "could not create the worktree $WORKTREE; no review"
    # The session may fetch, which moves origin/main; the base must not move.
    local base
    base=$(git rev-parse origin/main)

    local prompt="You are running unattended in a dedicated git worktree on branch \
$BRANCH. Your task is this repository's principal review for $DATE.

1. Read CLAUDE.md and follow every rule there.
2. Read .claude/skills/principal-review/SKILL.md and carry out its procedure,
   steps 1 to 5, as the principal-review skill describes them.
3. Write the report to exactly this path: $REPORT_REL
   Give it the sections step 5 names, each under a '## ' heading. The last
   must be '## Decisions needed from Nick': an explicit list, one decision
   per item, even if it is empty.
4. Change no other file. Do NOT commit, push, open a pull request, merge
   anything or post to Conch: the runner (ops/principal-review.sh) commits
   the report, pushes this branch, opens the PR and posts the summary once
   you exit. Step 6 of the skill is the runner's.
5. If you cannot complete the review, write no report, say why, and stop."

    log "starting claude session (timeout ${TIMEOUT}s), log: $LOG_FILE"
    local status
    set +e
    ( cd "$WORKTREE" &&
      timeout "$TIMEOUT" "$CLAUDE_BIN" -p "$prompt" \
          --model "$CLAUDE_MODEL" \
          --dangerously-skip-permissions \
          --output-format text ) >>"$LOG_FILE" 2>&1
    status=$?
    set -e
    log "claude session exited with status $status"

    if [ "$status" -eq 124 ]; then
        fail "session timed out after ${TIMEOUT}s; no PR, nothing posted"
    elif [ "$status" -ne 0 ]; then
        fail "session failed (exit status $status); no PR, nothing posted"
    fi
    if [ ! -s "$WORKTREE/$REPORT_REL" ]; then
        fail "session wrote no $REPORT_REL; no PR, nothing posted"
    fi

    # Commit the report and nothing else. The pre-commit hook is skipped: it
    # checks Go and Rust sources, this commit is one Markdown file (checked
    # just below), and the hook needs a toolchain the timer's PATH does not
    # have. CI runs the same checks on the PR.
    if ! git -C "$WORKTREE" add -- "$REPORT_REL" >>"$LOG_FILE" 2>&1; then
        keep_report
        fail "could not stage $REPORT_REL; no PR, nothing posted"
    fi
    # Nothing staged means the session committed the report itself, against
    # its instructions; what it committed is checked like any other.
    if ! git -C "$WORKTREE" diff --cached --quiet -- "$REPORT_REL"; then
        if ! git -C "$WORKTREE" commit --no-verify -q \
                -m "docs: principal review $DATE" -m "Refs #26" \
                -- "$REPORT_REL" >>"$LOG_FILE" 2>&1; then
            keep_report
            fail "could not commit $REPORT_REL; no PR, nothing posted"
        fi
    fi
    local changed
    changed=$(git -C "$WORKTREE" diff --name-only "$base" HEAD 2>>"$LOG_FILE") || changed=""
    if [ "$changed" != "$REPORT_REL" ]; then
        keep_report
        fail "the branch changes more than the report ($(one_line "$changed")); no PR, nothing posted"
    fi

    if ! git -C "$WORKTREE" push -u origin "$BRANCH" >>"$LOG_FILE" 2>&1; then
        keep_report
        fail "could not push $BRANCH; no PR, nothing posted"
    fi

    local pr_body="The principal review for $DATE, written by an unattended session following
.claude/skills/principal-review and opened by ops/principal-review.sh.
Its \"Decisions needed from Nick\" section is the part to read first.

Refs #26"
    local pr_out pr_url
    if ! pr_out=$(cd "$WORKTREE" && gh pr create --base main --head "$BRANCH" \
            --title "Principal review $DATE" --body "$pr_body" 2>>"$LOG_FILE"); then
        # The branch stays: it holds the only pushed copy of the report.
        fail "pushed $BRANCH but could not open its PR; open it by hand. Nothing posted"
    fi
    pr_url=$(printf '%s\n' "$pr_out" | grep -Eo 'https?://[^[:space:]]+' | tail -n 1 || true)
    if [ -n "$pr_url" ]; then
        log "opened $pr_url"
    else
        log "gh pr create printed no URL; the summary will carry no PR link"
    fi

    if ! post_report "$WORKTREE/$REPORT_REL" "$pr_url"; then
        log "the report and its PR exist; the post is not retried"
        exit "$EXIT_NOT_POSTED"
    fi
    log "success: review $DATE"
}

case "${1:-run}" in
    install) [ $# -le 1 ] || usage; install_units ;;
    status)  [ $# -le 1 ] || usage; show_status ;;
    post)    { [ $# -ge 2 ] && [ $# -le 3 ]; } || usage
             post_command "$2" "${3:-}" ;;
    run)     [ $# -le 1 ] || usage; run_review ;;
    *)       usage ;;
esac
