#!/usr/bin/env bash
# Exercise real Git leases across separate clones. gh and the model are local
# fixtures; this suite must never contact GitHub or run an actual AI session.
set -euo pipefail
source_root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
real_git=$(command -v git)
mkdir -p "$tmp/bin"
export REAL_GIT="$real_git"
export PATH="$tmp/bin:$PATH"

cat > "$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
exec 9>"$MOCK/gh.lock"
flock 9
printf '%q ' "$@" >> "$MOCK/calls"
printf '\n' >> "$MOCK/calls"
value() {
    local flag=$1; shift
    while [[ $# -gt 0 ]]; do
        if [[ $1 == "$flag" ]]; then printf '%s' "$2"; return; fi
        shift
    done
}
json_result() {
    local data=$1; shift
    local query; query=$(value --jq "$@")
    if [[ -n $query ]]; then jq -r "$query" <<<"$data"; else printf '%s\n' "$data"; fi
}
case "$1 $2" in
    'repo view') printf 'njdaniel/conch\n' ;;
    'label create') exit 0 ;;
    'issue view')
        if [[ " $* " == *' --comments '* ]]; then cat "$MOCK/issue.json";
        else json_result "$(cat "$MOCK/issue.json")" "$@"; fi ;;
    'issue list') jq '[select(.state == "OPEN")]' "$MOCK/issue.json" ;;
    'issue edit')
        add=$(value --add-label "$@"); remove=$(value --remove-label "$@")
        [[ $add != agent/in-progress || ! -f "$MOCK/fail-label" ]] || exit 1
        jq --arg add "$add" --arg remove "$remove" \
            '.labels |= (map(select(.name != $remove))
              | if $add == "" or any(.[]; .name == $add) then . else . + [{name:$add}] end)' \
            "$MOCK/issue.json" > "$MOCK/next"
        mv "$MOCK/next" "$MOCK/issue.json" ;;
    'issue comment')
        body=$(cat)
        jq --arg body "$body" '.comments += [{body:$body}]' "$MOCK/issue.json" > "$MOCK/next"
        mv "$MOCK/next" "$MOCK/issue.json" ;;
    'project view')
        [[ ! -f "$MOCK/fail-project" ]] || { echo 'missing project scope' >&2; exit 1; }
        printf '{"id":"project-discovered"}\n' ;;
    'project field-list') cat "$MOCK/fields.json" ;;
    'project item-add') printf '{"id":"item-discovered"}\n' ;;
    'project item-edit')
        [[ ! -f "$MOCK/fail-edit" ]] || exit 1
        [[ $(value --project-id "$@") == project-discovered ]]
        [[ $(value --field-id "$@") == field-discovered ]]
        [[ $(value --id "$@") == item-discovered ]]
        jq -n --arg option "$(value --single-select-option-id "$@")" '{option:$option}' > "$MOCK/board.json" ;;
    'api graphql')
        if [[ " $* " == *'projectItems('* ]]; then
            option=$(jq -r .option "$MOCK/board.json")
            name=$(jq -r --arg option "$option" '.fields[].options[]? | select(.id == $option) | .name' "$MOCK/fields.json")
            jq -n --arg name "$name" '{nodes:[{project:{id:"project-discovered"},fieldValueByName:{name:$name}}],pageInfo:{hasNextPage:false}}'
        elif [[ -f "$MOCK/fail-readback" ]]; then printf 'wrong-option\n';
        else jq -r .option "$MOCK/board.json"; fi ;;
    'pr list')
        state=$(value --state "$@"); head=$(value --head "$@")
        data='[]'
        [[ ! -f "$MOCK/prs-$state.json" ]] || data=$(cat "$MOCK/prs-$state.json")
        data=$(jq --arg head "$head" 'map(select(.headRefName == $head))' <<<"$data")
        json_result "$data" "$@" ;;
    *) printf 'unexpected gh call: %s\n' "$*" >&2; exit 90 ;;
esac
STUB
cat > "$tmp/bin/git" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == push && -n ${TEST_RACE_DIR:-} && " $* " == *'--force-with-lease=refs/heads/agent-claims/issue-42: '* ]]; then
    touch "$TEST_RACE_DIR/$TEST_CLIENT"
    ready=false
    for _ in $(seq 1 250); do
        if [[ -f "$TEST_RACE_DIR/a" && -f "$TEST_RACE_DIR/b" ]]; then ready=true; break; fi
        sleep 0.02
    done
    [[ $ready == true ]] || { echo 'race barrier timed out' >&2; exit 91; }
