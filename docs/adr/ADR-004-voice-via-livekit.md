# ADR-004: Voice via LiveKit, as a parallel track

- **Status:** Accepted (Nick merged #86, 2026-10-05)
- **Date:** 2026-10-05
- **Deciders:** Nick (Tier-H)
- **Proposal:** #76 · **Amends:** [ADR-000](ADR-000-charter.md) non-goals (voice line); [ADR-002](ADR-002-single-binary-sqlite.md) LiveKit bullet

## Context

ADR-000 barred voice before P8. Nick wants layered voice — several nets heard at once, one transmitted on at a time, whispers to individuals — and decided on 2026-10-05 to run it as a track parallel to P2–P7. The audience model in [ADR-005](ADR-005-nets-and-whispers.md) is shared by text and voice; designing them together avoids text nets that voice cannot later map onto.

## Decision

- **Voice starts now.** Video and screen sharing remain at P9.
- **LiveKit only**, self-hosted, as an optional process. Never bespoke WebRTC.
- **`conchd` is the only authority** on who may publish and who may hear. It issues short-lived LiveKit tokens and controls room access. LiveKit only moves audio.
- **One LiveKit room per net** (settled by the V0 spike, as this ADR provided; accepted by Nick, 2026-10-05). Channel-wide voice is one more room. A client joins a room for each net it may hear; the room name is inside the token `conchd` signs, so an outsider has nothing to request.
- **No LiveKit Go dependency** (same basis). `conchd` mints tokens and calls the room API with the standard library.
- **Graceful degradation:** with LiveKit absent or down, voice endpoints report "voice not configured/unavailable" and messaging, approvals, and audit are unaffected.
- **Client:** the native Linux binary `conch-voice` ([ADR-006](ADR-006-rust-voice-client.md)). No browser client.
- **Push-to-talk** is the default transmit mode.
- **Audit** records join, leave, and transmit start/stop with audience. **Audio is not recorded.**

Deferred, not decided here: agents as voice participants (live transcription to typed messages, `publish:audio` TTS), recording, video and screen share.

### Spike findings

From the V0 spike (#87; report `docs/reports/2026-10-05-voice-spike.md`, LiveKit server 1.13.7, Rust SDK 0.9.3):

| Question | Finding |
|---|---|
| Audience mapping | Rooms per net. In a single room, a server-driven subscription returned success but delivered no audio when the token denied subscribing, and nothing stops an outsider with a normal token. With a room per net, an outsider received nothing; four rooms at once cost about 7% of one core. |
| Live changes | `conchd` can remove a participant or revoke their subscribing mid-stream through the room API. |
| Go dependency | None needed: HS256 token minting and the HTTP room API work from the standard library. |
| Echo cancellation, noise suppression | The SDK's audio processing module is usable from Rust and ran without error. Its effectiveness with open speakers is **not measured**; treat the MVP as headphones-recommended until it is. |
| Global push-to-talk | COSMIC exposes no global-shortcuts portal, so the client reads the keyboard through `evdev`. That needs read access to the input device, which lets the process see every keystroke; setup must say so. |

Not yet verified: hold-to-talk end to end, two machines on a LAN, echo cancellation by ear, publishing into several rooms from one process, token expiry and refresh, and anything beyond loopback (NAT, loss).

V0's stated exit in [ROADMAP.md](../../ROADMAP.md) — two machines talking with hold-to-talk — is therefore **not yet met**. Those manual checks stay open under #87. They do not block V1 or V2, which involve no voice, and must be closed before V4.

**Whisper mapping is proposed, not decided.** The spike suggests a per-principal "inbox" room in which only the owner can subscribe, with whispers sent on a publish-only token; it is assembled from tested behaviours but was not run as a whole. It gets its own test before V5 commits to it.

## Consequences

- For anyone using voice, deployment becomes `conchd` + LiveKit: the first optional *server* process. Docs must say so plainly.
- Voice depends on authenticated humans and channel membership ([ADR-003](ADR-003-multi-human-access.md)); it cannot start before them.
- The P2 → P3 order is displaced: auth is pulled forward and voice competes with P3 (agent presence) for attention.
- P9 reuses this integration: one LiveKit design, not two.
