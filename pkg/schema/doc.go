// Package schema is the single source of truth for every wire shape in Conch:
// message envelopes, typed payload schemas, approval objects, and resolution
// events. Nothing elsewhere in the codebase hand-rolls JSON shapes (golden
// rule 2, ADR-000 D8).
//
// Payload schemas are versioned (e.g. leviathan.trade_signal.v1). Breaking
// changes require a version bump and Nick's sign-off; compatibility is
// enforced against golden fixtures in testdata/ by scripts/schema-compat.sh.
//
// Agent presence (P3) adds the documents the events socket streams, each
// carrying its own schema name so a reader can tell frames apart:
//
//   - SessionV1 (conch.session.v1), one live client or runtime of a
//     principal, and SessionRosterV1 (conch.session_roster.v1), every session
//     visible in a channel.
//   - RunV1 (conch.run.v1), one piece of work an agent session does, with
//     the lifecycle in RunTransitionAllowed and RunStateTerminal.
//   - AgentActivityV1 (conch.agent.activity.v1), one event in a run's
//     activity stream.
//   - InterventionV1 (conch.intervention.v1), a person's request to a run
//     and the adapter's answer.
//   - ReadStateV1 (conch.read_state.v1), how far a person has read a channel
//     and how many messages they may see past it.
//
// Their request and response bodies (CreateSessionRequestV1,
// CreateRunRequestV1, SetRunStateRequestV1, ListRunsResponseV1,
// AppendActivityRequestV1, ListActivityResponseV1,
// CreateInterventionRequestV1, AckInterventionRequestV1 and
// ListInterventionsResponseV1, MarkReadRequestV1 and ListReadStateResponseV1)
// live beside them.
package schema
