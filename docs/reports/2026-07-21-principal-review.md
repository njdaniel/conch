# Principal review — P2 process refresh close-out

**Date:** 2026-07-21 · **Author:** Fable (Principal Engineer) · **Decision owner:** Nick
**Window:** since the last report (`2026-07-15-principal-review.md`, merged as #66) — 2 PRs.
**Headline:** Quiet, process-focused window. No feature code merged. `make check` and CI are green on `main`. The one substantive PR (#70) went through the full five-gate review, which caught and fixed a real gap in the new CI process check before merge completed.

## Merged work (2 PRs)

- **#68** — README Quickstart: real, verified run instructions (issue #67). Docs-only; every command in the new Quickstart was run and verified against real binaries per the issue's acceptance criteria. No scope creep (single file, as scoped).
- **#70** — P2 process refresh: external-automation dispatch gate, `process-checks.yml`/`release.yml`, `security-reviewer` agent, `release`/`issue-writer` skills, model-drift cleanup (issue #69). Reviewed against all five gates before merge:
  1. Full local run: `make check` and `go test -race ./...` green.
  2. Line-by-line diff review found one material defect: `process-checks.yml`'s approval-path file filter missed `internal/server/store/approvals.go` (the actual approval state machine), its test file, `mcp_test.go`, and the approval/decision/resolution testdata fixtures — a PR touching them could have merged without the `approval-path` label, the exact gap the check exists to close. Also found the issue-reference regex only accepted closing-keyword phrasing ("Closes #42"), rejecting valid bare references ("Issue #42", "Refs #42").
  3. `actionlint` clean on both new workflows; both fixes were dry-run against every real approval-path file in the repo (no false negatives) and a control set of unrelated files (no false positives) before landing.
  4. No unreviewed bot commits appeared on the branch between fetches.
  5. Fix committed as e131399 with standard attribution, pushed to the PR branch, merged by Nick.

  Not approval-path (no approval-path files touched — confirmed by the filter itself, post-fix). #69 closed on merge.

## Invariant status

| Rule | Status |
|---|---|
| 1. Single-binary | **OK.** `go.mod` direct requires (`coder/websocket`, `modelcontextprotocol/go-sdk`, `modernc.org/sqlite`, `bubbletea`, `lipgloss`) match `deps-allowlist.txt` exactly — no new dependencies this window. |
| 2. Schema-first | **OK.** No new wire-facing structs this window (docs/process only). The one known non-`pkg/schema` JSON exception (`internal/server/mcp.go`'s SDK projection structs, documented in the 2026-07-15 report) is unchanged. |
| 3. Approval-path E2E test | **OK, unchanged.** No approval-path files touched this window; `internal/server/approvals` and `pkg/schema` test suites still pass (`go test ./...`, confirmed this session). |
| 4. API parity | **No new drift.** The one open gap from the last report (#65, MCP can read a terminal approval by id, REST cannot) is unchanged and still tracked — no new capability shipped this window without its REST/WS equivalent. |

## Backlog delta

- Closed: #67, #69.
- Filed: none — the process-refresh work was fully scoped by #69, no follow-on issues needed.
- Groomed: ran `ops/conch-agent.sh sync` — #65 (`Blocked by: nothing`, unblocked since #19 closed) was missing the `agent/ready` label; sync corrected it (`+agent/ready #65`). No other drift found in the two-tag model.
- P2 Hardening: 9 open (8 stubs #20–#26 minus #66/#67 folded elsewhere, plus #57, #65), 1 closed (#67). All 9 are now `agent/ready`; **none are `agent/todo`** — the unattended runner remains correctly idle pending Nick's phase-start approval.
- P3 Icebox: unchanged, 1 stub (#27).

## Decisions needed from Nick

1. **P0 Spike / P1 Core Loop milestone close** — both still show 0 open issues but remain `open` in GitHub. Carried over from the last report; still just a formalization step whenever you want it.
2. **P2 Hardening start** — all 9 open P2 issues are unblocked and `agent/ready`. Nothing happens until you apply `agent/todo` to the ones you want worked (individually or as a batch).
3. **Release readiness** — assessed against the new `release` skill's four preconditions: (a) CI green on `main` — yes, last run success; (b) `go run ./e2e/dogfood` against the release commit — not run this session, verify fresh immediately before any tag; (c) this report shows no unresolved invariant drift — true; (d) your recorded sign-off — not yet given. No action needed until you want to cut `v0.x.y`.
4. **Parallel-automation coordination** (open item from the last report) — **resolved.** #69/PR #70 folded Jules/codex-cloud output into the dispatch loop's five-gate review, enforced by `process-checks.yml`'s `gate:reviewed` check. No longer an open decision.

## Note on step 6 (post report into Conch / raise release approvals)

This requires a running `conchd` instance and the tenant #2 build-org channel to post into and to raise Conch approval objects against. I haven't done this — flagging it rather than assuming a live server. Let me know if you want me to do that now (I'd need `conchd`'s address/token or to boot it locally) or whether that's something you handle on your end.
