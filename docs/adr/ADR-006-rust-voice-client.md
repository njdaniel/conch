# ADR-006: A Rust voice client, `conch-voice`

- **Status:** Accepted (Nick merged #86, 2026-10-05)
- **Date:** 2026-10-05
- **Deciders:** Nick (Tier-H)
- **Proposal:** #84 · **Amends:** [ADR-000](ADR-000-charter.md) D1
- **Amended 2026-10-13** (#251, Nick's decision): `dmn-audio` comes from its own public repository. The dependency line and the last-but-one consequence below carry the change.

## Context

Voice ([ADR-004](ADR-004-voice-via-livekit.md)) needs a native client: low-latency capture and playback, mixing, and a global push-to-talk key that reports release — none of which a terminal UI can do. Two facts point at Rust for that one binary:

- `njdaniel/daemon` has a tested, real-time-safe PipeWire capture layer (`crates/dmn-audio`), voice-activity detection (`crates/dmn-vad`), and speech-to-text (`crates/dmn-stt`). It has no networked voice, playback, mixing, or hotkeys.
- LiveKit publishes a Rust client SDK. Native audio from Go would need cgo, which D2 and [ADR-002](ADR-002-single-binary-sqlite.md) avoid.

## Decision

- **D1 becomes:** Go, single module, for `conchd` and `conch`; one Rust binary, `conch-voice`, for native audio.
- `conch-voice` lives in this repo under `voice/` as its own Cargo workspace.
- It is a client of the public REST/WS API (D6). It gets no private server surface.
- **Schema-first holds** (D8): its wire types are checked in CI against the golden fixtures in `pkg/schema/testdata`. `pkg/schema` stays the single source of truth.
- **Dependencies:** a Rust allowlist enforced by `cargo-deny`, mirroring `deps-allowlist.txt` and `scripts/depgate.sh`. New crates need Nick's sign-off. `dmn-audio` is a git dependency pinned to a commit of its own public repository, [`njdaniel/dmn-audio`](https://github.com/njdaniel/dmn-audio) (*amended 2026-10-13*; as accepted, this line did not say where the crate lived).
- **Platform:** Linux with PipeWire. Other platforms need a different audio layer and a later decision.
- `make check` gains a Rust leg (fmt, clippy, test, deny) once `voice/` exists.

## Consequences

- Two toolchains in one repo and longer CI. Measured in the V0 spike (#87): a clean release build of a minimal client took about 41 s on 16 cores across 316 crates (libwebrtc is downloaded prebuilt), producing a 36.7 MB binary that links only libc, libm, and libgcc.
- **Build toolchain (spike finding):** the LiveKit SDK's prebuilt libwebrtc requires clang 21 or newer. Ubuntu/Pop!_OS 24.04 ships 18 and packages at most 20, so CI will install a pinned LLVM release (cached) and the repo will document the same step. This is a standing cost for anyone building `voice/`; it does not affect building `conchd` or `conch`.
- The spike bypassed `dmn-audio` (it piped `pw-record`/`pw-play`), so integrating it remains unproven V4 work.
- The deployment invariant (D3) is unaffected: text, approvals, and audit need neither `conch-voice` nor LiveKit.
- Conch is coupled to `dmn-audio`, which must first gain a configurable sample rate and an output stream. *Amended 2026-10-13:* as accepted, this read "coupled to `daemon`'s `dmn-audio` … If both projects keep changing it, extract it to its own repo." The extraction came first and for a different reason: `daemon` is a private repository and Conch is public, so the dependency could not be fetched by CI or by anyone building from a public checkout. The crate now lives in `njdaniel/dmn-audio` (MIT), extracted unchanged from `daemon` commit `360d613`, and the two additions are made there.
- CLAUDE.md rule 6 ("Idiomatic Go") and the worker agent definitions need Rust counterparts when the crate lands.
