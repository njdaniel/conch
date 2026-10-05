# ADR-006: A Rust voice client, `conch-voice`

- **Status:** Proposed — accepted when Nick merges the PR for #85
- **Date:** 2026-10-05
- **Deciders:** Nick (Tier-H)
- **Proposal:** #84 · **Amends:** [ADR-000](ADR-000-charter.md) D1

## Context

Voice ([ADR-004](ADR-004-voice-via-livekit.md)) needs a native client: low-latency capture and playback, mixing, and a global push-to-talk key that reports release — none of which a terminal UI can do. Two facts point at Rust for that one binary:

- `njdaniel/daemon` has a tested, real-time-safe PipeWire capture layer (`crates/dmn-audio`), voice-activity detection (`crates/dmn-vad`), and speech-to-text (`crates/dmn-stt`). It has no networked voice, playback, mixing, or hotkeys.
- LiveKit publishes a Rust client SDK. Native audio from Go would need cgo, which D2 and [ADR-002](ADR-002-single-binary-sqlite.md) avoid.

## Decision

- **D1 becomes:** Go, single module, for `conchd` and `conch`; one Rust binary, `conch-voice`, for native audio.
- `conch-voice` lives in this repo under `voice/` as its own Cargo workspace.
- It is a client of the public REST/WS API (D6). It gets no private server surface.
- **Schema-first holds** (D8): its wire types are checked in CI against the golden fixtures in `pkg/schema/testdata`. `pkg/schema` stays the single source of truth.
- **Dependencies:** a Rust allowlist enforced by `cargo-deny`, mirroring `deps-allowlist.txt` and `scripts/depgate.sh`. New crates need Nick's sign-off. `dmn-audio` is a git dependency pinned to a commit.
- **Platform:** Linux with PipeWire. Other platforms need a different audio layer and a later decision.
- `make check` gains a Rust leg (fmt, clippy, test, deny) once `voice/` exists.

## Consequences

- Two toolchains in one repo and longer CI. The LiveKit SDK brings a prebuilt libwebrtc; its build weight is measured in the V0 spike.
- The deployment invariant (D3) is unaffected: text, approvals, and audit need neither `conch-voice` nor LiveKit.
- Conch is coupled to `daemon`'s `dmn-audio`, which must first gain a configurable sample rate and an output stream. If both projects keep changing it, extract it to its own repo.
- CLAUDE.md rule 6 ("Idiomatic Go") and the worker agent definitions need Rust counterparts when the crate lands.
