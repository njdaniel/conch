# ADR-004: Voice via LiveKit, as a parallel track

- **Status:** Proposed — accepted when Nick merges the PR for #85
- **Date:** 2026-10-05
- **Deciders:** Nick (Tier-H)
- **Proposal:** #76 · **Amends:** [ADR-000](ADR-000-charter.md) non-goals (voice line); [ADR-002](ADR-002-single-binary-sqlite.md) LiveKit bullet

## Context

ADR-000 barred voice before P8. Nick wants layered voice — several nets heard at once, one transmitted on at a time, whispers to individuals — and decided on 2026-10-05 to run it as a track parallel to P2–P7. The audience model in [ADR-005](ADR-005-nets-and-whispers.md) is shared by text and voice; designing them together avoids text nets that voice cannot later map onto.

## Decision

- **Voice starts now.** Video and screen sharing remain at P9.
- **LiveKit only**, self-hosted, as an optional process. Never bespoke WebRTC.
- **`conchd` is the only authority** on who may publish and who may hear. It issues short-lived LiveKit tokens and controls room and track access. LiveKit only moves audio.
- **Graceful degradation:** with LiveKit absent or down, voice endpoints report "voice not configured/unavailable" and messaging, approvals, and audit are unaffected.
- **Client:** the native Linux binary `conch-voice` ([ADR-006](ADR-006-rust-voice-client.md)). No browser client.
- **Push-to-talk** is the default transmit mode.
- **Audit** records join, leave, and transmit start/stop with audience. **Audio is not recorded.**

Deferred, not decided here: agents as voice participants (live transcription to typed messages, `publish:audio` TTS), recording, video and screen share.

### Settled by spike before implementation

A throwaway spike (track phase V0) answers these; its report lands in `docs/reports/`:

1. **Audience mapping** — one room per channel with server-controlled subscriptions, or one room per net with clients joined to several. Deciding test: a non-audience client cannot receive the track.
2. **No new Go dependency** — whether token minting (HS256 JWT) and room control (HTTP API) work from the standard library. If not, a LiveKit Go SDK needs separate dependency sign-off.
3. **Echo cancellation and noise suppression** in the Rust SDK; otherwise the MVP is headphones-only.
4. **Global push-to-talk on Wayland**, including key release.

## Consequences

- For anyone using voice, deployment becomes `conchd` + LiveKit: the first optional *server* process. Docs must say so plainly.
- Voice depends on authenticated humans and channel membership ([ADR-003](ADR-003-multi-human-access.md)); it cannot start before them.
- The P2 → P3 order is displaced: auth is pulled forward and voice competes with P3 (agent presence) for attention.
- P9 reuses this integration: one LiveKit design, not two.
