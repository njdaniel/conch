# Design: the approval object

- **Status:** Draft for P1 implementation (governing decision: D9, [ADR-000](../adr/ADR-000-charter.md); notifications: D7)
- **Owner:** protocol-designer (schemas), server-engineer (state machine)

Approval objects are a **first-class entity, not a message subtype**. They have their own store, their own state machine, their own audit events. A message may *reference* an approval (so it renders in a channel), but the approval's lifecycle is independent of any message.

## 1. Entity

An approval object carries:

| Field | Notes |
|---|---|
| `id` | Server-assigned, opaque, unique. |
| `requester` | Principal (usually an agent) that created it. |
| `channel` | Channel it is raised in (renders there; scopes who sees it). |
| `title` / `body` | Rendered form for humans (TUI inbox, ntfy notification text). |
| `payload` | Optional typed machine payload (versioned schema from `pkg/schema`, D8) — e.g. the trade signal being approved. |
| `options` | **Typed options**, not free text: a non-empty list of `{id, label, kind}` where `kind ∈ {approve, reject, custom}`. Minimum viable set is one approve + one reject option; custom options allow "approve with size X"-style decisions. |
| `deadline` | Absolute RFC 3339 timestamp. Required — an approval that can wait forever is a bug in the requester. |
| `quorum` | Number of concurring decisions required to resolve (default 1). Decisions concur if they select the same option. |
| `escalation_target` | Principal or ntfy topic notified when the deadline passes unresolved (D7: second topic, priority=urgent). |
| `state` | See state machine. |
| `created_at` | Server-assigned. |

Exact Go types and JSON encoding live in `pkg/schema` (P1 issue); this document is the semantic contract they must satisfy.

## 2. State machine

```
             ┌──────────┐
  create ───▶│ pending  │
             └────┬─────┘
      decision(s) │ deadline passes
      meet quorum │        │
        ┌─────────┘        ▼
        ▼             ┌───────────┐  grace period ends /
   ┌──────────┐       │ escalated │  escalated decision
   │ resolved │◀──────┴─────┬─────┘
   └──────────┘             │ no decision by final deadline
        ▲                   ▼
        │              ┌─────────┐
        └── (terminal) │ expired │ (terminal)
                       └─────────┘
```

- **pending** — open for decisions.
- **resolved** — quorum met. Terminal. Carries exactly one resolution event.
- **escalated** — deadline passed while pending; escalation notification fired (urgent topic). Still decidable: decisions during escalation resolve it normally. Escalation is a notification/priority state, not a verdict.
- **expired** — final deadline (escalation grace period, default: equal to the original deadline window, configurable per approval) passed with no quorum. Terminal. Expiry produces a resolution event with `outcome: expired` so waiters always get a definitive answer.

Invariants:

- Terminal states never transition. A decision against a resolved/expired approval is a protocol error.
- Every transition writes an audit event **in the same transaction** as the state change.
- Concurrent decisions serialize through the store (SQLite single writer); the quorum check happens inside the transaction, so exactly one decision can be the resolving one.

## 3. Decisions and resolution

A **decision** is cast by a human principal via `conch approve <id> --reason "..."` (or reject/choose-option variants) — never by agents on their own approvals. Each decision records: principal, selected option id, **required free-text reason**, timestamp. An empty reason is rejected at the API layer, not just the CLI.

When quorum is met, the server emits exactly one **resolution event**:

```jsonc
// semantic shape — canonical schema lands in pkg/schema as approval.resolution.v1
{
  "approval_id": "...",
  "outcome": "approved | rejected | custom | expired",
  "option_id": "...",          // absent for expired
  "decisions": [                // every concurring (and dissenting) decision
    {"principal": "...", "option_id": "...", "reason": "...", "at": "..."}
  ],
  "resolved_at": "..."
}
```

The resolution event is what waiters receive, what the audit log stores, and what `check_decision` returns — one shape, one source of truth (the **shared resolution store**).

## 4. Blocking vs async — `await_decision` and `check_decision` (D9)

Both MCP tools read the same resolution store; they are two access patterns, not two systems.

- **`await_decision(approval_id, timeout)`** — blocks until the approval reaches a terminal state or `timeout` elapses. On resolution/expiry: returns the resolution event. On timeout: returns a non-terminal answer (`state: pending|escalated`, no resolution) — the caller may re-await or switch to polling. Timeout is bounded server-side (cap TBD in implementation) so agents can't hold connections open indefinitely.
- **`check_decision(approval_id)`** — returns immediately: current state plus the resolution event if terminal. Idempotent, cheap, safe to poll.

Guarantees:

- A resolution is delivered consistently: any number of awaiters and pollers all see the identical resolution event.
- Await-then-crash loses nothing: the resolution persists; the agent re-attaches with `check_decision`.
- Ordering: the audit log's event order is the truth; notification delivery order is best-effort.

## 5. Notifications (D7)

- On **create**: push to the configured ntfy approvals topic (title, requester, deadline, channel).
- On **escalation**: push to the second ntfy topic with `priority: urgent`.
- On **resolution**: push a confirmation to the approvals topic (so the phone thread closes the loop).
- ntfy is optional (single-binary invariant, ADR-002): delivery failure is recorded as an audit event (`notify_failed`) and never blocks the approval lifecycle. Decisions happen only via `conch` — ntfy is reachability, not an action channel.

## 6. Audit chain

Minimum audit events per approval: `approval_created` → (`notify_sent` | `notify_failed`) → [`decision_cast` ...] → (`approval_resolved` | `approval_escalated` → ... → `approval_expired`).

The P1 success criterion (ROADMAP) asserts this exact chain end-to-end, and every approval-path PR ships a test that walks it (CLAUDE.md rule 3).

## 7. Explicitly out of scope (for now)

- Delegation ("decide on my behalf"), approval templates, recurring approvals — P2+ if ever, new design doc.
- Agent-cast decisions. Humans decide; agents request and observe. Revisiting this requires an ADR.
- Vetoes / weighted quorum. Quorum is a simple count of concurring decisions in P1.
