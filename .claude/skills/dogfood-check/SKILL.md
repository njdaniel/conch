---
name: dogfood-check
description: Scripted end-to-end approval-loop check — agent connects via MCP, requests approval, ntfy fires, human resolves via conch approve, audit chain asserted. Run as the canary before any release and after approval-path merges.
---

# dogfood-check

The whole pitch of Conch is one loop (ROADMAP P1 success criterion). This skill verifies it end-to-end against a real running `conchd`.

**Automated:** `go run ./e2e/dogfood` runs this whole loop against real `conchd`/`conch` binaries (both the happy path and the ntfy-unreachable degraded path) and exits nonzero on any assertion failure. CI runs it on every push to `main` and on any PR labeled `approval-path`. The manual steps below are the reference the script encodes — use them if the script itself needs debugging.

## The loop under test

1. Start `conchd` fresh (temp SQLite db, test config, ntfy topic pointed at a test topic or a local ntfy container — remember ntfy is *optional*: also verify step 4 still works with ntfy unreachable).
2. Connect a test agent via the MCP endpoint; call `post_message` with a typed payload; verify it lands in the channel via `read_channel` **and** via the REST API (parity check).
3. Agent calls `request_approval` (typed options, short deadline) and blocks on `await_decision`; a second agent call polls `check_decision` (both semantics must work against the same approval).
4. Verify the ntfy notification fired (or degraded gracefully if ntfy is down — approval must still be resolvable).
5. Resolve as a human: `conch approvals list` shows it; `conch approve <id> --reason "dogfood"` resolves it.
6. Assert: `await_decision` unblocked with the structured resolution (decision + reason + resolver); `check_decision` returns the same resolution.
7. Assert the audit chain: audit log contains request → notify (or notify-failed) → resolve events, in order, attributed to the right principals.
8. Bonus path: let a second approval's deadline expire; assert the escalation event and urgent-topic notification.

## Pass/fail

- **Pass:** every assertion in 2–7 holds. Report the run in the release notes / PR.
- **Fail:** file an issue with the failing step and transcript. A release **does not ship** on a failing dogfood-check; an approval-path merge with a failing check gets reverted or hot-fixed the same day.
