# Principal review — P1 close-out

**Date:** 2026-07-15 · **Author:** Fable (Principal Engineer) · **Decision owner:** Nick
**Window:** since the last report (`p0-spike-report.md`, 2026-07-13) — the entire P1 Core Loop phase.
**Headline:** P1 milestone is closed (0 open, 13 closed). `dogfood-check` (the ROADMAP success criterion) passes end-to-end against real binaries, including the ntfy-degraded path. `make check` is green on `main`. CI green on the last three runs.

## Merged work (28 PRs since the last report)

Schema/design:
- #42 Freeze P1 message envelope + versioned payload registry (#9)
- #47 Approval object + resolution event schemas
- #48 v1 message API shapes — typed payload on post, MessageV1 responses
- #50 MCP SDK selection + endpoint design docs (design half of #11)

Server core:
- #49 Server adopts MessageV1 — typed payloads over REST v1 + WS, audit on post
- #51 Approval store + state machine (request/resolve/escalate/expire) — `approval-path`
- #56 Webhook ingest endpoint (#15)
- #61 MCP endpoint — `/mcp` streamable HTTP, bearer auth, `post_message`/`read_channel` (#11)
- #62 ntfy HTTP notifier, urgent escalation topic, graceful degradation (#14)
- #63 MCP tools — `request_approval`/`await_decision`/`check_decision` (#13) — `approval-path`

Client:
- #58 TUI channel/message view, Bubble Tea skeleton (#17)
- #59 TUI approvals inbox + decision flow (#18) — `approval-path`
- #60 CLI `approvals list`/`approve`/`reject` (#16) — `approval-path`

Release gate:
- #64 dogfood-check E2E script — the P1 success criterion, automated (#19) — `approval-path`

Process/ops (not P1 feature work):
- #44 Agent dispatch design doc + dispatch skill
- #53, #55 conch-agent two-tag work-state model (`agent/todo`/`agent/ready`), `sync`/`queue` subcommands

