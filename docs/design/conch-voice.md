# Design: the `conch-voice` client

- **Status:** Adopted for V4 implementation, 2026-10-09; corrected 2026-10-10 from measurements of the SDK (#178, §13) (governing decisions: [ADR-004](../adr/ADR-004-voice-via-livekit.md) and [ADR-006](../adr/ADR-006-rust-voice-client.md), both Accepted). §12 lists what still needs Nick's sign-off; §13 lists what is assumed and not yet measured.
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
- The SDK links a prebuilt libwebrtc from LiveKit's GitHub releases. That is a binary this project does not build. Left alone, the SDK's build script downloads it and checks nothing about it (§13, row 5). So the same toolchain script fetches that file, checks it against a pinned SHA-256, and the build is pointed at the verified copy; the check fails if the locked SDK ever expects a different build than the one pinned.
- System packages: GLib's development headers (`libglib2.0-dev`, found through `pkg-config`), which the SDK's build reads. From the slice that adds real devices (§9), also `libpipewire-0.3-dev` and `libclang-dev`.

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

- **Threads.** PipeWire's callbacks run on `dmn-audio`'s loop thread and only move samples to and from lock-free rings; they allocate nothing, lock nothing and log nothing (`daemon`'s rule, kept here).
- **The rings set the pace, not a timer.** The sending side forwards a frame whenever the capture ring holds a whole one. The playing side mixes a frame whenever the playback ring falls below its target fill. So the sound card's clock drives both directions and there is no second clock on this machine to drift against it. A remote speaker's clock does differ from the local one by a few parts per million; that speaker's jitter buffer absorbs it, at the cost of dropping or padding 10 ms once in a long while.
- **Receiving.** Each remote microphone track feeds its own jitter buffer, which aims to hold 40 ms and never more than 200 ms: when it is over, the oldest audio is dropped, so a speaker is never heard late. The mixer adds the speakers' frames and clips the sum to the valid range. Per-speaker volume, stereo placement and ducking are V5.
- **Devices.** The microphone and the speakers are PipeWire's defaults unless named in the configuration. When a device disappears the client keeps running: capture re-attaches through `dmn-audio`'s `Reattach`, and playback follows the default sink. While there is no microphone the gate stays shut and the status line says so.
- **Echo.** V4 assumes headphones. The SDK's audio processing can be switched on in the configuration (`echo_cancellation`), with the playback mix as its reference. How well it removes echo from open speakers has not been measured (ADR-004), so it is off by default and the README says to wear headphones. When it is on, captured frames pass through it *before* the gate, so the gate is still the last thing between the microphone and the track. This relies on the module only transforming the frames it is given, opening no device and sending nothing itself (§13); if that does not hold, the option is not shipped.

### The transmit gate

This is the property a user relies on: **microphone audio leaves the machine only while the talk key is held.**

- The gate is in `conch-voice-audio` and is the last step before the track. While it is shut, captured frames are dropped and nothing is handed to the track's audio source. Muting the LiveKit track is a second layer and is what `conchd` observes; it is not the guarantee. Measured (§13, row 12): with nothing handed over while the gate is shut, the client sends about 140 to 550 bytes a second of silence and nothing else, whatever the track's state.
- The client never lets the SDK open the microphone. It captures through PipeWire itself and feeds the SDK frame by frame, so there is no SDK capture path that could go round the gate.
- If the session's grant allows publishing, the track is published once on joining and muted **straight after** it is published. A press opens the gate and unmutes the track; a release shuts the gate and mutes it. Neither waits for the other, or for the report in §6. Publishing on each press was rejected in voice-control-plane.md §6: it clips the first word.
- **The order matters, and so does the SDK's own reconnect** (§13, row 12). `publish_track` ends by enabling the track whatever its mute state. A track muted *before* it is published, or republished by the SDK after a long interruption (§5), therefore sends to the server everything handed to it, and only the server withholds it from listeners. So the client mutes after publishing, and disables the track again whenever the SDK reports that it republished it. Neither is the guarantee (the gate is), but the second layer should be real.
- A press is heard quickly (§13, row 1): the first loud frame reaches a listener 14 to 80 ms after the unmute, no slower than the path itself. About 10 ms is lost at each end of a press; the release tail covers the end.
- The gate also shuts, whatever the key is doing, when: the user has muted or deafened; the connection is not established; the session's grant has `can_publish: false`; the microphone is missing; the key device has gone away; or a single press has lasted longer than `max_transmit_secs` (default 120), which catches a stuck key or something resting on the keyboard. A shut gate reopens only on a new press.
- The microphone stream stays open while joined, so the desktop's "microphone in use" indicator is on the whole time, although nothing is sent until a press. Opening it on each press would lose the start of every transmission. A client whose grant is listen-only (`can_publish: false`) publishes no track and does not open the microphone at all.
- `release_tail_ms` (default 100) keeps the gate open briefly after a release so the last syllable is not cut. It counts as part of the transmission: the release is reported when the gate shuts.

## 4. Keys

A terminal cannot see a key being released, and COSMIC offers no global-shortcuts portal (V0 spike), so the client reads the keyboard's event device directly.

- It opens the configured device read-only and reads fixed-size `input_event` records with ordinary file I/O: no crate, no `ioctl`, no `unsafe`. The record is 24 bytes on 64-bit Linux, the only target (ADR-006; the build refuses any other pointer width). Only key events with a configured code are acted on (press, release; auto-repeat is ignored). Every other event is discarded as it is read. Nothing about keys is stored, logged or sent.
- The device is named by its stable path under `/dev/input/by-id/`, because `eventN` numbers change between boots.
- **What this access exposes, stated plainly:** read access to a keyboard's event device lets a process see every key pressed on that keyboard, in every application, passwords included. That `conch-voice` looks only at the talk key is a property of its code, not of the permission, and any other process running as the same user gets the same access. Setup therefore grants access to one device, not to the `input` group, and the README says what is being granted before it says how.
- Granting it is a one-line udev rule for that device, shown by `conch-voice devices`. A per-boot `setfacl` works for a trial.
- If the device disappears (unplugged, permission withdrawn), the gate shuts as for a release and the client retries opening it.
- Mute and deafen can be bound to keys on the same device. Deafen silences playback and also blocks transmitting, as in most voice tools.
- **Without a key device** the client still runs. It reads commands from standard input, one per line: `down`, `up`, `mute`, `deafen`, `quit`. That is how the tests drive it, and it is a usable fallback (a compositor shortcut bound to a script cannot hold a key, but a foot pedal or a wrapper can write lines).
- A desktop that gains a global-shortcuts portal later is another source of the same press and release events; nothing else changes.

## 5. Sessions and connection

The client follows voice-control-plane.md §4: it asks `conchd` for a session immediately before every connection attempt, and joins the room of the grant that has no `audience`.

- **It never stores a join token, and never prints one.** Tokens, room names and the login token do not appear in any output, log line or error, in either the terminal or `--json` mode. That includes what the SDK logs, which was measured at every level (§13, row 10). At trace level the WebSocket library logs the whole upgrade request, bearer token included; at info and debug the SDK logs the room name. Only its warnings and errors are free of both, so those are the only levels of SDK output the client passes through, whatever log level the user asks for. The SDK and everything it links use the `log` crate, so one filter by target covers it.
- **Native code writes to standard output.** On a machine with an NVIDIA GPU, libwebrtc prints a line of its own to standard output in every process, outside any logging. So a reader of `--json` output skips lines that are not JSON objects, and the client's own JSON lines each begin with `{`.
- **Disconnects.** What it does next depends on why:

| What happened | Next |
|---|---|
| The room was deleted (a rotation, voice-control-plane.md §5) | Ask for a new session at once and join the channel's new room. If the user is still entitled this is a gap of about a second; if not, the request is refused and the client stops. |
| The same identity joined from elsewhere | Stop, saying so. Reconnecting would only displace the other device in turn. |
| `conchd` refuses the session: not signed in (401), not a member or no such channel (404), not a person (403), voice needs auth (400), voice not configured (503 `voice_not_configured`) | Stop with one line naming the reason and a nonzero exit. |
| `conchd` or LiveKit unreachable, `voice_unavailable`, the connection lost, the server shutting down | Retry: wait 1 s, doubling to 30 s, with jitter; the wait resets after 30 s connected. |

How each arrives was measured (§13, row 2), and they can be told apart:

| Cause | What the SDK reports | When |
|---|---|---|
| Room deleted | disconnected, `ROOM_DELETED` | about 2 ms |
| Removed by name | disconnected, `PARTICIPANT_REMOVED` | about 2 ms |
| Same identity joins elsewhere | disconnected, `DUPLICATE_IDENTITY` | about 0.5 s after the other device connects |
| LiveKit shuts down cleanly | disconnected, `SERVER_SHUTDOWN`; the SDK does not retry | at once |
| Connection lost, server gone | `Reconnecting`, then disconnected with no reason given | `Reconnecting` within 10 s; gives up 8 to 31 s later |
| Server unresponsive | `Reconnecting`; if it never answers, disconnected with no reason given | `Reconnecting` after 10 to 15 s; gives up about 100 s later |

A participant removed by name is treated like a deleted room: ask `conchd`, which either issues a session or says why not.

- **A press during a gap the client knows about does nothing** (§3) and the status line says "not connected". The client treats the SDK's `Reconnecting` as not connected from that moment until `Reconnected`. A key held when the connection drops ends that transmission; it does not resume by itself when the connection returns.
- **The client cannot know about the first 10 to 15 seconds of a silent outage** (§13, row 3). The SDK reports nothing for that long, and nothing at all for an interruption of 10 s or less: audio simply stops and comes back. A press in that window is taken, its report is sent, and its audio goes nowhere. The status display cannot say otherwise; the README says so.
- **The SDK's own reconnect is allowed, for 20 seconds** (decision 3). voice-control-plane.md §4 says the client must never reconnect with a token LiveKit sent it. The SDK reconnects by itself, using LiveKit's refreshed token for the same room, and no option switches that off (§13, row 3: an interruption under about 15 s is resumed with nothing changed; a longer one is a full reconnect that republishes the track; it tries ten times and can take 100 s to give up). This is acceptable because nothing in the server's enforcement depends on the client: a room whose holders are all entitled is exactly a room that may be rejoined, and a room that must be abandoned is deleted, which no token survives. What the rule protects is correctness after a rotation, and that is met: once the SDK gives up, the client always goes back to `conchd`.
  The client does not wait the full 100 s. If `Reconnecting` has not become `Reconnected` within 20 s, it closes the connection itself (measured: that takes 5 s) and asks `conchd` for a session, so that what happens next is the server's decision again. When the SDK does reconnect in full, the client disables the republished track (§3).

## 6. Transmit reports

voice-control-plane.md §9 decision 1 left this to V4, and #135 recorded it: the poller can miss a press shorter than its interval, so the client reports each press and release, and the poller checks the client.

**Endpoint.** `POST /v1/channels/{channel}/voice/transmit` with a `VoiceTransmitReportV1` body: a `state` of `started` or `stopped`, and an optional `audience` (absent means the whole channel; V5 uses it for nets). It is a new type, added through the `schema-change` skill as V3's voice types were (#179); nothing published changes, so there is no schema version to bump. A successful report is answered 204 with no body.

- The report carries no time. `conchd` stamps the moment it receives it, so a client cannot back-date or post-date its own record.
- The caller is checked as for a session: a verified, human member of the channel. On top of that, the credential must have been issued a session for the channel's current room (it is a recorded holder, voice-control-plane.md §4); otherwise 409 `voice_no_session`. Presence is not used for this check because it lags a join by up to a second.
- A report that does not change the state (a second `started`) succeeds and writes nothing.
- Reports are bounded per principal. Past the bound the answer is 429, audited once per window, so the endpoint cannot be used to flood the audit log.

**What the check can and cannot do.** The poller samples: it sees each microphone about twice a second and nothing in between. No rule built on samples can catch a client that misreports only between two of them. So the aim is stated as four things, in this order:

1. A transmission that is not reported at all is recorded, at the resolution V3 had.
2. Reports cannot be used to hide a transmission: a client whose reports are not true to within about one poll interval is recorded as unreported.
3. An honest client is not recorded as unreported, except when its report really was late.
4. Every row that opens a transmission is followed by a row that closes it.

**The rule.** For each participant in a room `conchd` holds what the client last *reported* (`started` or `stopped`; `stopped` until told otherwise) and what the poller *observes* on each pass (microphone unmuted or not). While they agree, the reports are the record and the poller writes nothing.

- **Passes are not evenly spaced.** The gap between passes is drawn at random between 350 and 650 ms, so a client cannot place a report around a pass that has not happened yet.
- **An unaccounted pass** sees the microphone unmuted while the reported state is `stopped` at that instant.
- **Two unaccounted passes in a row open an unreported transmission**, timed at the first of them. No report excuses that.
- **One unaccounted pass alone is also an unreported transmission**, one that began and ended, unless it is excused. A report and a mute travel separately, so an honest client can cross a pass at the edge of a press: a release caught between its `stopped` report and its mute, or a press between its unmute and its `started` report. A lone unaccounted pass is excused when a report was received after the pass before it and before the pass after it, and either one of those two passes saw the microphone not transmitting (it is at the edge of a transmission), or no other pass was excused this second way in the previous 3 s (an honest quick re-press can land on a pass; it cannot keep doing so). Judging a lone pass therefore waits for the next one.
- **An unreported transmission is closed** by the first later pass that sees the microphone muted, the participant gone, or the reported state `started`. Closed with `reason=reported`, it means the client reported late, not that it never reported; from there its reports are the record again.
- **Not transmitting on every pass for 2 s while the reported state is `started`**: the stop report was lost or never sent, or the report was false. The poller closes the transmission and sets the reported state to `stopped`. This is also what happens to a holder who reports `started` without ever having been seen in the room: it counts as not transmitting, and its report marks the room as in use so that the poller looks at it.
- **When a participant the poller had seen is gone** (left, removed), or **the room is rotated**, every transmission still open for it, reported or unreported, is closed at once, before its state is dropped.
- **When more than one reason to close applies on a pass**, one row is written, with the first of: `left`, `muted`, `reported`.

**Audit.**

| Event | Written by | When |
|---|---|---|
| `voice_transmit_started`, `voice_transmit_stopped` with `source=reported` | the endpoint | each reported change of state |
| `voice_transmit_unreported` with `source=observed` | the poller | an unreported transmission opens. Timed at its first unaccounted pass. |
| `voice_transmit_stopped` with `source=observed` | the poller | an unreported transmission closes: `reason=muted`, `reason=left`, or `reason=reported` when the client caught up |
| `voice_transmit_stopped` with `source=observed` and `reason=no_stop_report` | the poller | not transmitting for 2 s while reported `started` |
| `voice_transmit_stopped` with `source=observed` and `reason=left` | the poller | the reported state is `started` when the participant leaves or is removed, or the room is rotated |

How the four aims are met, and where they are not:

- **A press shorter than the gap between passes.** The poller may never see it. The reported pair is the record. This is the case polling alone missed.
- **A participant that never reports** (the headless participant in `e2e/voice`, a modified client). Every transmission the poller sees, even on a single pass, is recorded as unreported.
- **A false `stopped`, with the transmission continuing.** The second pass after it opens an unreported transmission, timed at the first.
- **Many short report pairs during one long transmission.** The reported state at a randomly timed pass is almost always `stopped`, so an unreported transmission opens and stays open. To have every pass find `started`, the client would have to report `started` across the whole window in which the next pass can fall, which is most of the time: its reports are then roughly true.
- **A `stopped` report that never arrives; a `started` with no audio; a session holder that reports and never connects.** All closed by the poller after 2 s.
- **A `started` report that arrives late**, after an unreported transmission was opened. The report is written; the next pass closes the unreported transmission with `reason=reported`; the press ends with its reported `stopped`.
- **`conchd` restarts during a press.** Reported state is in memory and is lost, so it reads `stopped`. The poller opens an unreported transmission and closes it when the microphone is muted; the client's `stopped` report changes nothing. The `started` row written before the restart is the one case of a row with no closing row of its own (aim 4); the restart is itself in the audit log, between them.
- **Two devices for one principal.** There is one connection per principal per room (voice-control-plane.md §9, decision 3) and reported state is per principal, so a last report from a displaced device can change the state the new device set. A false `started` is closed after 2 s; a false `stopped` shows the new device's press as unreported until its next report. Rare, and it errs toward recording more.
- **A report delivered late and out of order.** The honest client sends one report at a time (below), so this needs a request that outlives its own timeout and is delivered after a later report, or after a rotation once the client holds a session for the new room (a report names an audience, not a room). It can then write one spurious row and flip the state, with the same two outcomes as the previous case. It cannot hide a transmission. Numbering reports so that a stale one is discarded was considered and left out: every scheme tried either broke across a reconnect or needed the session response to change, for a case this rare. It can be added in a later version of the report.
- **An honest client can still be recorded as unreported in one more case** (aim 3): two quick re-presses within 3 s that each land on a pass. The second is recorded as a one-pass unreported transmission. It needs a release and a press inside one gap between passes, twice, each time straddling a pass by milliseconds.
- **Still unrecorded:** a burst shorter than the gap between passes from a client that does not report it (the stated limit of voice-control-plane.md §9 decision 1, unchanged), and misreporting within about one pass of the true edges of a transmission.
- **A slow link produces late reports, and they are recorded as late.** Reports go one at a time (below), so on a link where a round trip to `conchd` takes longer than the gaps between presses they queue up, and a press can be seen by the poller before its report arrives. That press gets an unreported row closed with `reason=reported`, and quick presses are recorded further apart than they were. This is expected to be rare: `conchd` and its users are normally on one network or a VPN, where a round trip takes milliseconds.

What follows:

- **It replaces V3's rows.** In V3 the poller wrote a `started` and a `stopped` row for every transmission it saw. From V4 it writes nothing while the reports and what it sees agree, so an honest client's press is one pair of rows.
- **Times.** A reported row carries the moment `conchd` received the report, which is the press or release plus the network delay, by the server's clock.
- **State is in memory**, per room and principal, and is dropped with the room.
- **This rule was attacked on paper four times before any code existed** (the review record is on PR #190), and each round changed it. The implementation issue (#135) therefore carries each case above as a test with the poller's passes driven by the test, a check on every scenario that opening and closing rows pair up, and a security review.

**Client side.** A report is queued when the gate opens and when it shuts. The client sends its reports one at a time, in order, each after the answer to the one before, because the server takes them in arrival order: two requests in flight at once could arrive reversed and leave the state wrong. A failed report is retried briefly, in place, and shown in the status line; when the client gives up on one it drops that connection, so the request cannot be delivered later. Two answers are not retried: 409 `voice_no_session` means the room was rotated or the session is otherwise gone, and the connection policy (§5) is already taking the client back to `conchd`; 429 means the bound was hit, and the status line says so. **Audio does not wait for, or depend on, the report:** if `conchd` cannot be reached the transmission goes ahead, and when `conchd` is back the poller records what it sees as unreported, which is the truthful record.

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

- **Unit tests** cover the gate, the jitter buffer and mixer, key decoding from hand-built bytes (never a recording of someone's keyboard), the push-to-talk state machine including every reason the gate shuts, the connection policy for every row of the table in §5, configuration, the login file, and the wire types against the golden fixtures.
- **The exit test** extends `e2e/voice`, which already runs a real `conchd` and a real LiveKit in Docker. Three headless `conch-voice` processes join one channel, each with a different tone as its microphone, driven through standard input. It asserts:
  - while one holds the key, the other two receive that speaker's audio, at that speaker's tone, and the speaker receives nothing of their own;
  - while nobody holds a key, nobody receives audio, although every "microphone" is playing the whole time: the gate, not silence, is what is tested;
  - each press and release is in the audit log as a reported pair, and the non-reporting headless participant's transmission is `voice_transmit_unreported`; a client made to report `stopped` while still sending is flagged too;
  - when a member is removed, the room is rotated; the remaining clients join the new room without being restarted and a new press there is heard (a press held across the rotation ends with it, §5); the removed one stops with the right message;
  - no token, room name or login appears in any client's output.
- **By hand, with Nick:** three people on Linux talking in one channel with hold-to-talk. This is V4's exit in ROADMAP.md, and it cannot be automated.

## 11. Decisions

Nick delegated design questions to the principal engineer on 2026-10-09 ("use your best judgment"). These are recorded with their reasons and he can reopen any of them.

1. **Direct dependencies are gated by a script, and `cargo-deny` gates sources, advisories and licences.** ADR-006 says "a Rust allowlist enforced by `cargo-deny`, mirroring `deps-allowlist.txt` and `scripts/depgate.sh`". `cargo-deny`'s allow-list applies to every crate in the graph, about 316 here, so it would turn each routine update into a sign-off and bury the dozen choices that matter. The Go gate lists direct dependencies only, and this mirrors it. `cargo-deny`'s documentation confirms it has no direct-only list. Because this departs from the ADR's wording it is also listed in §12.
2. **Four crates, three of them free of native code.** Most of the logic is then testable anywhere and quickly, and `unsafe` is forbidden in all of our own code.
3. **The SDK's own reconnect is allowed, for 20 seconds** (§5). It cannot be switched off, and it is harmless to the server's enforcement; but an unresponsive server would otherwise keep the client retrying on its own for 100 s, so the client cuts it short and goes back to `conchd`.
4. **The key is read from the event device with plain file I/O**, with no input crate (§4). The format is a fixed 24-byte record, and reading it needs no `unsafe`.
5. **Audio never waits for a transmit report** (§6). A client that cannot reach `conchd` can still be heard by people already in the room, and the server's own observation is the record of it.
6. **Reported transmissions replace the poller's rows instead of adding to them** (§6). One pair of rows per press is what an audit reader needs; two pairs with slightly different times would have to be reconciled by hand.
7. **Reports carry no time.** The server's clock is the only one in the audit log.
8. **Echo cancellation is off by default** until it has been measured by ear (§3).
9. **Login stays in `conch`**; the voice client only reads it (§8).
10. **`dmn-audio` gains new entry points and keeps its existing ones** (§9), so the two projects do not have to change together.

## 12. Needs Nick's sign-off

These are his under ADR-000 and are not settled by this note.

1. **The Rust dependency list, and the pins.** Direct dependencies proposed: `livekit` (exact version), `tokio`, `serde`, `serde_json`, `reqwest` and `tokio-tungstenite` with `futures-util` (the HTTP and WebSocket clients already in the tree through `livekit`), `thiserror`, `clap`, `log`, `toml`, and `tempfile` for tests; later `dmn-audio` (see item 3). Also his: the versions and hashes of the compiler and of libwebrtc in `scripts/voice-pins.sh`, which decide what is compiled into the client. Both arrive with the workspace PR (#193), which is his to merge.
2. **ADR-004 says the V0 manual checks "must be closed before V4".** They are still open (PR #88). Two of the three (two machines on a LAN, echo cancellation by ear) are better done with the real client than with spike code, and are part of V4's exit anyway. The first, hold-to-talk through the event device on COSMIC, is the one that could change §4, and takes five minutes with the spike. Proposed: Nick runs the first now, and the ADR's sentence becomes "before V4 exits" for the other two.
3. **Where `dmn-audio` comes from.** `njdaniel/daemon` is private and Conch is public, so the git dependency ADR-006 decided cannot be built by CI or by anyone else as things stand (§9). Recommended: extract `dmn-audio` into its own public repository, with a licence file. Whichever option he picks, ADR-006's dependency line is amended to match. The two additions `dmn-audio` needs are then made wherever it lives, under that repository's own process; this project can write the issue.
4. **CLAUDE.md gains Rust rules** beside rule 6, and a worker definition for `voice/` is added (ADR-006 foresaw both). They arrive with the workspace PR.
5. **How the dependency list is enforced.** ADR-006 says the list is "enforced by `cargo-deny`". Decision 1 enforces the list of direct dependencies with a script and uses `cargo-deny` for sources, advisories and licences, because `cargo-deny`'s allow-list covers every crate in the graph (confirmed from its documentation: "any crate not in that list will be denied", with no direct-only option). That is a different mechanism from the ADR's words, so it is his to accept, with a one-line amendment to ADR-006 if he does.

## 13. Relied on, and not yet measured

| # | Assumption | Basis | Verified by |
|---|---|---|---|
| 1 | The SDK can publish a track muted, and unmute and mute it fast enough for push-to-talk | **Holds.** `unmute` and `mute` return in under 1 ms. The first loud frame reaches a listener 14 to 43 ms after the unmute on a session's first press and 29 to 80 ms on later ones (120 presses), no slower than the path itself (32 to 87 ms). The server's `ListParticipants` shows an unmute within 10 ms and a mute within 7 ms. About 10 ms of audio is lost at each end of a press. A track muted straight after publishing was never reported unmuted before the first press (2,410 answers). | done (#178) |
| 2 | The SDK reports why it disconnected, distinguishing a deleted room, a duplicate identity and a lost connection | **Holds.** `ROOM_DELETED` and `PARTICIPANT_REMOVED` within about 2 ms; `DUPLICATE_IDENTITY` about 0.5 s after the other device connects; `SERVER_SHUTDOWN` at once on a clean shutdown, with no retry by the SDK. A lost connection is `Reconnecting` followed, if it fails, by a disconnect with no reason given. The table is in §5. A join with a token for a room that does not exist fails after about 0.6 s with an error that holds no token. | done (#178) |
| 3 | The SDK's internal resume cannot be switched off, and gives up within a bounded time | **Holds, with two things this note had not said.** No option switches it off (ten attempts, 300 ms doubling to 7 s, fixed in the SDK). It gives up 8 to 31 s after `Reconnecting` when the server is gone and about 100 s after when the server is unresponsive; `Room::close` ends it in 5 s. First: for an interruption of 10 s or less the application is told nothing, and `Reconnecting` comes only 10 to 15 s into a silent one. Second: past about 15 s it is not a resume but a full reconnect, which republishes the track and resubscribes to everyone. It uses LiveKit's own token: a reconnect after 60 s succeeded with a join token long expired, which was refused when presented afterwards. §5 is corrected for both. | done (#178) |
| 4 | One process can hold the connection, receive several speakers and publish at once at the measured cost | **Holds.** Publishing while receiving three speakers for a minute: 2.5% of one core, 120 MB resident and flat, 28 threads, every frame received and sent (release build). | done (#178) |
| 5 | `webrtc-sys` verifies the libwebrtc it downloads | **Does not hold.** `webrtc-sys-build` 0.3.19 fetches `webrtc-linux-x64-release.zip` from the release tagged `webrtc-89d790b` over HTTPS and unpacks it with no checksum. It also honours `LK_CUSTOM_WEBRTC`, a directory to use instead. The workspace therefore fetches the file itself, verifies a pinned SHA-256, and sets that variable (§2). Checked 2026-10-10 by reading the crate's source; a build from an empty target directory then downloaded nothing. | done (#177) |
| 6 | `cargo-deny` cannot express a direct-only allow-list | **Holds**, from its documentation (`[bans] allow`: "any crate not in that list will be denied"; `allow-workspace` and `wrappers` do something else). Checked 2026-10-09 against 0.20.2. | done (#177) |
| 7 | `dmn-audio` builds as a dependency of another workspace, with the system's clang 18 generating its PipeWire bindings while the SDK is compiled with clang 22 | Not tried | #184 |
| 8 | The event device delivers press and release for the chosen key on COSMIC | The spike's code was written and never run (V0 check 1) | Nick, §12 item 2 |
| 9 | Echo cancellation is good enough for open speakers | Not measured | Nick, at the exit session |
| 10 | The SDK's log output never contains a join token (for example inside a WebSocket address) | **Does not hold at trace level; holds at error, warn, info and debug.** At trace, `tungstenite`'s client handshake logs the whole upgrade request, which carries the token as a bearer header: once per join, eight times for a failed join, and LiveKit's own token on a reconnect. The room name is logged at info (on a disconnect) and at debug (the join response). Only error and warn are free of both. The SDK and all it links use the `log` crate. Separately, libwebrtc prints a line to standard output on a machine with an NVIDIA GPU, outside any logging. §5 is corrected. | done (#178) |
| 11 | The SDK's audio processing module only transforms frames it is given: it opens no audio device and sends nothing | The spike fed it frames by hand; not checked that it does nothing else | #186 |
| 12 | With the track muted, nothing handed to the SDK's audio source reaches a listener | **Holds for the listener; does not hold for what leaves the machine.** No listener received a non-silent frame from a muted track in any run. But a track muted *before* it is published, or republished by the SDK's own reconnect, sends everything handed to it to the server at the full rate (12,345 bytes a second against 12,344 unmuted), and only the server withholds it. Muted *after* publishing, or disabled again after a republish, it sends silence (about 140 bytes a second, or 5,850 in the one session in six that negotiated no discontinuous transmission). With nothing handed over, as the gate ensures, 136 to 550 bytes a second leave in every case. §3 is corrected. | done (#178) |

Findings are written back into this table as they are made, as voice-control-plane.md §10 did. Rows 1 to 4, 10 and 12 were measured on 2026-10-10 against LiveKit 1.13.7 with `livekit` 0.9.4, on loopback, by `go run ./e2e/sdkprobe` (five repetitions unless a count is given; ranges are minimum to maximum). Loopback makes the latencies a floor, and a network that drops packets was not simulated.

## 14. Out of scope

Nets and whispers in voice; per-speaker volume, stereo placement and ducking (V5). Behaviour under packet loss, noise suppression, a setup doctor, packaging (V6). Agents in voice, recording, video and screen share. Any platform other than Linux with PipeWire. Showing names instead of principal ids.
