# P3 first-prototype exit record

Issue #286. This is the record of Nick's own run of
[`docs/p3-walkthrough.md`](../p3-walkthrough.md) from a clean checkout: the exit of the
first P3 prototype (the local multi-client slice of ADR-007), kept apart from completion
of P3 itself. The rows are those of the brief's test table,
[`p3-local-prototype.md`](p3-local-prototype.md) §4.

> **Status: draft, waiting for Nick's run.** Every field marked *pending* is filled in
> from that run and from nothing else. The automated results below are context, not a
> substitute: the brief asks for the by-hand run because pure API tests do not establish
> client UX.

## The run

| | |
| --- | --- |
| Date | *pending* |
| Commit | *pending* (`git rev-parse HEAD` of the clean checkout) |
| OS | *pending* |
| Terminal | *pending* (program and version, size used) |
| Browser | *pending* (name and version) |
| Go | *pending* (`go version`) |
| Node | *pending* (`node --version`; `web/.nvmrc` pins 24.21.0) |
| Started with | `go run ./e2e/p3-local --keep` |

## Rows

Result is **pass**, **fail** or **not run**. A failed row gets its own issue, and #286 stays
open until it passes or Nick accepts it as known (#286, Design 4).

| Row | What | Result | Note |
| --- | --- | --- | --- |
| P3-01 | Two TUIs, the browser and two scripted agents connect with separate credentials; the views show the real principals and sessions | *pending* | |
| P3-02 | A message from each human client and the permitted agent is seen once, in order, by each authorized peer; history agrees after reload | *pending* | |
| P3-03 | Different channels at once: each client keeps its own selection and draft | *pending* | |
| P3-04 | One person in two clients: two sessions; ending one leaves the other; revoking one credential affects only its scope | *pending* | |
| P3-05 | Restricted channel, net and whisper: insiders see it in history, live and search; outsiders cannot fetch or subscribe | *pending* | |
| P3-06 | A scripted agent through idle, running, waiting, completed and failed: TUI and browser agree with the API on ids, task, outcome and activity | *pending* | |
| P3-07 | An agent stopped: stale, then offline within the threshold; no invented outcome; reconnection keeps one identity | *pending* | |
| P3-08 | Instruction and cancel acknowledged and enforced; unsupported operations absent or disabled with a reason | *pending* | |
| P3-09 | Client connection dropped, then the server restarted: stale status, bounded retries, history restored, no silent resend | *pending* | |
| P3-10 | Membership removed and a credential revoked: live access ends, read/write/reconnect refused, others unaffected | *pending* | |
| P3-11 | No LiveKit and no model: chat, scripted agents, approvals and audit work; optional features say unavailable | *pending* | |
| P3-12 | Reset: only the run's own processes and data removed; personal credentials and other Conch instances untouched | *pending* | |
| P3-13 | One real adapter, opted in: recorded separately below | *pending* | |

## P3-13: the real adapter

Run with `go run ./e2e/p3-local --keep --real-adapter` and `claude` on PATH. It runs as
Nick, with his credentials, and can cost money.

| | |
| --- | --- |
| Runtime | *pending* |
| Model | *pending* |
| Outcome | *pending* |
| Note | *pending* |

## What surprised him

*pending*

## Automated results at the time of the run (context only)

All of these are on `main` and run in CI on every pull request:

- `go run ./e2e/p3-local` (#283, #284): the setup checks and P3-01 to P3-12 against real
  binaries, with the TUIs on pseudo-terminals and the scripted agents as real
  `conch-adapter` processes. Each server mutation recorded in #341 makes the suite fail
  and name its row. P3-13 is reported not run.
- `make web-e2e` (#285): the browser's part of P3-01, 02, 04, 05, 06/08 and 09 in
  Chromium.

## What remains of P3

Filed under the P3 milestone; the roadmap's P3 section links them. The P3 success
criterion is unchanged and is not met by this slice.

| Issue | Item |
| --- | --- |
| #343 | `conch-adapter`: Codex runtime profile |
| #344 | `conch-adapter`: local model (Ollama), and three runtimes at once |
| #345 | `conch-adapter`: `pause` and `resume` |
| #346 | TUI and web: `pause` and `resume` controls |
| #347 | The web client embedded in the `conchd` binary |
| #348 | TypeScript bindings generated from `pkg/schema` |
| #349 | TUI: Runs view and a richer run timeline |
| #350 | Web: Runs view and a richer run timeline |
| #351 | Read state: a per-person read mark and unread counts on the API |
| #352 | Web: composing to nets and whispers |
| #353 | VM/LAN extension of the local prototype (tracking) |
| #354 | Voice extension of the local prototype (tracking, gated on #189) |