Every P1 issue (#9, #11–#19) is closed; #9's schema freeze, #10 (implicit, folded into #47/#48), #12 (approval store) also closed. Milestone: **P1 Core Loop — 0 open, 13 closed.**

## Notable dispatch-loop findings

Several of these PRs (#59, #60, #61, #62) were produced by automation outside my own worktree-isolated dispatch loop — Google's Jules agent and separate codex cloud tasks Nick ran in parallel. None of them went through the five-gate review before merge. Post-merge, I found and fixed real defects in three of the four:

- **#59** (TUI inbox): CI was broken (a feature commit dropped `version: "2"` from `.golangci.yml`, masking 5 real staticcheck findings). Worse, a later commit had silently reverted three correct Copilot-autofix bounds checks, reopening a genuine panic: a slow `ListApprovals` refresh landing empty while the user sat in decision mode would index an empty slice. Fixed, tested, verified live.
- **#60** (CLI approve/reject): went through three rounds of the branch being overwritten mid-fix by concurrent automation (once reverting as far back as before #14's ntfy work was merged), including one push that didn't even compile. Final state: fixed a real UX bug (terminal-state/not-found errors printed twice — reproduced live), added missing reject/`--option` test coverage.
- **#63** (MCP approval tools, my own codex dispatch): caught one real defect — an over-strict validation loop rejected the schema's legitimate `custom` option kind, which would have been an MCP-only API-parity regression (an agent could never raise a custom-option approval a human could raise via REST/CLI).
- **#61** (MCP endpoint): verified post-merge only; all 6 of Copilot's inline findings turned out to already be fixed by the branch's own follow-up commit before merge. No action needed.

**Take-away for Nick:** the parallel-automation pattern (Jules + codex cloud tasks running unsupervised against the same issues my loop was also working) produced usable code but skipped the review gate entirely, and repeatedly clobbered in-flight fixes on shared branches. If this continues, it needs either its own review gate before merge, or a coordination signal (e.g., a label) so two automated pipelines don't fight over the same branch.

## Invariant status

| Rule | Status |
|---|---|
| 1. Single-binary | **OK.** `go.mod` direct requires (`coder/websocket`, `modelcontextprotocol/go-sdk`, `modernc.org/sqlite`, `bubbletea`, `lipgloss`) match `deps-allowlist.txt` exactly, each with a dated sign-off comment. ntfy remains optional (proven live by `dogfood-check`'s degraded path: approval still resolves, `notify_failed` audited, nothing blocks). |
| 2. Schema-first | **OK.** Reviewed every wire-facing struct added this window. The only non-`pkg/schema` JSON-tagged types are the `mcpXInput`/`mcpXOutput` projection structs in `internal/server/mcp.go`, which exist for a documented, narrow reason (the MCP SDK infers `json.RawMessage`/`schema.Timestamp` as objects instead of their JSON encoding) and wrap canonical `pkg/schema` values rather than duplicating them. |
| 3. Approval-path E2E test | **OK, and now automated.** `TestFullChainRequestNotifyResolveAudit` (approvals package) and `TestMCPApprovalFullChainAwaitAndCheck` (mcp_test.go) cover it at the unit level; `e2e/dogfood` now covers it live against real binaries, both happy and degraded paths, in CI on every push to `main` and every `approval-path`-labeled PR. |
| 4. API parity | **Drift found, issue filed (#65).** MCP's `check_decision`/`await_decision` (#13) can read any single approval by id, including terminal ones, directly from the store. REST has no equivalent — `GET /v1/approvals` (#12) only lists the open set. An MCP agent can inspect a specific resolved approval; a human via REST/CLI cannot. Doesn't block the P1 criterion (dogfood-check only needs the open-list + resolve flow) but is a real capability gap. Filed as #65 (P2 Hardening). |

**Process note, not an invariant:** rule 3 says approval-path PRs are "merged by Nick personally." Under Nick's explicit, session-scoped authorization for this P1-completion loop ("codex everything, auto-merge," given this session), I merged #63 and #64 myself after the full five-gate review, rather than Nick clicking merge. This was a deliberate, bounded override for this session, not a standing change to the rule — flagging it here for the record rather than letting it pass silently. #59/#60's `approval-path` labels were also missing at merge time (the originating automation didn't set them); added retroactively for accurate history.

## Backlog delta

- Closed: #9, #11–#19 (all of P1).
- Filed: #65 (`GET /v1/approvals/{id}`, P2 Hardening, template-standard, `Blocked by: nothing`).
- Re-scoped: #57 (`GET /v1/channels` list endpoint + TUI adoption) moved from no milestone to **P2 Hardening** — it's an API-parity nice-to-have the TUI already works around via `CONCH_CHANNELS`, not a P1 blocker.
- P2 Hardening backlog is otherwise unchanged: 7 stubs (#20–#26), all currently `agent/ready` (unblocked — #19 closed) but correctly **not** `agent/todo`, so the unattended runner won't touch them without Nick's explicit approval.
- P3 Icebox: 1 stub (#27), unchanged.

## Decisions needed from Nick

1. **P1 milestone close** — the milestone is empty (0 open); ready to close in GitHub if you want that formalized.
2. **Parallel-automation coordination** — per the dispatch-loop findings above: do you want Jules/codex-cloud tasks kept as-is, gated some other way, or folded into this session's dispatch loop so they get the five-gate review before merge?
3. **P2 scoping** — 7 stubs + #57 + #65 are unblocked and awaiting your `agent/todo` approval whenever you want P2 started; no action needed until then.
4. **Release readiness** — P1's own success criterion is proven (dogfood-check green, CI green, invariants holding except the flagged parity gap). Whether "P1 complete" warrants a tagged pre-1.0 release (per the session-1 SemVer decision: `v0.x.y`, tags only at Nick-gated releases) is your call.