fi
exec "$REAL_GIT" "$@"
STUB
cat > "$tmp/bin/claude" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'started\n' >> "$MOCK/model-started"
jq -e 'any(.labels[]; .name == "agent/in-progress")' "$MOCK/issue.json" >/dev/null
[[ $(jq -r .option "$MOCK/board.json") == opt-busy ]]
[[ -n $(git ls-remote --refs origin refs/heads/agent-claims/issue-42) ]]
[[ " $* " == *'dispatcher has already claimed'* ]]
if [[ -f "$MOCK/model-pr" ]]; then
    printf '[{"number":99,"url":"https://github.com/njdaniel/conch/pull/99","headRefName":"issue-42/agent"}]\n' > "$MOCK/prs-open.json"
fi
STUB
chmod +x "$tmp/bin/gh" "$tmp/bin/git" "$tmp/bin/claude"

case_n=0
setup_case() {
    case_n=$((case_n + 1))
    case_dir="$tmp/case-$case_n"
    mkdir -p "$case_dir/mock"
    export MOCK="$case_dir/mock"
    "$real_git" init -q --bare --initial-branch=main "$case_dir/origin.git"
    "$real_git" clone -q "$case_dir/origin.git" "$case_dir/seed" 2>/dev/null
    printf 'fixture\n' > "$case_dir/seed/README"
    "$real_git" -C "$case_dir/seed" add README
    "$real_git" -C "$case_dir/seed" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm seed
    "$real_git" -C "$case_dir/seed" push -q origin main
    for client in a b; do
        "$real_git" clone -q "$case_dir/origin.git" "$case_dir/$client"
        mkdir -p "$case_dir/$client/ops"
        cp "$source_root/ops/issue-claim.sh" "$source_root/ops/conch-agent.sh" "$case_dir/$client/ops/"
    done
    printf '%s\n' '{"id":"fixture-issue","number":42,"title":"P0 fixture","state":"OPEN","url":"https://github.com/njdaniel/conch/issues/42","body":"## Files\n- src/fixture\n\n## Blocked by\nNothing.","labels":[],"comments":[],"closedByPullRequestsReferences":[]}' > "$MOCK/issue.json"
    printf '{"fields":[{"id":"field-discovered","name":"Status","options":[{"id":"opt-busy","name":"In Progress"},{"id":"opt-review","name":"Review"},{"id":"opt-backlog","name":"Backlog"},{"id":"opt-test","name":"Ready to test"}]}]}\n' > "$MOCK/fields.json"
    printf '{"option":"opt-backlog"}\n' > "$MOCK/board.json"
    : > "$MOCK/calls"
}
claim() { (cd "$case_dir/$1" && bash ops/issue-claim.sh claim 42 --agent "$2" --branch "issue-42/$1" --files 'src/fixture'); }
transition() { local client=$1; shift; (cd "$case_dir/$client" && bash ops/issue-claim.sh "$@"); }
ref_sha() { "$real_git" --git-dir="$case_dir/origin.git" rev-parse --verify refs/heads/agent-claims/issue-42 2>/dev/null || true; }
reject() { if "$@" > "$MOCK/rejected" 2>&1; then echo "FAIL: unexpectedly succeeded: $*" >&2; exit 1; fi; }
assert_option() { [[ $(jq -r .option "$MOCK/board.json") == "$1" ]]; }
edit_fixture() { jq "$1" "$MOCK/issue.json" > "$MOCK/next"; mv "$MOCK/next" "$MOCK/issue.json"; }

setup_case
claim a session-a > "$MOCK/claim"
[[ -n $(ref_sha) ]]
jq -e 'any(.labels[]; .name == "agent/in-progress") and (.comments[0].body | contains("session-a"))
    and (.comments[0].body | contains("src/fixture"))' "$MOCK/issue.json" >/dev/null
assert_option opt-busy
reject claim b session-b
reject transition a release 42 --agent wrong-session --reason abandoned
assert_option opt-busy
printf 'ok: visible claim and owner checks\n'

setup_case
mkdir "$case_dir/race"
export TEST_RACE_DIR="$case_dir/race"
(export TEST_CLIENT=a; claim a race-a) > "$case_dir/a.out" 2>&1 & pid_a=$!
(export TEST_CLIENT=b; claim b race-b) > "$case_dir/b.out" 2>&1 & pid_b=$!
result_a=0; wait "$pid_a" || result_a=$?
result_b=0; wait "$pid_b" || result_b=$?
unset TEST_RACE_DIR
[[ $(( (result_a == 0) + (result_b == 0) )) == 1 ]]
[[ -n $(ref_sha) ]]
[[ $(jq '.comments | length' "$MOCK/issue.json") == 1 ]]
assert_option opt-busy
printf 'ok: simultaneous remote claims have exactly one winner\n'

