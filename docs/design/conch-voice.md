# Design: the `conch-voice` client

- **Status:** Proposed for V4 implementation, 2026-10-09 (governing decisions: [ADR-004](../adr/ADR-004-voice-via-livekit.md) and [ADR-006](../adr/ADR-006-rust-voice-client.md), both Accepted). §12 lists what still needs Nick's sign-off; §13 lists what is assumed and not yet measured.
- **Touches:** [voice-control-plane.md](voice-control-plane.md) (the server this is a client of; §6 below extends its audit), [ADR-002](../adr/ADR-002-single-binary-sqlite.md) (optional processes), [ADR-003](../adr/ADR-003-multi-human-access.md) (the login it reuses).
- **Owner:** principal engineer (this note, the workspace, the exit test), protocol-designer (the one new wire shape), server-engineer (transmit reports in `conchd`).
- **Issues:** epic #176, this note #175. Slices: workspace #177, SDK measurements #178, schema #179, server #135, the three library crates #180 to #182, the binary #183, devices #184, keys #185, echo #186, TUI #187, end-to-end #188, exit #189.
- **Evidence:** the V0 spike report (`docs/reports/2026-10-05-voice-spike.md`, PR #88) and voice-control-plane.md §10.

The ADRs decide that voice has one native client, `conch-voice`: a Rust binary under `voice/`, a client of the public API, Linux with PipeWire, built on LiveKit's Rust SDK and on `dmn-audio` from `njdaniel/daemon`. This note says how it is built and how it behaves. It covers the client and the one server addition the client needs (transmit reports, §6).

## 1. What V4 ships, and what it does not

V4 ships channel-wide voice for people: a member of a channel runs `conch-voice join <channel>`, hears everyone else in the channel's room mixed together, and is heard while holding one key. They can mute, deafen, change audio device without restarting, and see who is connected and who is talking. Each press and release is reported to `conchd` and audited.

Not in V4:

- **Nets and whispers in voice** (V5). The client joins the channel's room only and ignores any net grant in a session.
- **Hardening and packaging** (V6): behaviour on a bad link, noise suppression, a setup doctor, release artefacts.
- **Agents in voice, recording, video.** Deferred by ADR-004.
- **Any platform but Linux with PipeWire** (ADR-006).

## 2. Layout, build and gates

`voice/` is its own Cargo workspace (ADR-006) with four crates:

| Crate | Holds | Links native code |
|---|---|---|
| `conch-voice-api` | The client of `conchd`: the stored login, the REST calls voice needs, the presence socket, and Rust copies of the wire types | no |
| `conch-voice-audio` | The transmit gate, per-speaker jitter buffers, the mixer, test sources and sinks | no |
| `conch-voice-control` | Configuration, key-event decoding, the push-to-talk state machine, the connection policy | no |
| `conch-voice` | The binary: LiveKit transport, PipeWire through `dmn-audio`, the terminal | yes |

Only the binary needs the SDK's libwebrtc and PipeWire. The other three are plain Rust, so nearly all of the logic builds and is tested without the heavy toolchain. All four set `#![forbid(unsafe_code)]`; the native code is reached only through `livekit` and `dmn-audio`.

**Wire types.** `pkg/schema` stays the single source of truth (ADR-006). `conch-voice-api` has hand-written Rust types for the shapes it uses, and a test decodes and re-encodes every golden fixture for those shapes in `pkg/schema/testdata` and compares the JSON. A fixture that the Rust types cannot round-trip fails `make check`.

**Toolchain.**

- Rust is pinned in `voice/rust-toolchain.toml`. `Cargo.lock` is committed and every command runs with `--locked`.
- The SDK's prebuilt libwebrtc needs clang 21 or newer, and Ubuntu 24.04 ships 18 (ADR-006). `scripts/voice-toolchain.sh` downloads one pinned LLVM release, checks its SHA-256, and unpacks `clang` into `~/.cache/conch/`. Nothing is installed system-wide and no root is needed. CI runs the same script and caches the result.
- The `webrtc-sys` build script downloads a prebuilt libwebrtc from LiveKit's GitHub releases. That is a binary this project does not build. It is fixed by the exact `livekit` version in `Cargo.lock`. Whether the download is checksummed by the crate is not yet verified (§13).
- From the slice that adds real devices (§9), building the binary also needs the system packages `libpipewire-0.3-dev` and `libclang-dev`. Before that it needs none.

**Gates.** `make check` gains a Rust leg (ADR-006): `cargo fmt --check`, `cargo clippy -- -D warnings`, `cargo test`, the dependency gate and `cargo deny check`. It uses one shared target directory under `~/.cache/conch/` so that each git worktree does not rebuild 300 crates. If the toolchain is missing the leg fails and names the script to run; it does not skip.

**Dependencies** (decision 1, and §12):

- `voice/deps-allowlist.txt` lists the direct dependencies of the four crates, and `scripts/voice-depgate.sh` fails if a manifest names a crate that is not listed. This mirrors `deps-allowlist.txt` and `scripts/depgate.sh` exactly: direct dependencies are gated, transitive ones follow.
- `cargo-deny` checks what a list of names cannot: every crate comes from crates.io or from the one pinned git source of `dmn-audio` (§9), none has a known advisory, and every licence is on an allowed list.

## 3. The audio path

Everything is 48 kHz mono in 10 ms frames (480 samples), which is what the SDK takes and gives.

```text
microphone ─► capture ring ─► transmit gate ─► LiveKit track (published once, muted)
remote tracks ─► one jitter buffer per speaker ─► mixer ─► playback ring ─► speakers
```

- **Threads.** PipeWire's callbacks run on `dmn-audio`'s loop thread and only move samples to and from lock-free rings; they allocate nothing, lock nothing and log nothing (`daemon`'s rule, kept here). A pump on the client's side moves one frame every 10 ms in each direction.
- **Receiving.** Each remote microphone track feeds its own jitter buffer, which aims to hold 40 ms and never more than 200 ms: when it is over, the oldest audio is dropped, so a speaker is never heard late. The mixer adds the speakers' frames and clips the sum to the valid range. Per-speaker volume, stereo placement and ducking are V5.
- **Devices.** The microphone and the speakers are PipeWire's defaults unless named in the configuration. When a device disappears the client keeps running: capture re-attaches through `dmn-audio`'s `Reattach`, and playback follows the default sink. While there is no microphone the gate stays shut and the status line says so.
- **Echo.** V4 assumes headphones. The SDK's audio processing can be switched on in the configuration (`echo_cancellation`), with the playback mix as its reference. How well it removes echo from open speakers has not been measured (ADR-004), so it is off by default and the README says to wear headphones.

### The transmit gate

This is the property a user relies on: **microphone audio leaves the machine only while the talk key is held.**

- The gate is in `conch-voice-audio`, before any SDK call. While it is shut, captured frames are dropped and nothing is handed to the SDK. Muting the LiveKit track is a second layer and is what `conchd` observes; it is not the guarantee.
- The track is published once on joining, muted. A press opens the gate and unmutes the track; a release shuts the gate and mutes it. Neither waits for the other, or for the report in §6. Publishing on each press was rejected in voice-control-plane.md §6: it clips the first word.
- The gate also shuts, whatever the key is doing, when: the user has muted or deafened; the connection is not established; the session's grant has `can_publish: false`; the microphone is missing; the key device has gone away; or a single press has lasted longer than `max_transmit_secs` (default 120), which catches a stuck key or something resting on the keyboard. A shut gate reopens only on a new press.
- The microphone stream stays open while joined, so the desktop's "microphone in use" indicator is on the whole time, although nothing is sent until a press. Opening it on each press would lose the start of every transmission.
- `release_tail_ms` (default 100) keeps the gate open briefly after a release so the last syllable is not cut. It counts as part of the transmission: the release is reported when the gate shuts.

## 4. Keys

A terminal cannot see a key being released, and COSMIC offers no global-shortcuts portal (V0 spike), so the client reads the keyboard's event device directly.

- It opens the configured device read-only and reads fixed-size `input_event` records with ordinary file I/O: no crate, no `ioctl`, no `unsafe`. Only key events with a configured code are acted on (press, release; auto-repeat is ignored). Every other event is discarded as it is read. Nothing about keys is stored, logged or sent.
- The device is named by its stable path under `/dev/input/by-id/`, because `eventN` numbers change between boots.
- **What this access exposes, stated plainly:** read access to a keyboard's event device lets a process see every key pressed on that keyboard, in every application, passwords included. That `conch-voice` looks only at the talk key is a property of its code, not of the permission, and any other process running as the same user gets the same access. Setup therefore grants access to one device, not to the `input` group, and the README says what is being granted before it says how.
- Granting it is a one-line udev rule for that device, shown by `conch-voice devices`. A per-boot `setfacl` works for a trial.
- If the device disappears (unplugged, permission withdrawn), the gate shuts as for a release and the client retries opening it.
- Mute and deafen can be bound to keys on the same device. Deafen silences playback and also blocks transmitting, as in most voice tools.
- **Without a key device** the client still runs. It reads commands from standard input, one per line: `down`, `up`, `mute`, `deafen`, `quit`. That is how the tests drive it, and it is a usable fallback (a compositor shortcut bound to a script cannot hold a key, but a foot pedal or a wrapper can write lines).
- A desktop that gains a global-shortcuts portal later is another source of the same press and release events; nothing else changes.

## 5. Sessions and connection

The client follows voice-control-plane.md §4: it asks `conchd` for a session immediately before every connection attempt, and joins the room of the grant that has no `audience`.

- **It never stores a join token, and never prints one.** Tokens, room names and the login token do not appear in any output, log line or error, in either the terminal or `--json` mode.
- **Disconnects.** What it does next depends on why:

| What happened | Next |
|---|---|
| The room was deleted (a rotation, voice-control-plane.md §5) | Ask for a new session at once and join the channel's new room. If the user is still entitled this is a gap of about a second; if not, the request is refused and the client stops. |
| The same identity joined from elsewhere | Stop, saying so. Reconnecting would only displace the other device in turn. |
| `conchd` refuses the session: not signed in (401), not a member or no such channel (404), not a person (403), voice needs auth (400), voice not configured (503 `voice_not_configured`) | Stop with one line naming the reason and a nonzero exit. |
| `conchd` or LiveKit unreachable, `voice_unavailable`, the connection lost, the server shutting down | Retry: wait 1 s, doubling to 30 s, with jitter; the wait resets after 30 s connected. |

- **A press during any gap does nothing** (§3) and the status line says "not connected". A key held when the connection drops ends that transmission; it does not resume by itself when the connection returns.
- **The SDK's own resume is allowed** (decision 3). voice-control-plane.md §4 says the client must never reconnect with a token LiveKit sent it. The SDK resumes a briefly interrupted connection by itself, using LiveKit's refreshed token for the same room, and that cannot be switched off (§13). This is acceptable because nothing in the server's enforcement depends on the client: a room whose holders are all entitled is exactly a room that may be resumed, and a room that must be abandoned is deleted, which no token survives. What the rule protects is correctness after a rotation, and that is met: once the SDK gives up, the client always goes back to `conchd`.

## 6. Transmit reports

voice-control-plane.md §9 decision 1 left this to V4, and #135 recorded it: the poller can miss a press shorter than its interval, so the client reports each press and release, and the poller checks the client.

**Endpoint.** `POST /v1/channels/{channel}/voice/transmit` with a `VoiceTransmitReportV1` body: a `state` of `started` or `stopped`, and an optional `audience` (absent means the whole channel; V5 uses it for nets). The report carries no time. `conchd` stamps the moment it receives it, so a client cannot back-date or post-date its own record.

- The caller is checked as for a session: a verified, human member of the channel. On top of that, the credential must have been issued a session for the channel's current room (it is a recorded holder, voice-control-plane.md §4); otherwise 409 `voice_no_session`. Presence is not used for this check because it lags a join by up to a second.
- A report that does not change the state (a second `started`) succeeds and writes nothing.
- Reports are bounded per principal. Past the bound the answer is 429, audited once per window, so the endpoint cannot be used to flood the audit log.

**Audit.**

| Event | Written by | When |
|---|---|---|
| `voice_transmit_started`, `voice_transmit_stopped` with `source=reported` | the endpoint | each reported change of state |
| `voice_transmit_unreported` with `source=observed` | the poller | it sees a microphone unmuted and no `started` report arrives within 2 s of first seeing it |
| `voice_transmit_stopped` with `source=observed` | the poller | an unreported transmission ends; or a reported one is still open when the participant leaves or the room is rotated (`reason=left`) |

- **This replaces V3's rows.** In V3 the poller wrote a `started` and a `stopped` row for every transmission it saw. From V4 it writes nothing for a transmission that was reported, so there is one pair of rows per press, with exact times, and an `unreported` row marks exactly the transmissions the client did not account for. A participant that never reports (the headless participant in `e2e/voice`, a modified client) shows up as `unreported` every time.
- **What is still unrecorded:** a burst shorter than the poll interval from a client altered not to report it. That was the stated limit of decision 1 and it has not changed.
- **A report with no audio** (a client that reports `started` and sends nothing) is recorded as reported. It claims more than happened, never less, so it is left alone.
- **State is in memory.** After `conchd` restarts, a press already in progress is seen by the poller with no report and is recorded as unreported, once.

**Client side.** The report is sent when the gate opens and when it shuts, without waiting for the answer. A failed report is retried briefly and shown in the status line. **Audio does not wait for, or depend on, the report:** if `conchd` cannot be reached the transmission goes ahead, and when `conchd` is back the poller records what it sees as unreported, which is the truthful record.

## 7. Presence and the status display

Who is connected and who is talking comes from `conchd`'s presence socket (voice-control-plane.md §6), not from LiveKit, so the client shows what the server has decided and logged.

- On a terminal the client redraws a few lines: the connection state, its own state (talking, muted, deafened, no microphone), and the participants with a mark beside whoever is transmitting. Plain ANSI output; no terminal UI crate.
- With `--json` it prints one JSON object per line for every change of state, and a per-speaker count of received audio. Tests read this.
- Participants are shown as principal ids (`p7`), as `conch voice status` shows them. The API has no way for a member to turn an id into a name yet; that is a separate issue, not a V4 dependency.
- The `conch` TUI shows the same roster from the same socket, so someone reading text can see who is in voice without running the voice client.

## 8. Configuration, login and commands

**Configuration** is TOML at `$XDG_CONFIG_HOME/conch/voice.toml`, all optional: `server` (default `CONCH_SERVER`, then `http://127.0.0.1:8080`, as `conch`), a default `channel`, `[keys]` (`device`, `talk`, `mute`, `deafen`), and `[audio]` (`input`, `output`, `echo_cancellation`, `release_tail_ms`, `max_transmit_secs`). Unknown keys are an error, so a typo is not silently ignored.

**Login** stays in `conch`. `conch-voice` reads the token `conch login` stored (`credentials.json` beside its own configuration), or `CONCH_TOKEN`, and never writes that file. It applies the same rule as the Go client: a credentials file readable by other users is refused. The file's format and the way a server address is normalised into its key become a contract between two programs, so both are tested against one shared set of vectors.

**Commands.**

| Command | Does |
|---|---|
| `conch-voice join [channel]` | Joins the channel's voice and runs until `quit` or Ctrl-C. |
| `conch-voice devices` | Lists PipeWire inputs and outputs, and keyboards with whether they can be read, and prints the udev rule that would grant one. |
| `conch-voice keys <device>` | Prints the code of each key pressed on that device until Ctrl-C, to find the talk key. Says first that it will show every key pressed. |

`join` takes test options that replace hardware: a WAV file as the microphone, a sink that counts instead of playing, and standard input instead of a key device (§4).

## 9. What `dmn-audio` must gain

Today `dmn-audio` delivers 16 kHz mono in 20 ms frames, fixed by constants its consumers depend on, and its only output is a virtual microphone, not the speakers. `conch-voice` needs two additions. Both can be added beside what exists, so `daemon` itself does not change:

1. **Capture at the caller's rate.** A mono microphone stream at a rate chosen by the caller, delivered through a lock-free ring, with the existing re-attach on hot-plug. `dmn-audio` already captures at the graph rate internally for its pass-through; this makes that a public source.
2. **Playback.** A stream to the default (or a named) sink, fed from a ring, where an underrun is silence. It is the virtual microphone's stream with different node properties.

**The repository is private, and this one is public.** ADR-006 makes `dmn-audio` a git dependency pinned to a commit. `njdaniel/daemon` is a private repository with no licence file (its manifests say MIT); Conch is public under AGPL-3.0. As things stand, CI could fetch the dependency only with a stored credential, and nobody but Nick could build `conch-voice` from a public checkout. One of these has to change before the device slice, and it is Nick's choice (§12):

- **Extract `dmn-audio` into its own public repository** (recommended). ADR-006 already names this as the step to take if the coupling becomes a burden. `daemon` and Conch both depend on it by commit, and the two additions above are made there.
- **Make `daemon` public.** No code moves, but it publishes everything else in that repository.
- **Copy the PipeWire code into `voice/`** as a crate of this workspace, with attribution. Simplest to build, but the two copies drift, and it reverses ADR-006's decision.

Until that is settled V4 is not blocked: microphone and speakers sit behind two small traits in `conch-voice-audio`, the WAV and counting implementations come first, and everything up to and including the headless exit test runs without PipeWire. Only the slice that talks to real devices waits. If it is delayed further, the fallback is the spike's proven route, piping `pw-record` and `pw-play`, behind the same traits.

## 10. Testing and exit

Tests never need a microphone, PipeWire, a keyboard or LiveKit unless they are marked ignored and say what they need.

- **Unit tests** cover the gate, the jitter buffer and mixer, key decoding from recorded bytes, the push-to-talk state machine including every reason the gate shuts, the connection policy for every row of the table in §5, configuration, the login file, and the wire types against the golden fixtures.
- **The exit test** extends `e2e/voice`, which already runs a real `conchd` and a real LiveKit in Docker. Three headless `conch-voice` processes join one channel, each with a different tone as its microphone, driven through standard input. It asserts:
  - while one holds the key, the other two receive that speaker's audio, at that speaker's tone, and the speaker receives nothing of their own;
  - while nobody holds a key, nobody receives audio, although every "microphone" is playing the whole time: the gate, not silence, is what is tested;
  - each press and release is in the audit log as a reported pair, and the non-reporting headless participant's transmission is `voice_transmit_unreported`;
  - when a member is removed, the room is rotated and the remaining clients are talking in the new room without being restarted, and the removed one stops with the right message;
  - no token, room name or login appears in any client's output.
- **By hand, with Nick:** three people on Linux talking in one channel with hold-to-talk. This is V4's exit in ROADMAP.md, and it cannot be automated.

## 11. Decisions

Nick delegated design questions to the principal engineer on 2026-10-09 ("use your best judgment"). These are recorded with their reasons and he can reopen any of them.

1. **Direct dependencies are gated by a script, and `cargo-deny` gates sources, advisories and licences.** ADR-006 says "a Rust allowlist enforced by `cargo-deny`, mirroring `deps-allowlist.txt` and `scripts/depgate.sh`". `cargo-deny`'s allow-list applies to every crate in the graph, about 316 here, so it would turn each routine update into a sign-off and bury the dozen choices that matter. The Go gate lists direct dependencies only, and this mirrors it. Whether `cargo-deny` can express a direct-only list is to be confirmed when the workspace lands (§13); if it can, the script goes.
2. **Four crates, three of them free of native code.** Most of the logic is then testable anywhere and quickly, and `unsafe` is forbidden in all of our own code.
3. **The SDK's own resume is allowed** (§5).
4. **The key is read from the event device with plain file I/O**, with no input crate (§4). The format is a fixed 24-byte record, and reading it needs no `unsafe`.
5. **Audio never waits for a transmit report** (§6). A client that cannot reach `conchd` can still be heard by people already in the room, and the server's own observation is the record of it.
6. **Reported transmissions replace the poller's rows instead of adding to them** (§6). One pair of rows per press is what an audit reader needs; two pairs with slightly different times would have to be reconciled by hand.
7. **Reports carry no time.** The server's clock is the only one in the audit log.
8. **Echo cancellation is off by default** until it has been measured by ear (§3).
9. **Login stays in `conch`**; the voice client only reads it (§8).
10. **`dmn-audio` gains new entry points and keeps its existing ones** (§9), so the two projects do not have to change together.

## 12. Needs Nick's sign-off

These are his under ADR-000 and are not settled by this note.

1. **The Rust dependency list.** Direct dependencies proposed: `livekit` (exact version), `tokio`, `serde`, `serde_json`, an HTTP client and a WebSocket client (the ones already in the tree through `livekit`), `toml`, `clap`, `thiserror`, `tracing`, `tracing-subscriber`, `directories`, `hound`, and `dmn-audio` by git commit (see item 3); `tempfile` for tests. The exact list arrives with the workspace PR, which is his to merge.
2. **ADR-004 says the V0 manual checks "must be closed before V4".** They are still open (PR #88). Two of the three (two machines on a LAN, echo cancellation by ear) are better done with the real client than with spike code, and are part of V4's exit anyway. The first, hold-to-talk through the event device on COSMIC, is the one that could change §4, and takes five minutes with the spike. Proposed: Nick runs the first now, and the ADR's sentence becomes "before V4 exits" for the other two.
3. **Where `dmn-audio` comes from.** `njdaniel/daemon` is private and Conch is public, so the git dependency ADR-006 decided cannot be built by CI or by anyone else as things stand (§9). Recommended: extract `dmn-audio` into its own public repository, with a licence file. Whichever option he picks, ADR-006's dependency line is amended to match. The two additions `dmn-audio` needs are then made wherever it lives, under that repository's own process; this project can write the issue.
4. **CLAUDE.md gains Rust rules** beside rule 6, and a worker definition for `voice/` is added (ADR-006 foresaw both). They arrive with the workspace PR.

## 13. Relied on, and not yet measured

| # | Assumption | Basis | Verified by |
|---|---|---|---|
| 1 | The SDK can publish a track muted, and unmute and mute it fast enough for push-to-talk | Mute and unmute were each visible to the server in about 25 ms (voice-control-plane.md §10, finding 1), using the spike's client | #178 |
| 2 | The SDK reports why it disconnected, distinguishing a deleted room, a duplicate identity and a lost connection | The SDK's API; not exercised | #178 |
| 3 | The SDK's internal resume cannot be switched off, and gives up within a bounded time | Reading the SDK; not exercised | #178 |
| 4 | One process can hold the connection, receive several speakers and publish at once at the measured cost | The spike measured listening in four rooms; publishing while listening was not measured | #178 |
| 5 | `webrtc-sys` verifies the libwebrtc it downloads | Unknown | #177 |
| 6 | `cargo-deny` cannot express a direct-only allow-list | Its documentation, from memory | #177 |
| 7 | `dmn-audio` builds as a dependency of another workspace, with the system's clang 18 generating its PipeWire bindings while the SDK is compiled with clang 22 | Not tried | #184 |
| 8 | The event device delivers press and release for the chosen key on COSMIC | The spike's code was written and never run (V0 check 1) | Nick, §12 item 2 |
| 9 | Echo cancellation is good enough for open speakers | Not measured | Nick, at the exit session |

Findings are written back into this table as they are made, as voice-control-plane.md §10 did.

## 14. Out of scope

Nets and whispers in voice; per-speaker volume, stereo placement and ducking (V5). Behaviour under packet loss, noise suppression, a setup doctor, packaging (V6). Agents in voice, recording, video and screen share. Any platform other than Linux with PipeWire. Showing names instead of principal ids.
