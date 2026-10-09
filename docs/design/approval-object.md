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

### 5.1 What is guaranteed (issue #158)

With a notifier configured, each of the four transitions that has a notification (created, escalated, resolved, expired) gets **at least one delivery attempt, and an audit row for each attempt**: `notify_sent`, or `notify_failed` with the reason. When `conchd` cannot make the attempt at all, it writes `notify_failed` with `error="not attempted: …"`. A transition is never left with only a log line. A notification can arrive twice (after a crash, below); it is never retried merely because ntfy refused it — that is a `notify_failed` row, and the deadline escalation is the backstop.

The row is the durable record that an attempt was made. Nothing else is stored: no table and no queue, so the guarantee is kept by the following rules.

| What goes wrong after the transition has committed | What `conchd` does |
|---|---|
| The approval (or its resolution) cannot be read back | Retries the read every 5 s, five times; then writes `notify_failed` with `error="not attempted: the approval could not be loaded"`. |
| The audit row cannot be written | Keeps the row in memory and retries every 5 s until it is written. The notification is not sent again. |
| The notifier panics | That delivery is recorded as `notify_failed` with `error="the notifier panicked"`. The approval keeps its timers and `conchd` keeps running. |
| `conchd` stops between the commit and the row (crash, kill, or a retry above still pending at shutdown) | The next start finds the transition without a row and makes the attempt then. |
| The store fails at the deadline or the grace timer itself (escalate, expire, or reading the grace deadline) | The timer is re-armed for 5 s later instead of being dropped until the next restart. If the store had in fact carried the transition out and only reported failure, the retry finds it done and does what follows a commit: the notification and, for an escalation, the grace timer. |

**Finding what a stopped `conchd` left undone.** Each start appends one `approvals_started` audit row (subject `approvals`), whose detail is `notifications=on since=<id>` or `notifications=off since=<id>`. `<id>` is the last audit id that existed when the process began: everything after it belongs to that run. Before writing its row, a start reads the previous one. If that says `on` and a notifier is configured now, it reads the audit log from the previous row's `since` up to its own, and collects every `approval_created`, `approval_escalated`, `approval_resolved` and `approval_expired` in that range with no notify row for the same approval and event. Each is attempted, in log order, and gets its row. Only when every one of them has a row is the new `approvals_started` row written.

This runs before timers are armed and before the listener accepts. If it cannot finish then (the log cannot be read, a row or the start row cannot be written), `conchd` starts anyway and repeats it every 5 s until it has finished. A repeat concerns the same range, never the current run's own transitions, and does not deliver again what an earlier pass in the same process delivered; only the missing rows are retried. Because the start row carries `since`, it means the same thing written late as written at once.

Limits, stated so nobody has to discover them:

- Catch-up at start has a 10 s budget for deliveries, which also cuts short the delivery in progress, so an unreachable ntfy cannot hold up start-up for longer than that (connections made meanwhile wait; they are not refused). What is left when it is spent gets `notify_failed` with `error="not attempted: start-up time budget spent"`.
- At start, an approval that cannot be read is recorded as not attempted at once; the five retries above apply to a running `conchd`, not to catch-up.
- If notifications were on, `conchd` stopped, and it is next started with notifications **off**, what was owed is abandoned: that start writes `notifications=off`, and the log shows the transition, no notify row, and the reason.
- If a start cannot write its `approvals_started` row and `conchd` stops again before the retry succeeds, the next start reads from the older row. Should the notifier setting have differed between the two, that one run is misjudged: its transitions are not caught up (older row `off`) or are delivered late although notifications were off (older row `on`). The window is from the start until 5 s after the audit log accepts writes again, and no transition can commit while it does not.
- A database upgraded from a version without `approvals_started` rows has no starting point, so its first start catches up on nothing.
- With no notifier configured nothing is sent and no notify rows are written. "Configured" means an ntfy server is set and valid; `conchd` then has a notifier. (An ntfy server with a topic left empty still has one, and notifications for that topic are not sent; see the follow-up issue.)
- A late "created" notification can arrive for an approval that has since been decided. The resolution notification follows it.
- A notify row can appear more than once for one attempt: a write the store carried out but reported as failed is repeated.

## 6. Audit chain

Minimum audit events per approval: `approval_created` → (`notify_sent` | `notify_failed`) → [`decision_cast` ...] → (`approval_resolved` | `approval_escalated` → ... → `approval_expired`).

Notify rows follow their transition but are not written in its transaction, so other rows can come between (§5.1), and after a crash a transition's notify row appears after the next `approvals_started` row.

The P1 success criterion (ROADMAP) asserts this exact chain end-to-end, and every approval-path PR ships a test that walks it (CLAUDE.md rule 3).

## 7. Explicitly out of scope (for now)

- Delegation ("decide on my behalf"), approval templates, recurring approvals — P2+ if ever, new design doc.
- Agent-cast decisions. Humans decide; agents request and observe. Revisiting this requires an ADR.
- Vetoes / weighted quorum. Quorum is a simple count of concurring decisions in P1.