for refusal in closed busy blocked linked branch-pr board-busy; do
    setup_case
    case "$refusal" in
        closed) edit_fixture '.state = "CLOSED"' ;;
        busy) edit_fixture '.labels = [{name:"agent/in-progress"}]' ;;
        blocked) edit_fixture '.labels = [{name:"blocked"}]' ;;
        linked) edit_fixture '.closedByPullRequestsReferences = [{number:99}]' ;;
        branch-pr) printf '[{"number":99,"url":"fixture-pr","headRefName":"issue-42/a"}]' > "$MOCK/prs-open.json" ;;
        board-busy) printf '{"option":"opt-busy"}' > "$MOCK/board.json" ;;
    esac
    reject claim a refused
    [[ -z $(ref_sha) ]]
done
printf 'ok: closed, busy, blocked and existing PRs are refused\n'

for failure in fail-project missing-option; do
    setup_case
    if [[ $failure == fail-project ]]; then touch "$MOCK/fail-project";
    else printf '{"fields":[]}' > "$MOCK/fields.json"; fi
    reject claim a refused
    [[ -z $(ref_sha) ]]
    [[ $(jq '.labels | length' "$MOCK/issue.json") == 0 ]]
done
printf 'ok: missing board access/options prevent claim and dispatch\n'

for failure in fail-label fail-edit fail-readback; do
    setup_case
    touch "$MOCK/$failure"
    reject claim a partial
    reserved=$(ref_sha)
    [[ -n $reserved ]]
    reject claim b duplicate
    if [[ $failure == fail-label ]]; then
        (cd "$case_dir/a" && CONCH_AGENT_LOG_DIR="$MOCK/queue" bash ops/conch-agent.sh queue) > "$MOCK/queue-out"
        sed -n '/In progress \/ has open PR/,$p' "$MOCK/queue-out" | grep -q '#42'
    fi
    rm "$MOCK/$failure"
    transition a resume 42 --agent partial > "$MOCK/resume"
    [[ $(ref_sha) == "$reserved" ]]
    assert_option opt-busy
done
printf 'ok: partial claim retains reservation and can resume\n'

setup_case
claim a lifecycle > "$MOCK/claim"
printf '[{"number":99,"url":"https://github.com/njdaniel/conch/pull/99","headRefName":"issue-42/a"}]' > "$MOCK/prs-open.json"
reject transition a release 42 --agent lifecycle --reason abandoned
transition a review 42 --agent lifecycle > "$MOCK/review"
assert_option opt-review
[[ -n $(ref_sha) ]]
reject transition a complete 42 --agent lifecycle
mv "$MOCK/prs-open.json" "$MOCK/prs-merged.json"
transition a complete 42 --agent lifecycle > "$MOCK/complete"
assert_option opt-test
[[ -z $(ref_sha) ]]
jq -e 'all(.labels[]; .name != "agent/in-progress")' "$MOCK/issue.json" >/dev/null
printf 'ok: PR handoff and merged completion retain acceptance gate\n'

setup_case
claim a first > "$MOCK/claim"
cp "$case_dir/a/.git/conch-claims/issue-42.json" "$MOCK/old-receipt"
transition a release 42 --agent first --reason 'worker stopped; return to backlog' > "$MOCK/release"
assert_option opt-backlog
[[ -z $(ref_sha) ]]
claim b second > "$MOCK/reclaim"
new_sha=$(ref_sha)
cp "$MOCK/old-receipt" "$case_dir/a/.git/conch-claims/issue-42.json"
reject transition a release 42 --agent first --reason stale
[[ $(ref_sha) == "$new_sha" ]]
assert_option opt-busy
printf 'ok: release/reclaim and stale receipt cannot release newer owner\n'

run_runner() {
    (cd "$case_dir/a" && CONCH_REPO="$case_dir/a" CONCH_AGENT_LOG_DIR="$MOCK/runner" \
        CLAUDE_BIN="$tmp/bin/claude" bash ops/conch-agent.sh run)
}
for outcome in failed-claim no-pr pr; do
    setup_case
    edit_fixture '.labels = [{name:"agent/todo"}]'
    case "$outcome" in
        failed-claim) touch "$MOCK/fail-project" ;;
        pr) touch "$MOCK/model-pr" ;;
    esac
    if [[ $outcome == pr ]]; then run_runner > "$MOCK/runner-out" 2>&1;
    else reject run_runner; fi
    if [[ $outcome == failed-claim ]]; then
        [[ ! -f "$MOCK/model-started" ]]
        [[ -z $(ref_sha) ]]
    else
        [[ -f "$MOCK/model-started" ]]
        [[ -n $(ref_sha) ]]
        if [[ $outcome == pr ]]; then assert_option opt-review;
        else
            assert_option opt-busy
            [[ -d "$MOCK/runner/worktrees/issue-42" ]]
            run_runner > "$MOCK/idle-retry" 2>&1
            [[ $(wc -l < "$MOCK/model-started") == 1 ]]
        fi
    fi
done
printf 'ok: runner launches only after visible claim; retains failed work; publishes Review\n'
printf 'PASS: issue claim lifecycle\n'
