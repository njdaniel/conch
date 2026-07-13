# P0 spike report

**Date:** 2026-07-13 · **Author:** Fable (Principal Engineer) · **Decision owner:** Nick
**Scope:** ROADMAP P0 — `conchd` skeleton, SQLite/WAL store, REST message API, WS hub, bare CLI, curl-driven fake agent, CI.
**Kill criterion under test:** "if realtime feels bad or the architecture fights back hard, stop and report."

## Verdict: PROCEED

Realtime does not feel bad — it is imperceptible on localhost — and the
architecture cooperated at every step. No workaround debt was taken on.
Details and the honest pain list follow.

## What was built (all merged)

| Piece | PR | Notes |
|---|---|---|
| conchd skeleton: config, serve, /healthz, graceful shutdown | #32 | |
| SQLite store (WAL, migrations, append-only audit) | #31 | landed before this milestone window |
| REST v0: post message, list with cursor pagination | #35 | golden fixtures landed here |
| Channel + principal provisioning over REST | #36 | gap found mid-P0, filed as #34 |
| WebSocket hub: per-channel subscribe/broadcast | #37 | bounded buffers, slow-consumer drop |
| Bare CLI: `conch send`, `conch tail` | #40 | speaks only the public API |
| schema-compat gate proven to bite | #8 (probes #38/#39) | fixture immutability enforced in CI + pre-commit |

## Realtime feel (the spike question)

`scripts/demo-p0.sh` measures post→tail latency end to end: curl process
start, HTTP POST, SQLite insert, hub fan-out, WS frame, `conch tail` line
to stdout. Over 100 messages on one machine: **avg 5ms, max 7ms** — and
most of that is curl fork/exec, not conchd. Interactive use of
`conch tail` alongside posting feels instant. Two subscribers on one
channel receive concurrently; cross-channel isolation holds; a laggard
subscriber is dropped (bounded buffer) without stalling anyone else.

## Pain points, honestly

1. **Wire-value discipline needed constant attention.** Three review
   catches of the same class: local-timezone timestamps on the wire,
   raw driver errors in `/healthz`, nanosecond-vs-millisecond precision
   drift between POST responses and reads. None hard to fix; all easy to
   reintroduce. **P1 consequence:** the #9 envelope must pin timestamp
   encoding (UTC, fixed precision) in the schema itself.
2. **Unreviewed bot commits on PR branches.** GitHub Copilot autofix and
   copilot-swe-agent pushed four commits to open PR branches this
   milestone; two were defective (one broke the build by using another
   SQLite driver's API; one leaked a signal registration) and one was a
   genuinely correct RFC 6455 fix. Process rule now in force: every bot
   commit gets diffed and verified before building on the branch.
3. **Small environment traps, all resolved:** GNU Make's built-in
   `LINT = lint` silently disabled the linter (`?=` never assigned);
   `http.Server.Shutdown` ignores hijacked WS connections, so the hub
   needs explicit close on shutdown; `signal.NotifyContext` keeps its
   handler until `stop()`, so second-signal force-kill needed
   `context.AfterFunc`.
4. **Issue-planning gap:** nothing in the original P0 issue set could
   create a channel or principal, discovered while verifying #5's
   "curl alone can drive it" criterion. Filed #34, fixed in #36 within
   the milestone. Cost: one extra PR.

None of these is architecture fighting back; they are normal build
friction, and each produced a durable fix or rule.

## Architecture assessment

- **Single binary held** (ADR-002): no external processes; the store,
  hub, REST, and WS all live in conchd. depgate gate kept the module at
  two approved deps (modernc.org/sqlite, coder/websocket).
- **Schema-first held** (D8): all wire shapes in `pkg/schema` with golden
  fixtures; the compat gate is now proven to reject mutation of
  published fixtures in both CI and the local pre-commit hook.
- **API parity held** (ADR-001): `conch` is a pure client of the public
  API — enforced structurally (the CLI cannot import `internal/server`).
- The stdlib mux + one small hub package was enough; no framework was
  ever missed.

## Recommendation for P1

1. **Start #9 (schema freeze) immediately on a proceed verdict** —
   protocol-designer owned, Nick sign-off. Pin in the envelope: UTC
   timestamps with fixed precision, explicit versioned payload names,
   opaque preservation of unknown payload schemas.
2. Add `-race` to the CI test step before the approval state machine
   lands (#12) — the hub is race-clean today, but approvals are where a
   data race would hurt most.
3. Keep the demo script as a living artifact: extend it into the
   `dogfood-check` canary once MCP + approvals exist (P1 #19).
4. Watch item: WS endpoint has no auth (P2 #24 by design). Fine bound to
   localhost in P0/P1; revisit before any non-local deployment.

## Decision requested from Nick

- [ ] Accept verdict **PROCEED** (or overrule with **STOP**)
- [ ] On proceed: green-light #9 so the P1 schema freeze starts
