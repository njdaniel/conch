#!/usr/bin/env bash
# Shared, cooperative issue claims. Git's compare-and-swap on a dedicated
# remote ref arbitrates across clones; labels and comments provide visibility.
# No worker may start until claim/resume has returned success.
set -euo pipefail
umask 077

usage() {
    cat <<'USAGE'
Usage:
  bash ops/issue-claim.sh claim ISSUE --agent SESSION --branch BRANCH --files PATHS
  bash ops/issue-claim.sh resume ISSUE --agent SESSION
  bash ops/issue-claim.sh review ISSUE --agent SESSION
  bash ops/issue-claim.sh complete ISSUE --agent SESSION
  bash ops/issue-claim.sh release ISSUE --agent SESSION --reason REASON
  bash ops/issue-claim.sh status ISSUE

Run from the Conch checkout. SESSION must uniquely identify this dispatch.
PATHS is a human-readable list of reserved files/areas, never secrets.
review requires an open PR on the recorded branch; complete requires a merged
PR and sets Ready to test (never Done). release refuses linked/open PRs.
Failed visibility updates retain the claim: repair access and use resume.
No claim expires automatically. A dispatcher claims for restricted workers.
CONCH_PROJECT_OWNER/CONCH_PROJECT_NUMBER default to njdaniel/14.
USAGE
}
die() { printf 'issue-claim: %s\n' "$*" >&2; exit 1; }
if [[ ${1:-} == --help || ${1:-} == -h ]]; then usage; exit 0; fi
[[ $# -ge 2 ]] || { usage >&2; exit 2; }
action=$1 issue=$2
shift 2
case "$action" in claim|resume|review|complete|release|status) ;; *) usage >&2; exit 2 ;; esac
[[ $issue =~ ^[1-9][0-9]*$ ]] || die 'ISSUE must be a positive integer'
agent='' branch='' files='' reason=''
while [[ $# -gt 0 ]]; do
    [[ $# -ge 2 ]] || die "missing value for $1"
    case "$1" in
        --agent) agent=$2 ;; --branch) branch=$2 ;;
        --files) files=$2 ;; --reason) reason=$2 ;;
        *) die "unknown argument: $1" ;;
    esac
    shift 2
done
[[ $action == status || -n $agent ]] || die '--agent SESSION is required'
if [[ $action == claim ]]; then
    [[ -n $branch && -n $files ]] || die 'claim requires --branch and --files'
    git check-ref-format --branch "$branch" >/dev/null
fi
[[ $action != release || -n $reason ]] || die 'release requires --reason'

