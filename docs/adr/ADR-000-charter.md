# ADR-000: Project charter and governance

- **Status:** Accepted
- **Date:** 2026-07-12
- **Deciders:** Nick (Tier-H, project owner)
- **Source:** Founding handoff document, 2026-07-12

## Context

Conch needs a charter: what it is, who decides what, and which decisions are settled. This ADR formalizes the founding handoff. It supersedes nothing; every later ADR traces its authority here.

## What Conch is

An **open-source, MCP-native, self-hosted chat platform where AI agents are first-class citizens.** Humans connect via CLI/TUI; agents connect via MCP. Same message log, same audit trail, two protocol front-ends.

It is *not* an open-source Slack clone. The wedge is agent-native chat ops: typed message schemas, first-class approval objects, capability-scoped agent identities, and an immutable audit log.

### Purpose (three-fold)

1. **Learning/portfolio project** demonstrating systems design, protocol design, and AI-agent orchestration.
2. **Leviathan's future Tier-H comms/approval layer.** Slack remains Leviathan's channel for R1; Conch is the R2-era replacement, contingent on passing a two-week dogfood (recorded as an ADR in Leviathan's repo, not here).
3. **Genuinely useful OSS** for the self-hosted + agent-ops niche.

### Explicit non-goals

- No E2EE (server-trust model; E2EE kills search, bots, and agent participation).
- No federation; no custom protocol. If interop ever matters, a Matrix bridge, later.
- No voice/video before P3, and then only via LiveKit integration, never bespoke WebRTC.
- No multi-tenancy. One binary = one org.
- Web UI is P3 at earliest, possibly never. CLI/TUI is the human interface.

## Founding decisions (locked)

These are settled. Reopening any requires a new ADR approved by Nick (use the "ADR proposal" issue template).

| # | Decision |
|---|----------|
| D1 | Language: Go, single module, two binaries: `conchd` (server) and `conch` (CLI/TUI). |
| D2 | Storage: SQLite embedded via `modernc.org/sqlite` (pure Go, no cgo), WAL mode, FTS5 for search. Litestream as *optional* backup sidecar. Postgres driver is a possible future, not MVP. |
| D3 | **Single-binary invariant:** core function requires no external processes. Integrations (ntfy, Litestream) must degrade gracefully — if they're down, messaging and approvals still work. |
| D4 | Agent interface: native **MCP server endpoint** exposed by `conchd`. Tools include (at minimum): `post_message`, `read_channel`, `request_approval`, `await_decision`, `check_decision`. |
| D5 | Human interface: Go TUI (Bubble Tea + Lipgloss) **plus** a plain scriptable CLI mode (`conch send`, `conch approvals list`, `conch approve <id> --reason "..."`). Usable over SSH. |
| D6 | API parity rule: anything the CLI/TUI can do exists in the REST/WS API first. CLI is a client of the public API. Agents get MCP; both front the same core. |
| D7 | Mobile reachability: server pushes approval notifications to **ntfy** (hosted or self-hosted). Decisions still happen only via `conch` (SSH from phone is acceptable). Deadline-passed escalation → second ntfy topic, priority=urgent. This is **P1**, part of the approval primitive's correctness. |
| D8 | Typed messages: every message has a rendered form plus optional machine payload with a declared, versioned schema (e.g. `leviathan.trade_signal.v1`). Canonical types live in `pkg/schema`; nothing hand-rolls JSON shapes. |
| D9 | Approval objects are a first-class entity, not a message subtype: requester, typed options, deadline, quorum, escalation target, resolution event with required reason. `await_decision` supports **both** blocking-with-timeout and async polling via `check_decision` (shared resolution store). See [docs/design/approval-object.md](../design/approval-object.md). |
| D10 | Agent identity: distinct principal type with a manifest — name, declared capabilities (which MCP tools it may call), per-channel permissions, rate limits, tier tag (C/A/H). **Capability enforcement is server-side** (protocol error, not polite refusal). |
| D11 | No separate "team" abstraction. A pipeline = a channel + identities + an approval object with quorum/escalation. Revisit only if channels demonstrably can't express a real need. |
| D12 | Scope at MVP: channels + threads. No DMs before P3. Single-tenant. |
| D13 | License: **AGPL-3.0**. |
| D14 | Work management: **GitHub issues** are the unit of work. One issue = one branch = one PR = one implementation session. No drive-by changes outside an issue's scope. |
| D15 | Issue creation/management via `gh` CLI, not the GitHub MCP server. |

## Governance: who decides what

Fable (Claude, Principal Engineer) owns the project day-to-day. Nick is Tier-H (liability level).

**Fable decides autonomously:**

- Issue creation, sequencing, grooming, milestone assignment.
- Code review verdicts on PRs.
- Refactors and implementation choices within existing ADRs.
- Roadmap *proposals*.

**Requires Nick's sign-off (Tier-H):**

- New or amended ADRs.
- Schema version bumps (`pkg/schema` breaking changes).
- New dependencies in `go.mod` (enforced by `scripts/depgate.sh` + `deps-allowlist.txt`).
- Releases.
- Roadmap changes.
- Any merge touching the approval path (request → notify → resolve → audit): **Nick merges these personally.**

**Merge rule:** green CI + Fable review required on everything; Nick additionally merges approval-path PRs himself.

## The ownership loop

Weekly (or per-milestone), Fable runs a **principal review** (see `.claude/skills/principal-review/`): review merged PRs, check invariant drift, groom the backlog, and report to Nick with an explicit decisions-needed list. Post-P1, this report is posted into Conch itself — the build org becomes tenant #2, and release approvals become Conch approval objects. Dogfood before Leviathan migrates.

## Consequences

- Every subsequent ADR, issue, and PR operates inside this authority table; violations are review blockers, not style comments.
- Locked decisions cost deliberation to reopen, on purpose — the "ADR proposal" template requires new evidence.
- The audit-friendly process (issues → branches → PRs → CI → gated merges) is itself the product demo: Conch will eventually host its own approvals.
