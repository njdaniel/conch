# ADR-003: Multi-human access without the GUI

- **Status:** Accepted (Nick merged #86, 2026-10-05)
- **Date:** 2026-10-05
- **Deciders:** Nick (Tier-H)
- **Proposal:** #75 · **Amends:** [ADR-000](ADR-000-charter.md) non-goals (multi-human GUI line), D12

## Context

The roadmap placed every multi-human concern at P7, bundled with a Slack-style web GUI. The nets-and-voice track ([ADR-004](ADR-004-voice-via-livekit.md), [ADR-005](ADR-005-nets-and-whispers.md)) is for groups of people — two squads of four with leads on a command net — and cannot be built or tested with one human. The bundle is what no longer fits; the GUI's position is unchanged.

## Decision

- A small number of **human principals** (Nick plus collaborators) may use one instance now, each with their own local account and session.
- **Channel membership** exists and is enforced server-side on every read and write path (REST, WebSocket, MCP).
- Human and agent principals keep distinct auth, permission, and rate-limit models (D10).
- Clients are the existing TUI/CLI and `conch-voice` ([ADR-006](ADR-006-rust-voice-client.md)).

Unchanged:

- The Slack-style web GUI stays at P7 and still requires its own ADR. This ADR does not permit it.
- Standalone human-to-human DMs stay at P7. In-channel whispers are ADR-005's subject.
- Single-tenant: one binary = one org.

## Consequences

- P2's auth work (#24) and the capability epic (#20) become the track's first phase (V1), with new channel-membership work added.
- REST and WebSocket become authenticated. `CONCH_AUTHOR` stops being the identity source — a breaking change for scripts and the dogfood harness.
- Sessions and membership checks are new security surface; implementation PRs get a `security-reviewer` pass.
- Until that pass is done, the documented deployment stays localhost/VPN/reverse proxy.
- The deployment invariant (D3, [ADR-002](ADR-002-single-binary-sqlite.md)) is unaffected.
