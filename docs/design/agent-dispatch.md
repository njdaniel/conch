# Agent dispatch

Status: adopted · Owner: Fable (Principal Engineer) · Operational procedure: `.claude/skills/dispatch/`

How issue work gets assigned to workers, briefed, isolated, and reviewed. P0
was delivered by a mix of workers under ad-hoc dispatch; it worked because the
dispatcher improvised the same safeguards every time. This document makes the
safeguards procedure instead of memory. The session evidence behind each rule
is in [the P0 spike report](../reports/p0-spike-report.md) ("Pain points").

## Roles

- **Dispatcher** — the principal session (Fable). Picks the worker, writes the
  brief, owns the review gate, commits, pushes, and opens the PR. The review
  gate is never delegated.
- **Worker** — executes exactly one issue against a written brief. Code
  workers do not commit, push, or use `gh`. (Exception: a Claude worker whose
  task *is* GitHub mechanics, e.g. the issue #8 probe PRs.)
- **Reviewer of record** — the dispatcher, at principal-engineer standard.
  Nick reviews and merges per ADR-000 — always personally for approval-path
  PRs and schema version bumps.

## Worker registry

| Worker | Runtime | Best at | Hard limits | Cost |
|---|---|---|---|---|
| principal session | this session | judgment calls, verdicts, approval-path, cross-cutting work, all review | attention is the scarce resource | high |
| protocol-designer | Claude agent, **opus** | wire shapes, MCP tool definitions, schema freezes (#9) | output requires Nick's D8 sign-off | high |
| **codex** | `codex exec --full-auto` | mechanical implementation against a crisp spec (#5, #6) | no network, **no loopback sockets** — cannot run socket tests, `gh`, or e2e, so its "tests pass" excludes them; full-auto requires the user's explicit authorization each session; wire-level judgment errors observed in both P0 dispatches | medium |
| server-engineer / cli-engineer | Claude agent, sonnet | scoped implementation inside their directory | narrower context than the principal | medium |
| general Claude worker | Claude agent, opus or sonnet | GitHub mechanics, probes, multi-step ops that need `gh` (#8) | — | medium |
| explorer / Explore | Claude agent, read-only | triage, locating code | read-only | low |
| docs-agent | Claude agent, sonnet | README, changelog, ADR drafts | ADRs still need Nick | low |

## Decision table

| The issue is… | Dispatch to |
|---|---|
| a wire shape, schema version, or MCP tool definition | protocol-designer (opus) |
| implementation with crisp acceptance criteria and no open design questions | codex |
| implementation with design ambiguity | server-/cli-engineer; principal if cross-cutting |
| GitHub or process mechanics | general Claude worker |
| approval-path code, a kill-criterion verdict, a release call | principal session — never delegated |
| documentation only | docs-agent |

Parallel dispatch is allowed only when file areas are disjoint (e.g. #6 in
`internal/cli` alongside #8 in `pkg/schema/testdata` probes). One worker per
file area at a time.

## The brief

Every dispatch gets a written brief (scratchpad file, not committed) with
exactly these sections — the template that worked for #5, #6, and #9:

1. **Task** — issue number and one-line goal; "read CLAUDE.md first".
2. **Scope** — the files/packages the worker may touch.
3. **Explicitly out of scope** — always including "no new go.mod dependencies
   (depgate fails the build)" and, for code workers, "do not commit, push, or
   use gh".
4. **Required tests** — from the issue, adjusted for the worker's sandbox:
   tell codex which tests it *cannot* run so it reports the gap instead of
   faking it.
5. **Verify before finishing** — `make check` (with the golangci-lint PATH
   note for local runs).
6. **Conventions** — concrete existing files to imitate.

## Isolation

- Claude agents run in git worktrees (`isolation: worktree`), always.
- Codex runs in the main working tree on a branch the dispatcher creates
  *before* launching it (`issue-N/codex`). The dispatcher does not touch the
  tree while codex runs.
- Branch naming `issue-N/<worker>` keeps authorship visible in history.

## The review gate

Mandatory, dispatcher-owned. No worker output reaches a commit without all of:

1. **Full local test run.** Worker sandboxes lie by omission: codex cannot
   open sockets, so every socket-dependent test arrives unverified even when
   its summary says tests pass. `make check` plus `go test -race ./...`
   locally, every time.
2. **Line-by-line diff review.** Wire-level judgment is where workers fail —
   P0 review caught local-timezone timestamps and a wrong status code (#5) and
   an unclean interrupt exit (#6). Read everything.
3. **End-to-end verification against real binaries.** Drive the flow the issue
   ships (curl, conch, a WS client) — not only the test suite.
4. **Bot-commit check before any push.** Fetch the PR branch and diff any
   commits that appeared since your last fetch: Copilot autofix/swe-agent push
   unreviewed commits (P0 saw four; two defective, one a genuinely correct
   RFC 6455 fix — so *read* them, don't just revert them). Push only with
   `--force-with-lease` pinned to the head you inspected.
5. **Attribution.** Commit trailer `Co-Authored-By: Claude Fable 5
   <noreply@anthropic.com>`; the PR body carries an Attribution section naming
   the worker, its version, what was unverified at handoff, and what the
   reviewer amended.

## Unattended runner

`ops/conch-agent.sh` (systemd timer, 3×/day) is a separate, headless path that
dispatches issues without a dispatcher session. It is **opt-in**: it only
considers open issues a human has labeled `agent/ready`. The label survives
failed runs (auto-retry on the next firing); a session that finds an issue
not executable removes `agent/ready` along with adding `blocked`, so a
rewritten issue needs fresh approval before the runner touches it again.

## Authorization boundaries

- **Codex full-auto** disables its approval gate; the user must name that mode
  before the first codex dispatch of a session (the harness enforces this).
- **Merges are Nick's.** The dispatcher never merges — and never assumes a
  merge happened: verify PR state with `gh` (two P0 PRs reported as merged
  were still open).
- New dependencies a worker needs → stop, get Nick's sign-off and a
  `deps-allowlist.txt` entry first (ADR-000).

## Known hazards, with receipts

| Hazard | P0 receipt | Countered by |
|---|---|---|
| sandbox gaps → unverified tests | #5, #6 (no loopback) | gate 1 |
| wire-judgment defects | #5 UTC/413, #6 interrupt exit | gate 2 |
| unreviewed bot commits on PR branches | #32 signal leak, #36 build break, #37 correct fix | gate 4 |
| stale-lease push failure | #32, #37 | someone moved the branch — fetch and inspect, never retry blind |
