# Agent entry point

Read [CLAUDE.md](CLAUDE.md) before changing Conch; its build, scope, review and
approval rules apply to every agent. Follow relevant nested instructions too.

Before implementing an issue, check its live comments, labels, linked PRs and
Project status, and check active file reservations. The dispatcher must obtain
a successful shared claim with `bash ops/issue-claim.sh claim ISSUE --agent
SESSION --branch BRANCH --files PATHS`. Use a unique session name. If already
claimed, stop and request a handoff or choose another issue through the dispatcher.
Never clear another session's label or reservation, or launch after a failed
claim/board update.

A network-restricted worker receives the existing claim receipt and reserved
scope in its brief; it does not claim again or use GitHub. The dispatcher owns
claim transitions, review and PR creation under the existing dispatch rules.
The unattended runner already claims for its worker.

At PR handoff the dispatcher runs `review`; after merge, `complete` moves the
item to Ready to test. Explicit abandonment uses `release --reason` only after
the previous worker has stopped. These commands never grant implementation
approval, merge, release software, or mark a deployed acceptance check Done.
See [issue claims](docs/design/agent-dispatch.md#issue-claims).
