---
name: principal-review
description: The recurring ownership loop — review merged PRs since the last review, check invariant drift, groom the backlog, and write a report for Nick. Run weekly or per-milestone.
---

# principal-review

This loop is what "Fable owns the project" means operationally (ADR-000 §6). Output is a dated report in `docs/reports/`.

## Procedure

1. **Establish the window.** Find the last report in `docs/reports/`; review everything merged since (`gh pr list --state merged`), plus open PRs and CI status on main.
2. **Review merged work.** For each merged PR: does it match its issue's acceptance criteria? Any scope creep? Approval-path PRs — was the E2E test present and was it merged by Nick?
3. **Check invariant drift** against the golden rules:
   - Single-binary: `deps-allowlist.txt` vs `go.mod`; any new required external process?
   - Schema-first: grep for hand-rolled JSON shapes outside `pkg/schema`.
   - Parity: any CLI/TUI or MCP capability without a REST/WS equivalent?
   - Audit chain: do approval-path tests still cover request → notify → resolve → audit?
4. **Groom the backlog.** Close stale issues, write the next tranche of issues to the template standard (Context/Files/Acceptance/Tests/Blocked-by), promote P2/P3 stubs to full issues only when their milestone is next, re-order blocked-by chains if reality changed.
5. **Write the report** to `docs/reports/YYYY-MM-DD-principal-review.md`:
   - Merged work summary (one line per PR).
   - Invariant status (per rule: OK or drift found + issue filed).
   - Backlog delta (issues written/closed/re-scoped).
   - **Decisions needed from Nick** — proposed ADRs, dependency requests, release readiness. Explicit list, even if empty.
6. Post-P1: also post the report into Conch itself (the build org is tenant #2) and raise release approvals as Conch approval objects.

## Cadence

Weekly, or at each milestone boundary — whichever comes first. If two loops in a row have nothing to review, say so in the report rather than skipping it.