root=$(git rev-parse --show-toplevel)
cd "$root"
common=$(git rev-parse --git-common-dir)
mkdir -p "$common/conch-claims"
state_dir=$(cd "$common/conch-claims" && pwd)
state_file="$state_dir/issue-$issue.json"
# Serializes lifecycle operations in this clone, including linked worktrees.
# The remote lease below, not this local lock, coordinates separate machines.
exec 8>"$state_dir/issue-$issue.lock"
flock -n 8 || die "another local transition is handling #$issue"
ref="refs/heads/agent-claims/issue-$issue"
repo=$(gh repo view "$(git remote get-url origin)" --json nameWithOwner --jq .nameWithOwner)
[[ $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die 'cannot identify origin repository'
project_owner=${CONCH_PROJECT_OWNER:-njdaniel}
project_number=${CONCH_PROJECT_NUMBER:-14}
[[ $project_number =~ ^[1-9][0-9]*$ ]] || die 'invalid Project number'

remote_sha() { git ls-remote --refs origin "$ref" | awk '{print $1}'; }
read_issue() {
    issue_json=$(gh issue view "$issue" --repo "$repo" \
        --json id,number,state,url,labels,body,comments,closedByPullRequestsReferences)
    jq -e --argjson n "$issue" '.number == $n' <<<"$issue_json" >/dev/null
}
free_issue() {
    [[ $(jq -r .state <<<"$issue_json") == OPEN ]] || die 'issue is closed'
    jq -e '[.labels[].name | select(. == "agent/in-progress" or . == "blocked")] | length == 0' \
        <<<"$issue_json" >/dev/null || die 'issue is busy or blocked; inspect its comments'
    jq -e '.closedByPullRequestsReferences | length == 0' <<<"$issue_json" >/dev/null \
        || die 'issue already has a linked PR'
    [[ $(gh pr list --repo "$repo" --head "$branch" --state open --json number | jq length) == 0 ]] \
        || die 'the requested work branch already has an open PR'
}
# Discover IDs on every transition. Never invent fields/options or request
# broader permissions automatically. A missing project scope stops dispatch.
board_prepare() {
    local fields matches
    project_id=$(gh project view "$project_number" --owner "$project_owner" --format json | jq -er .id)
    fields=$(gh project field-list "$project_number" --owner "$project_owner" --limit 100 --format json)
    matches=$(jq --arg status "$1" '[.fields[] | select(.name == "Status")
        | . as $f | .options[]? | select((.name | ascii_downcase) == ($status | ascii_downcase))
        | {field: $f.id, option: .id}]' <<<"$fields")
    [[ $(jq length <<<"$matches") == 1 ]] || die "Project needs one Status option named '$1'"
    field_id=$(jq -er '.[0].field' <<<"$matches")
    option_id=$(jq -er '.[0].option' <<<"$matches")
}
board_available() {
    local items status
    items=$(gh api graphql -f query='query($issue:ID!) { node(id:$issue) {
      ... on Issue { projectItems(first:100) { nodes { project { id }
        fieldValueByName(name:"Status") { ... on ProjectV2ItemFieldSingleSelectValue { name } }
      } pageInfo { hasNextPage } } }
    } }' -f issue="$(jq -er .id <<<"$issue_json")" --jq '.data.node.projectItems')
    [[ $(jq -r .pageInfo.hasNextPage <<<"$items") == false ]] \
        || die 'issue has more than 100 projects; inspect status manually'
    status=$(jq -r --arg id "$project_id" '[.nodes[] | select(.project.id == $id)]
        | if length > 1 then error("duplicate Project item")
          else (.[0].fieldValueByName.name // "" | ascii_downcase) end' <<<"$items")
    case "$status" in ''|backlog|ready) ;; *) die "Project status is '$status'; inspect the existing work" ;; esac
}
board_set() {
    local item_id actual
    item_id=$(gh project item-add "$project_number" --owner "$project_owner" \
        --url "$(jq -r .url <<<"$issue_json")" --format json | jq -er .id)
    gh project item-edit --project-id "$project_id" --id "$item_id" \
        --field-id "$field_id" --single-select-option-id "$option_id" >/dev/null
    actual=$(gh api graphql -f query='query($item:ID!) { node(id:$item) {
      ... on ProjectV2Item { fieldValueByName(name:"Status") {
        ... on ProjectV2ItemFieldSingleSelectValue { optionId }
      } }
    } }' -f item="$item_id" --jq '.data.node.fieldValueByName.optionId')
    [[ $actual == "$option_id" ]] || die 'Project status readback did not match; claim retained'
}
post() { gh issue comment "$issue" --repo "$repo" --body-file - <<<"$1" >/dev/null; }
owned() {
    [[ -f $state_file ]] || die 'no local claim receipt; inspect status and contact the dispatcher'
    jq -e --arg agent "$agent" --arg repo "$repo" \
        '.agent == $agent and .repo == $repo' "$state_file" >/dev/null || die 'claim belongs to another session'
    sha=$(jq -er .sha "$state_file")
    [[ $(remote_sha) == "$sha" ]] || die 'remote claim changed; do not change labels or board'
    branch=$(jq -er .branch "$state_file")
    files=$(jq -er .files "$state_file")
}
publish_claim() {
    gh label create agent/in-progress --repo "$repo" \
        --description 'An agent has claimed this issue' --color BFDADC >/dev/null 2>&1 || true
    gh issue edit "$issue" --repo "$repo" --add-label agent/in-progress >/dev/null
    post "<!-- conch-claim:$sha:active -->
Claimed by **$agent**.

- Branch: \`$branch\`
- Started (UTC): $(jq -r .started "$state_file")
- Reserved files/areas: $files
- Claim receipt: \`$sha\` on \`agent-claims/issue-$issue\`

Do not start another implementation for this issue. Check the linked PR and contact the dispatcher to hand over or release it."
    board_set
    # Re-read the issue too; a success return is the launch gate.
    read_issue
    jq -e 'any(.labels[]; .name == "agent/in-progress") and .state == "OPEN"
        and (.closedByPullRequestsReferences | length == 0)' <<<"$issue_json" >/dev/null \
        || die 'issue changed while claiming; inspect it before resuming'
    printf 'Claimed #%s: %s; branch=%s; receipt=%s\n' "$issue" "$agent" "$branch" "$sha"
}
read_issue
if [[ $action == status ]]; then
    printf 'Remote claim: %s\n' "$(remote_sha)"
    [[ ! -f $state_file ]] || jq . "$state_file"
    gh issue view "$issue" --repo "$repo" --comments
    exit 0
fi
if [[ $action == claim ]]; then
    [[ ! -f $state_file ]] || die 'local receipt exists; use resume with its session or inspect status'
    free_issue
    [[ -z $(remote_sha) ]] || die 'another session holds the remote claim'
    board_prepare 'In progress'
    board_available
    base=$(git rev-parse 'refs/remotes/origin/main^{commit}')
    nonce=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
    started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    metadata=$(jq -n --arg repo "$repo" --argjson issue "$issue" --arg agent "$agent" \
        --arg branch "$branch" --arg files "$files" --arg started "$started" --arg nonce "$nonce" \
        '{repo:$repo,issue:$issue,agent:$agent,branch:$branch,files:$files,started:$started,nonce:$nonce}')
    sha=$(git -c user.name='Conch claim' -c user.email='conch-claim@users.noreply.github.com' \
        commit-tree "$(git rev-parse "$base^{tree}")" -p "$base" -m "$metadata")
    # Write the receipt before the remote operation: even an interrupted push
    # can be reconciled with resume. No working branch or source file changes.
    jq --arg sha "$sha" '. + {sha:$sha}' <<<"$metadata" > "$state_file"
    if ! git push --porcelain --no-follow-tags --force-with-lease="$ref:" origin "$sha:$ref"; then
        die "claim push failed or raced; receipt retained; inspect status (do not launch)"
    fi
    # Non-cooperating/manual activity may have appeared during the push.
    read_issue
    free_issue
    board_available
    publish_claim
    exit 0
fi
owned
case "$action" in
    resume)
        [[ $(jq -r .state <<<"$issue_json") == OPEN ]] || die 'issue is closed'
        jq -e '(.closedByPullRequestsReferences | length == 0)
            and all(.labels[]; .name != "blocked")' <<<"$issue_json" >/dev/null \
            || die 'issue is blocked or has a PR; inspect before resuming'
        board_prepare 'In progress'
        publish_claim
        exit 0
        ;;
    review|complete)
        pr_state=open target=Review
        [[ $action != complete ]] || { pr_state=merged; target='Ready to test'; }
        prs=$(gh pr list --repo "$repo" --head "$branch" --state "$pr_state" --json number,url)
        [[ $(jq length <<<"$prs") == 1 ]] || die "expected one $pr_state PR on $branch"
        pr_url=$(jq -er '.[0].url' <<<"$prs")
        board_prepare "$target"
        post "<!-- conch-claim:$sha:$action -->
$agent: $target — $pr_url. Claim receipt: \`$sha\`."
        board_set
        if [[ $action == review ]]; then printf 'Review #%s: %s\n' "$issue" "$pr_url"; exit 0; fi
        ;;
    release)
        [[ $(jq -r .state <<<"$issue_json") == OPEN ]] || die 'closed issue: use complete for a merged PR'
        jq -e '.closedByPullRequestsReferences | length == 0' <<<"$issue_json" >/dev/null \
            || die 'issue has a linked PR; do not release it as abandoned'
        [[ $(gh pr list --repo "$repo" --head "$branch" --state open --json number | jq length) == 0 ]] \
            || die 'branch has an open PR; use review'
        board_prepare Backlog
        post "<!-- conch-claim:$sha:released -->
$agent explicitly released this claim. Reason: $reason. Work branches and files are preserved."
        board_set
        ;;
esac
# Keep the remote reservation until all visibility changes succeed. The lease
# prevents an old receipt from deleting a newer owner's claim.
gh issue edit "$issue" --repo "$repo" --remove-label agent/in-progress >/dev/null
git push --porcelain --no-follow-tags --force-with-lease="$ref:$sha" origin ":$ref"
rm "$state_file"
printf '%s #%s; reservation removed\n' "$action" "$issue"
