# ADR-005: Nets and whispers — audience-scoped speaking

- **Status:** Proposed — accepted when Nick merges the PR for #85
- **Date:** 2026-10-05
- **Deciders:** Nick (Tier-H)
- **Proposal:** #82 · **Design:** [nets-and-whispers.md](../design/nets-and-whispers.md) · **Amends:** [ADR-000](ADR-000-charter.md) D11, D12

## Context

Every message in a channel goes to everyone in it. Layered comms — squads with their own net, leads bridging on a command net, a word to one person — cannot be expressed. Separate channels give separation but not layering: one participant monitoring several scopes and transmitting on one, with one log and one audit trail. For voice the workaround fails outright, since nobody can usefully sit in several rooms.

## Decision

- **Audience on every utterance.** A message or audio track is addressed to the whole channel (default, unchanged), a **net**, or an explicit list of principals (**whisper**). The author is always in the audience.
- **Nets are flat sets within one channel.** Layering comes from overlapping membership. Members listen and transmit; monitors listen only.
- **Visibility is fixed at post time.** Joining later reveals no history; leaving hides nothing already received.
- **Server-side, fail closed.** A principal outside the audience never receives the utterance on any path. One store-level visibility predicate serves every read path.
- **Discretion, not secrecy.** No E2EE. Every scoped utterance is in the audit log with its audience and resolved recipients.
- **No agent-only audiences.** An audience must contain at least one human principal. Agents cannot hold a conversation no human receives.
- **Capability-gated** through the D10 manifest: transmitting on a net and whispering are per-channel grants.
- **Approvals stay channel-wide.** This decision does not touch the approval path.
- **Voice uses the same audience model** ([ADR-004](ADR-004-voice-via-livekit.md)).

### Answers to the open questions in #82

| Question | Decision |
|---|---|
| Flat nets or a unit tree? | Flat. A tree may be added later as presentation. |
| Post-time snapshot or live membership? | Post-time snapshot. |
| New envelope version or additive field? | New version (`conch.message.v2`). A v1 client must not render a whisper as an open message. |
| Whispers with nets, or held to P6? | With nets. |
| May agents whisper during an assembly? | No, except the facilitator. |

### Charter amendments

- **D11** becomes: no team abstraction *above* the channel. Nets are subsets within a channel.
- **D12** becomes: in-channel whispers are permitted with nets; standalone DMs remain at P6 (agents) and P7 (humans).

## Consequences

- `pkg/schema`: new message envelope version, audience and net types, changed MCP tool schemas — via the `schema-change` procedure.
- Storage: tables for nets, net members, and per-message recipients; existing rows unchanged.
- **The main cost is read-side filtering.** REST list, WebSocket fan-out, MCP `read_channel`, and — as they land — search (#22), audit export (#21), threads (#25), plus webhook and ntfy fan-out must all apply the predicate. One miss leaks whispers. A leak test over every read path is required, and implementation PRs get a `security-reviewer` pass.
- Requires authenticated readers and channel membership ([ADR-003](ADR-003-multi-human-access.md)) first.
- The server controls who receives an utterance, not what a recipient repeats.
