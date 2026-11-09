# Design: the screen's sound from the Linux desktop ("everything but the call")

- **Status:** **Proposed, for Nick's sign-off**, 2026-11-09, by the protocol-designer (issue [#1585](https://github.com/njdaniel/conch/issues/1585), D4-08; parent #772). It answers the Linux half of [#1552](https://github.com/njdaniel/conch/issues/1552) (Nick, 2026-11-07: a desktop share carries the screen's sound by default, scope `system_excluding_call`, where a route exists). Nothing here is implemented; no code, dependency, schema version or migration lands with this note. What needs Nick is in §15; the rest is a recommendation with its reason, made under the principal's delegation.
- **Changes to accepted text:** none made here. Where this note would change text Nick signed (model B's "share audio is the companion's", [linux-desktop.md](linux-desktop.md) §6 and §8; "main never calls the route on Linux", [voice-control-plane.md](voice-control-plane.md) §13), it says so and lists the amendment for his yes (§17). Until he signs, every accepted document stands as written and the Linux share is silent and says so (#1582, D4-05).
- **Reads:** [linux-desktop.md](linux-desktop.md) §1, §6, §8, §9, §10 (model B, the §8 safety rules, the threat table); [voice-control-plane.md](voice-control-plane.md) §12–13 (the claim, the companion, its share audio by consent); [voice-multi-share.md](voice-multi-share.md) §5.4, §7.3–7.4 (audio per sharer; the helper under B2 plays none of it); [windows-desktop.md](windows-desktop.md) §8 (the three scopes; exclusion by process tree); [conch-voice.md](conch-voice.md) §13 row 7 (why the helper owns PipeWire in its process); [tui-voice.md](tui-voice.md) and `pkg/schema/voice_control*.go` (the control channel, v1 and v2); the capability report [linux-desktop-capabilities.md](../reviews/linux-desktop-capabilities.md) rows L-S5, L-S6 and experiment LE8.
- **Current-code baseline:** `main` at `aaea4d11` (alpha.18), inspected 2026-11-09, in the worktree of this issue. Evidence markers: a claim checked in source or a document today is cited; a claim from memory says **(memory)**; a claim nobody has run says **(unverified)**.

Nick decided that a desktop share carries the screen's sound by default. On Windows the shell has a route (process loopback, [windows-desktop.md](windows-desktop.md) §8). On Linux nothing captures the screen's sound today: Electron's `loopback` is Windows-only, and the accepted design says a bare output monitor is never opened because it re-broadcasts the call. This note finds the Linux route, says who publishes the sound and how that squares with model B, and lists what only Nick can decide.

## 0. The answer in one page

| Question | Recommendation |
| --- | --- |
| The capture route | **The helper captures the other applications' playback streams on PipeWire, one capture per eligible `Stream/Output/Audio` node, linked by `target.object` to that stream and never to a sink or its monitor, and mixes them.** The eligible set is every application output stream except those of the Conch process tree (the helper's own pid, Electron main's pid and its descendants). A bare monitor is never opened (§5). |
| Who publishes | **The helper, as a `ScreenshareAudio` track on the person's own participant `p<id>`** (option A, §4, §6). The person's share grant already carries the sound (`publishPermission`: a person holding the share gets `ScreenShare`, which bundles both sources; `VoiceShareV1.audio` is true). Viewers already key share tracks by `publisherOf`, which folds `p<id>` and `p<id>.screen` into one stream (`web/src/voice/sdk.ts:814`, `presence.ts:105`). |
| Model B | **Amended, not broken.** "One microphone publisher, one voice playback path" stands. The one sentence that changes is "share audio is the companion's" (linux-desktop §6): on Linux share **playback** stays the companion's (B2, #906, #934 unchanged) and share **capture and publication** is the helper's. The renderer still holds one credential and never an audio grant: this route is *stricter* than the Windows one, where the renderer publishes the sound (§6). |
| Control channel | **`conch.voice_control.v3`**: v2 plus `set_share_audio` (desired state: capturing or not, with the scope), `set_share_audio_mute`, and three state fields (`share_audio`, `share_audio_muted`, `share_audio_problem`) and a count (`share_audio_sources`). A new version, as v2 was, so a v1/v2 owner sees nothing new (§7). **Needs Nick.** |
| Server | **The consent route records who publishes.** `VoiceCompanionAudioRequestV1` gains an optional `publisher` (`companion`, the default and today's meaning, or `person`). With `person`, conchd records the consent on the share and writes the audit rows exactly as today, and grants the companion **nothing**. Without it, following #1552's letter would hand the renderer an audio grant it must never use (§8). **Needs Nick** (a server route change on the share-audio path; additive on the wire). |
| Dependencies | **No new direct Rust dependency** if the capture lives in `dmn-audio` (Nick's crate, MIT, already a dependency; it captures an app's output stream by `target.object` today, at 16 kHz mono). It needs a capture at 48 kHz, stereo, at a chosen rate: a `dmn-audio` change and a commit bump. The alternative is `pipewire` (MIT/Apache-2.0, already transitive) as a direct dependency of `conch-voice`, which needs an allowlist line (§13). **Needs Nick** either way (a `dmn-audio` rev bump is a dependency change). |
| "This application" scope | **Not offered on Linux.** The portal picker returns a PipeWire video stream and no process id, so a picked window cannot be mapped to an audio stream. L-S5 becomes "unsupported for now", with the reason (§4, option A's limit). |
| The gate | **L-S6, by hand, with a measurement listener**: remote speech played alone must come back as digital silence from the route (§14.4). Nothing ships to a release before it passes on Nick's machine (#922, LE8). |
| #1586 | **XL as one issue; split into five** (§16): schema v3 (S), `dmn-audio` capture (M, Nick's repository), helper (L), server `publisher` (S), desktop main and web (M); the by-hand L-S6 run stays in #922. |

## 1. What is already decided, and what this note may change

| Decision | Where | Standing |
| --- | --- | --- |
| Media model B: the helper is the person's voice participant; Electron is a companion `p<id>.screen` for screen video; one playback owner per audio source | linux-desktop §6, Nick 2026-10-28 | **Accepted.** This note keeps the participants, the grants, the companion route and the playback rule. It proposes one amendment to the text "share audio is the companion's" (§6, §17). |
| The companion's grant is `["screen_share"]` only; `screen_share_audio` is added to the companion only by the consent route, removed on every end | linux-desktop §6.4; voice-control-plane §13; #898 merged | **Accepted and merged.** Unchanged. Under this note's route the companion is never granted audio on Linux at all. |
| §8 safety rules: never a bare monitor; the route excludes the helper's playback, proven by L-S6; share audio has its own mute; microphone mute never enables a captured monitor; deafen stops local playback; lock, leave and quit stop both streams | linux-desktop §8; restated in #1552 | **Kept, every one** (§10, §12). |
| A desktop share carries the screen's sound by default, scope `system_excluding_call`, where a route exists; consent recorded per share through the companion audio route; "with sound" shown; one-click "Share without sound"; the choice kept per profile | #1552, Nick 2026-11-07 | **Accepted.** This note gives Linux the route. One letter of it changes: the consent is recorded through the same route **with `publisher: person`**, so that the renderer is not granted audio (§8). Listed for Nick (§15, D-3). |
| The helper's subscription rule: `--share-audio-owner`, `companion_identity`, never its own companion's tracks, under B2 never others' `ScreenshareAudio` | #906, #934 merged; `voice/conch-voice/src/livekit.rs:89–140` | **Unchanged.** The helper publishing a track does not change what it subscribes to; it never subscribes to its own tracks in any case. |
| Several shares per room; audio per sharer; the desktop companion plays every watched stream's sound and the helper none | voice-multi-share §5.4, §7.4, Nick 2026-11-07 | **Unchanged.** |
| Q-D4-1 to Q-D4-4 (#1578): portal only; end the share on a room renewal; 720p30 and 1080p30 | Fable 2026-11-09 (Q-D4-1 asked of Nick) | **Unchanged**; §10 follows the renewal rule. |
| ADR-006: the helper never lets the SDK open a device; it captures through PipeWire and feeds frames; `#![forbid(unsafe_code)]`; one `thiserror` enum per crate | ADR-006; conch-voice.md §3 | **Kept.** The share-audio capture is a second PipeWire capture fed to a second `NativeAudioSource`, the microphone's pattern. |

## 2. The Linux facts this note rests on

1. **No Chromium route.** Electron 45.1.0's `setDisplayMediaRequestHandler` audio option "can be `loopback` or `loopbackWithMute`" and "Specifying a loopback device will capture system audio, and is currently only supported on Windows" ([session.md](https://raw.githubusercontent.com/electron/electron/v45.1.0/docs/api/session.md), fetched 2026-11-09). Chromium's `getDisplayMedia` on Linux returns no system-audio track **(memory)**; the per-window `windowAudio` hint is a draft and unmeasured in the pinned Chromium (windows-desktop §8).
2. **Electron's media permission is not per device.** `setPermissionRequestHandler` for `media` carries `details.mediaTypes` ("lists which of them was requested"); the page names no device id. `setDevicePermissionHandler`'s `deviceType` "can be `hid`, `serial`, or `usb`" (same page). So main cannot grant the renderer "this one audio source and no other"; granting `audio` grants every input, the real microphone included. This is what rules out option B (§4).
3. **The helper owns PipeWire in its process, and may not grow screen capture there** (conch-voice.md §13 row 7; `scripts/voice-pins.sh`). Audio capture through `dmn-audio` is exactly what it owns already.
4. **`dmn-audio` captures an application's output stream today.** Its `PipeWireSource` "captures a mic, one app's output stream (by `target.object`) or the default sink monitor" and "re-attaches when the app restarts" (`dmn-audio` `src/lib.rs`, rev `39e7748`); `CaptureTarget::App { application, monitor_fallback }` matches by `application.name`, and its `NodeTable` lists `Stream/Output/Audio` nodes with `application.name`, `node.name` and `object.serial` ("the preferred `target.object` value, unique for the node's lifetime", `src/pipewire.rs:86`). Its output is 16 kHz mono for speech; `MicCapture` and `Playback` take a rate of the caller's choosing, mono. A 48 kHz stereo app-stream capture does not exist yet. PipeWire's `target.object` is "where the node should link to, this can be a node.name or an object.serial" ([pipewire-props(7)](https://docs.pipewire.org/page_man_pipewire-props_7.html), fetched 2026-11-09).
5. **Process identity on PipeWire is advisory.** `application.process.id` and `application.name` are "determined by the application connecting to PipeWire", which "can set and change other properties freely, so only `pipewire.*` should be used for security"; `pipewire.sec.pid` is the "client pid, set by protocol", and for a PulseAudio-API client it is `pipewire-pulse`'s (same page). Chromium reaches PipeWire through the PulseAudio API **(memory)**, so its nodes carry `pipewire.sec.pid` of `pipewire-pulse` and `application.process.id` of Chromium's audio process. §5.2 says what that means for the exclusion rule (it is a confidentiality rule for the sharer, not an authority boundary, so advisory properties are acceptable; spoofing can only hide a stream from the share).
6. **The LiveKit Rust SDK publishes a second audio track with a source.** `TrackPublishOptions.source: TrackSource` (`livekit-0.9.4/src/room/options.rs:129`); the helper already publishes its microphone as `TrackSource::Microphone` through `LocalParticipant::publish_track` (`livekit.rs:446–463`). `TrackSource::ScreenshareAudio` is a variant the helper's subscription tests already use (`livekit.rs:751`).
7. **The person's share grant carries the sound.** `publishPermission(voiceRolePerson, …)` returns `livekit.Permission{CanPublish: true, ScreenShare: in.share, …}`, the bundled form (`internal/server/voice_permission_fn.go:70–75`); voice-control-plane §13: "The person's own claim is unchanged: `VoiceShareV1.audio` still answers true, since `p<id>`'s grant carries the sound as before." The poller counts the person's own `ScreenShareAudioPublished` as the share's sound with no consent (`voice_share.go:1293`: `audio := part.ScreenShareAudioPublished || (screen.ScreenShareAudioPublished && c.audioScope != "")`), as a browser sharer's is today.
8. **Viewers key a share's tracks by publisher.** `publisherOf` maps `p<id>` and `p<id>.screen` to the same principal and `sdk.ts` keys both share tracks by it (`sdk.ts:814`, `:1436`; the comment at `:748`: "desktop companion p<id>.screen publishes the stream (publisherOf)"). A viewer therefore plays a video track from `p<id>.screen` and an audio track from `p<id>` as one stream without a change. The TUI's helper (no companion) plays every remote audio track, `ScreenshareAudio` included (`livekit.rs:131`).
9. **The control channel has no share command.** v1 and v2 offer lifecycle, mute, deafen, state, devices and talk mode and "neither offers a press, a release or frame transport" (linux-desktop §1). A command to start and stop a capture is a new version by the rule v2 set (`voice_control_v2.go`: a v1 helper refuses a kind it does not know, which the owner treats as a broken helper).
10. **The consent route grants the companion.** `PUT …/voice/companion/audio` records `voiceShare.audioScope` and sets the companion's permission to `[screen_share, screen_share_audio]` (voice-control-plane §13; `voice_permission_fn.go:79`); the pass withdraws a companion's unconsented audio as unclaimed (`voice_share.go:1335`, `:1460`).

## 3. Threat-model framing: what the route must never do

- **Return the call.** The room's voices reach this machine twice: the helper plays remote voice, and under B2 the companion (Electron) plays the sound of every stream the person watches. Both are "the call". A capture that includes either re-broadcasts the room to the room. The §8 rule "never a bare monitor" exists because a sink's monitor includes both.
- **Give the renderer an audio capture.** The renderer is the compromised-page boundary of linux-desktop §10. It holds the companion grant only, which never names a microphone or `screen_share_audio` unless main asks for it after a consent. The Windows route (§8 there) does ask, and accepts the synthesised-speech residual. This note need not: on Linux the publisher can be the helper, and the renderer's grant stays `["screen_share"]` always.
- **Publish without the owner's word.** Authority arrives on the command channel from the spawning process only (linux-desktop §5). A capture must start on a command there, after main recorded the consent, and stop on a command or on owner loss.
- **Outlive the share.** Every end of the share (self, operator, left, access lost, rotated, idle, lock, suspend, quit, helper loss, renderer crash) must end the sound, with an audit row that says so.

## 4. The options

| Option | Who captures | Who publishes | Server change | Renderer permission | Rebroadcast risk | Verdict |
| --- | --- | --- | --- | --- | --- | --- |
| **A. The helper captures and publishes on `p<id>`** | the helper, on PipeWire, the other applications' streams | the helper, `TrackSource::ScreenshareAudio` on `p<id>` | consent recorded with `publisher: person`; no grant change (the person's grant carries the sound today, §2.7) | none added: the renderer keeps `["screen_share"]` and no microphone | excluded by construction: the helper's own stream and Electron's are not captured; no monitor | **recommended** |
| **B. The helper exposes a virtual source; the renderer captures it with `getUserMedia` and publishes** | the helper builds the mix into an `Audio/Source` node (as `dmn-audio`'s virtual microphone does) | the renderer, after `PUT …/companion/audio` as today | none | **`audio` for the main frame**, which Electron cannot limit to one device (§2.2): a compromised renderer could publish the real microphone as share audio, with no press and no gate, for the life of a share | the same as A for the mix; the virtual source is one more node any same-user app can also capture | **rejected**: it breaks ADR-007's "one owner captures a given microphone stream" at the boundary §10 defends; worse than the synthesised-audio residual |
| **C. No Linux route for now** | — | — | none | none | none | the honest fallback if A is refused; the share stays silent and says so (#1582). It leaves #1552's default unmet on the platform Nick uses |
| D. A null sink the helper creates; other applications moved to it by metadata; its monitor captured | the helper, a sink monitor | the helper | as A | none | the monitor is of a sink the call is kept off, so none in theory; **but** moving other applications' streams (`target.object` metadata) changes where the person's own applications play, and a helper crash leaves them playing into a sink nobody hears until the session manager restores them | rejected: intrusive; a bare monitor in all but name; against the §8 rule's spirit |
| E. One capture stream with explicit PipeWire links from each eligible stream's ports (the mechanism OBS's PipeWire audio capture plugin is believed to use, **memory**) | the helper, one stream, PipeWire mixes on the input ports | the helper | as A | none | as A | a refinement of A that saves one stream per application; needs link creation through the `pipewire` crate (`Core::create_object` with the link factory, **unverified** in the crate's safe API). Kept as the implementation's optimisation if LE8 shows the per-stream cost matters; not the first route |
| F. Chromium or Electron loopback | Chromium | the renderer | none | audio in the companion grant by consent (the Windows route) | endpoint loopback includes the helper's playback | not available on Linux (§2.1); and if it were, it is the bare monitor |

**Costs of A, as #1552's grooming listed them, checked:**

- *An owner-to-helper command: a new control version, needs Nick.* Confirmed (§2.9, §7).
- *conchd must record consent for sound published by `p<id>` under a companion share.* Confirmed, and smaller than feared: LiveKit needs no change (§2.7). What changes is the consent record's meaning and that the companion must **not** be granted (§8).
- *Picture and sound from two participants: no A/V sync.* Confirmed. LiveKit synchronises tracks within a participant **(memory)**; across `p<id>` and `p<id>.screen` the offset is each path's own latency. For a screen share this is a lag of the order of the two paths' jitter buffers, not lip sync on a face. Accepted and measured in LE8 (a clap on screen against its sound at the viewer). The Windows route has the same two paths under the bonnet (the helper would capture, the companion publish) and was never claimed to be in sync either.

**A's limit, stated:** the **"this application"** scope cannot be built from the portal picker's result. The ScreenCast portal hands Chromium a PipeWire video node and says nothing about which process owns the window **(verified only as far as the portal's interface: `Start` returns streams with `id`, `position`, `size`, `source_type`; no pid; memory for Chromium's use of it)**. Mapping a window to an audio stream would need a compositor-specific window-to-pid path. L-S5 is therefore marked **unsupported for now** and the desktop offers two choices on Linux: the screen's sound (`system_excluding_call`) or none. The vocabulary stays (the Windows route offers `application`).

## 5. The route: capture on PipeWire

### 5.1 Shape

The helper, while the person's share is live and main has asked for sound, holds **one capture stream per eligible application output stream**, each a `Stream/Input/Audio` node connected with `target.object = <object.serial of that stream>` and `stream.capture.sink` **unset** (false), so that PipeWire links it to that application's output ports and to nothing else. It never connects a capture with `stream.capture.sink = true`, never names a sink or a sink's monitor as a target, and never uses `dmn-audio`'s `monitor_fallback`. The captures ask for 48 kHz, two channels, f32 (§15 D-5), and PipeWire's adapter converts each application's format; a stream that settles at another rate is refused, as the microphone's is (`MicError::RateMismatch`'s rule).

The helper mixes the captures frame by frame (the `Mixer` that already sums remote voices in `conch-voice-audio`, with its gain and clip rules), writes the mix to a second `NativeAudioSource`, and publishes it as one `LocalAudioTrack` with `TrackPublishOptions { source: TrackSource::ScreenshareAudio, dtx: false, .. }` on `p<id>`. DTX is off because music and games are not speech; the bitrate follows the SDK's stereo default (the web client's `audioPublishOptions(stereo)` is the precedent, `sdk.ts:1322`).

### 5.2 Eligibility: everything but the call

A `Stream/Output/Audio` node is **eligible** unless any of these holds:

1. its `pipewire.sec.pid` is the helper's own pid (the helper's playback stream, set by protocol, not spoofable);
2. its `application.process.id` or `pipewire.sec.pid` is in the **Conch process tree**: the helper's pid, the helper's parent (Electron main, `std::os::unix::process::parent_id`, which the helper already watches for owner loss) and every descendant of that parent, read from `/proc/<pid>/stat` at evaluation time (the renderer, GPU and audio service processes of Chromium are children of main);
3. its `application.name` equals the desktop application's registered name, passed by main in the v3 command (§7), as a second net for Chromium's audio process, whose `application.process.id` may name a process the tree walk misses between reads;
4. its `node.name` or `media.class` marks it as one of the helper's own streams (the helper's own capture streams are `Stream/Input/Audio` and never match the class; this is belt and braces).

Everything else is eligible: a browser, a game, a music player, a notification sound, whatever sink it plays to. The set is **re-evaluated on every registry change** `dmn-audio`'s `NodeTable` reports (a stream added, removed or changed, as `Reattach` already watches), so an application that starts during the share is captured within one registry round trip and one that quits drops out with its stream.

**Why the tree and not a list:** the Windows design excludes "our own process tree (the helper and the Electron tree)" by construction (windows-desktop §8); this is the same rule by other means. **Why advisory properties are acceptable here** (§2.5): the exclusion protects the **sharer** from re-broadcasting the call and from sharing their own application's sound back; an application that lies about its pid can only *hide itself from the share* (no harm to anyone) or *fail to hide* (it is a third-party application, whose sound the person chose to share). No authority depends on the property. The one process whose exclusion matters for feedback, the helper itself, is excluded by `pipewire.sec.pid`, which the protocol sets.

**Fail closed:** if `/proc` cannot be read, if the parent pid is 1 (owner gone), or if the helper cannot tell its own pid, share audio is `unavailable` with `share_audio_problem: no_process_tree` and nothing is captured. The helper never answers "I could not work out what to exclude" by capturing everything.

### 5.3 Ordering and lifetime

1. Main holds the share claim (`POST …/voice/share`, the companion publishing video) and the person's standing choice is "with sound".
2. Main records the consent: `PUT …/voice/companion/audio {"scope":"system_excluding_call","publisher":"person"}` (§8). On any refusal the share continues silent and the strip says why (§11); main never sends the helper the command without a recorded consent.
3. Main sends `set_share_audio {capturing: true, scope: "system_excluding_call", application_name: "<name>"}` on the command channel.
4. The helper, in phase `connected` and with `companion_identity` present in its session (there is a desktop; a TUI session refuses with `unsupported`), evaluates eligibility, opens the captures, publishes the track, and reports `share_audio: on` with `share_audio_sources: N`.
5. On every end (§10) main sends `set_share_audio {capturing: false}` **and** `DELETE …/voice/companion/audio`, in that order, before or with ending the share; the helper unpublishes the track and closes the captures within `VoiceControlCaptureStopMS` (500 ms), as the microphone's stop is bounded.
6. Owner loss (EOF on either channel, parent change, a byte on the events channel) stops the capture with the rest of the session, by the existing fail-closed path.

The desktop's own voice session is the only one that ever carries this: `conch voice` (TUI) owners speak v1/v2 and never see the command; a standalone `conch-voice join` has no owner and never captures.

## 6. Who publishes, and model B's participant boundary

Model B's text says: "One playback owner per audio source: voice audio is the helper's, share audio is the companion's" (linux-desktop §6), and "share audio is never in the grant, and is added by a conchd permission update on the connected companion only when main asks after an explicit audio-scope consent (on Linux, with no audio route yet, never)". Both sentences were written about **playback and about the companion's grant**; neither considered the helper publishing. The proposal:

- **Publication** of the share's sound on Linux is the **helper's**, on `p<id>`, under the person's own share grant. The companion's grant stays `["screen_share"]` and is never widened on Linux. The renderer holds one credential, as before, and gains no capability.
- **Playback** of others' share sound is unchanged: the companion's under B2, the helper's under B1 or with no companion (#934).
- **Subscription** is unchanged: the helper never subscribes to its own tracks, nor to its companion's.
- **Presence, claims, audit** are unchanged: the share is the person's; `sharing_audio` is the OR over the person's participants (§2.7).

Why this respects rather than breaks the boundary: model B drew the line to keep **voice** in one trusted native process and **screen video** in Chromium's mature capture path. The screen's sound has neither of those reasons to be in Chromium (Chromium cannot capture it on Linux) and one strong reason to be in the helper (it already owns PipeWire and the gate). The Windows design already anticipated this ("the helper publishing a `screen_share_audio` track on its own participant that viewers associate through presence" is one of the two plumbings it names and leaves to W4, windows-desktop §8). The amendment is one sentence in §6 and the §8 table rows (§17).

**What this does not change for Windows:** W4 may keep the renderer-publishes route with the consent grant, or adopt this one with a WASAPI capture in the helper; that is #778's design, and the control-channel addition of §7 is platform-neutral so that it can.

## 7. The control channel: `conch.voice_control.v3` (proposal)

A new version, by v2's rule (§2.9): a v3 session is v2 plus the following; the hello offers `v1`, `v2`, `v3`; the version is fixed for the session; a v3 line in a v2 session is `unknown_schema`. Nothing in v1 or v2 changes. Every shape lives in `pkg/schema/voice_control_v3.go` with golden and conformance fixtures under `testdata/voice-control-v3/`, generated TypeScript (`web/src/schema/`, for main) and Rust conformance in `conch-voice-control`, under the `schema-change` skill. Flat structs, string enums, no free text from the helper, no command that opens the microphone's gate.

**Commands (owner to helper), desired state like `set_mute`:**

| Kind | Fields | Meaning |
| --- | --- | --- |
| `set_share_audio` | `capturing: bool`; `scope: VoiceShareAudioScope` (the existing vocabulary, `application` or `system_excluding_call`), required when `capturing`; `application_name: string` (1–128 bytes, printable, no control characters; the desktop's registered `application.name`, for §5.2 rule 3), required when `capturing` | Capture and publish the share's sound, or stop. Idempotent. Refused into the state, never by a protocol error: `unsupported_scope` for `application` on Linux; `unsupported` when the session has no `companion_identity`; `not_connected` outside phase `connected` |
| `set_share_audio_mute` | `muted: bool` | Mute the published share-audio track (LiveKit `mute`: the track stays published, viewers see it muted). Independent of `set_mute` (microphone) and `set_deafen` |

**State (`state` event) gains, always written:**

| Field | Type | Meaning |
| --- | --- | --- |
| `share_audio` | `off`, `on`, `unavailable` | `on`: captures open and the track published; `unavailable`: asked for and not achievable now, with `share_audio_problem`; `off`: not asked for, or stopped |
| `share_audio_muted` | bool | the mute above |
| `share_audio_problem` | `""`, `no_pipewire`, `no_process_tree`, `publish_refused`, `unsupported_scope`, `unsupported`, `not_connected`, `no_sources` | why `unavailable`; empty otherwise. `no_sources` is informational: eligible streams are zero right now (nothing is playing), the track is published and silent, `share_audio` is `on` |
| `share_audio_sources` | int, 0–1024 | eligible streams being mixed now; for the strip's "sound from 3 apps" |

**Validation rules (added to v2's):** `share_audio` is `off` in `starting` and the final phases; `share_audio_problem` is non-empty exactly when `share_audio` is `unavailable`, except `no_sources`, which may accompany `on`; `share_audio_sources` is 0 unless `share_audio` is `on`; `share_audio_muted` may be true in any phase (a desired state, like `muted`). `transmitting` is unaffected by any of these: the share's sound is not the microphone and never touches the gate.

**Why not a flag at launch instead of a version:** the capture must start and stop with each share, not with the session, and the owner must learn that it failed; only the command channel carries authority and only the state carries confirmation.

**Fixtures:** the v3 golden set (each command and a state in each phase), the conformance cases (`application` scope refused into the state; `capturing` without `scope` invalid; a v3 command in a v2 session is `unknown_schema`; `share_audio_problem` non-empty with `on` invalid except `no_sources`), and the v1 and v2 frozen fixtures unchanged byte for byte.

## 8. The server: consent that names the publisher

`PUT /v1/channels/{channel}/voice/companion/audio` is the one place a share's audio consent is recorded (voice-control-plane §13; #1552: "conchd still records it … on each share"). Today it does three things: records `voiceShare.audioScope`, writes `voice_share_audio_started`, and grants the companion `screen_share_audio`. Under route A the third is wrong: the companion will never publish sound on Linux, and a grant it holds is exactly the residual §10 refuses ("synthesised speech cannot be published as the person").

**Proposal (S2):** `VoiceCompanionAudioRequestV1` gains an optional field `publisher: VoiceShareAudioPublisher`, a closed vocabulary `companion` | `person`, absent meaning `companion` (today's behaviour, byte for byte). With `person`:

- the consent is recorded on the share (`audioScope`, plus `audioPublisher`), before LiveKit would be asked; **LiveKit is not asked**: the person's grant already carries the sound (§2.7) and the companion is left at `[screen_share]`;
- `voice_share_audio_started` is written as today, with `publisher=person` added to the detail (additive);
- presence `sharing_audio` follows the person's own `ScreenShareAudioPublished`, as today for a browser sharer; a **companion** track seen with a `person` consent is unconsented and withdrawn as unclaimed, exactly as with no consent (`voice_share.go:1335`, `:1460`: the checks become "a consent naming the companion");
- `DELETE …/companion/audio` is unchanged: it clears the consent, writes `voice_share_audio_stopped reason=self`, and sets the companion back to `[screen_share]` (a no-op write it already makes);
- every end of the share ends the consent and writes the stopped row, unchanged;
- changing `publisher` on a live consent is a scope change (a `started` row with the new detail; the companion's grant is added or withdrawn to match);
- `VoiceCompanionAudioV1` (the response) gains `publisher`, echoing what was recorded, so main never has to remember what it asked for.

**Compatibility:** the request is decoded strictly (`DecodeVoiceCompanionAudioRequestV1`, `DisallowUnknownFields`), so an **older server answers a `publisher` field with 400 `invalid_request`**; main treats that as "this server has no sound route for the Linux desktop", keeps the share silent, and says so (§11). That is right: an older server also predates every other part of this route. A newer server with an older client (no field) behaves as today. The response gains an optional field, which the generated TypeScript validator tolerates for responses (responses tolerate unknown fields, `voice_companion.go` header), and the v1 frozen rule of #1507 concerns documents readers validate closed (presence, share), not this request. The published presence and share shapes gain nothing.

**Alternative (S4):** a new route `PUT …/voice/share/audio {scope}` for the person's own share sound, leaving the companion route alone. Cleaner names, one more route to audit and isolate, and two places a consent lives. Not recommended. **Alternative (S0):** call the companion route as today and accept the dangling grant: rejected, it contradicts §6.4 and the §10 row "share audio published without a press or a gate".

`e2e/isolation` gains a row for `publisher` (a non-member, a non-holder, a viewer-only person, each refused as today). No MCP tool is involved: agents are not in voice (voice-control-plane §1); parity holds through REST (rule 4).

## 9. The desktop: default, preference, words

- **Preference**, per profile, in main's preferences store: `shareAudio: "system_excluding_call" | "none"`, default **`system_excluding_call`** (#1552). The renderer reads it through the existing preferences IPC and never writes credentials with it.
- **Before the picker** (UX-15: limitations appear before starting): the Share control shows the standing choice as a labelled toggle, "Share with sound" on by default, with the one-click alternative "Share without sound". The choice made here is remembered. With no route available at that moment (the helper's last state said `unavailable`, or the server refused `publisher`), the toggle is disabled with the reason (§11) and the share proceeds silent, as #1582 shows today.
- **While sharing**: the indicator reads "Sharing your screen and its sound in #name" (or "… without sound …"), with a **share-sound mute** beside Stop, independent of the microphone mute and labelled so (UX-15: "Keep microphone and stream-audio mute independent"). `share_audio_sources` may be shown as "sound from N apps"; 0 shows "nothing is playing".
- **Changing the choice mid-share** is allowed: turning sound off sends `set_share_audio {capturing:false}` and `DELETE …/companion/audio`; turning it on repeats §5.3 steps 2–3. Each is a fresh consent or withdrawal in the audit log.
- **Words** live in `web/src/voice/native/words.ts` (#1582's file) and are tested there. No inline style, no `dangerouslySetInnerHTML`.
- **UX-15 wording:** "separately choose whether to include supported system audio" is met by the labelled toggle; a one-line note under UX-15 that the desktop's default is on where a route exists (#1552) is a doc follow-up on acceptance, not a decision.

## 10. Lifecycle rules

| Event | Rule |
| --- | --- |
| Share starts, sound on | §5.3: consent recorded, then the command; the strip shows "with sound" only once the state says `on` (confirmed state, never optimistic, linux-desktop §4) |
| Share-audio mute | `set_share_audio_mute {muted:true}`: the track is muted at LiveKit, captures stay open, consent stays recorded, `sharing_audio` stays true; viewers see the track muted. Unmute is the reverse. Nothing else changes |
| Microphone mute, unmute | nothing: the share's sound is not the microphone, and `set_mute` never opens a capture ("microphone mute never turns on a captured monitor", §8: there is no monitor to turn on) |
| Deafen, undeafen | nothing for the capture: deafen stops what the helper plays, and the helper's playback is excluded from the capture anyway; the companion's playback of watched streams is excluded too |
| An application starts, stops or changes sink during the share | re-evaluated on the registry change; captured or dropped; `share_audio_sources` updates. No device is involved: the captures follow streams, not sinks |
| The helper's speakers change (`set_devices`, a default-sink change) | nothing: exclusion is by process, not by node, so the helper's new playback stream is excluded on its first registry event |
| PipeWire restarts | `dmn-audio`'s re-attach rebuilds the captures; while gone, `share_audio: unavailable no_pipewire`; the published track plays silence; main says "sound paused" |
| Operator stops the share (`DELETE …/share?principal=`), access lost, idle, left | conchd withdraws the person's share permission; LiveKit unpublishes the track **(memory: a permission update revokes tracks of sources no longer permitted; LE2's companion scenario shows it for the companion)**; the helper sees its track unpublished, closes the captures and reports `off`; main also sends `capturing:false` when presence shows the share ended; the stopped rows are conchd's, with the share's reason |
| Room renewal (rotation) | the share ends with words (Q-D4-2); main sends `capturing:false`; the consent ends with the share (`reason=rotated`); a new share is a new consent |
| Lock (logind `Lock`) | main stops the companion's capture and sends `capturing:false` and the DELETE; voice stays with `set_mute true` as §9 says. "Lock stops both streams" holds |
| Suspend (`PrepareForSleep(true)`) | `leave`: the helper's teardown closes the captures with the session |
| Leave, sign-out, profile switch, quit | the share ends first (main's `teardownHooks.media`), then voice; the captures close with the share or with the helper, whichever is first |
| Helper loss (crash, owner-loss exit) | the captures die with the process (PipeWire removes a client's objects); `p<id>` is gone, so the share ends `left` and the companion is orphaned within 5.5 s (voice-control-plane §13); main ends the companion at once (D4-02) and sends the DELETE |
| Renderer crash | main ends the companion (D4-02); the share's video is gone, the share ends `idle` within 5 s; main sends `capturing:false` and the DELETE at once rather than waiting |
| Second device claims the share | refused (`voice_companion_busy` or the share's own rules); nothing here changes |
| Owner loss | the helper's existing fail-closed exit: gate shut, captures closed, track gone, within `VoiceControlOwnerLossExitMS` |

## 11. Failure modes and what the strip says

| Failure | State | Words (proposed, `words.ts`) | The share |
| --- | --- | --- | --- |
| Server refuses `publisher` (older server, 400) | main's error | "This server can't record a sound consent for the Linux app yet. Your screen is shared without sound." | silent, continues |
| Server refuses the consent otherwise (`voice_no_session`, 409) | main retries once after one pass, then | "Sound couldn't be added to this share; try Share again." | silent, continues |
| No PipeWire, or `dmn-audio` cannot open a capture | `unavailable`, `no_pipewire` | "Your screen's sound can't be shared right now: the sound system isn't reachable." | silent, continues; retried on the next registry event |
| Process tree unreadable | `unavailable`, `no_process_tree` | "Your screen's sound can't be shared right now." (the detail goes to the log, by reason only) | silent, continues |
| LiveKit refuses the publish (no share grant yet, a pass in flight) | `unavailable`, `publish_refused` | main repeats the command once after one pass (1.5 s); then "Sound couldn't be added to this share; try Share again." | silent, continues |
| Nothing is playing | `on`, `no_sources`, 0 | "Sharing your screen and its sound (nothing is playing)" | with sound, silence |
| `application` scope asked (never by this desktop) | `unavailable`, `unsupported_scope` | not shown; a test asserts main never sends it on Linux | — |
| TUI or standalone session gets the command | `unavailable`, `unsupported` | not applicable: v1/v2 owners cannot send it | — |
| Capture ring overrun, a slow mixer | counted in the helper's stats, logged by count | nothing; the audio drops frames, never stalls the gate | continues |

Every message appears before the share starts where it can (UX-15) and in short while sharing (UX-23). None names a token, a room, a path or a device identifier.

## 12. Threat rows for linux-desktop §10

| Threat | Boundary and required verification |
| --- | --- |
| Share audio returns the room's own voices (through the helper's playback or the companion's playback of watched streams) | No monitor is ever opened: every capture targets one application output stream by `object.serial`; `monitor_fallback` and `stream.capture.sink` are never set (a unit test over the capture specs, and a test that the `SinkMonitor` target is unreachable from the helper's code). The Conch process tree is excluded by `pipewire.sec.pid` (the helper), by pid tree (Electron and its children) and by `application.name`. **Gate: L-S6 by hand with a measurement listener** (§14.4) |
| Share audio published without a press, a gate or the owner's word | The publisher is the helper, which publishes only on `set_share_audio` received on the command channel from the spawning process after main recorded a consent; the renderer has no path to that channel, and its grant never carries `screen_share_audio` on Linux. **Stricter than today's text**, which allows the companion to be granted audio after a consent |
| A compromised renderer makes main ask for sound | Main's IPC checks: gesture, owned frame, generation, the share live, the preference; the worst outcome is the person's own screen's sound shared during a share the person started with the toggle visible, the residual Nick's default accepts |
| A local application spoofs `application.process.id` or `application.name` | It can only hide its own sound from the share (it is matched as Conch's) or fail to; nothing it does includes the call: the helper's own stream is excluded by `pipewire.sec.pid`, which the protocol sets, and Electron's by two advisory properties **and** the pid tree. No authority depends on these properties |
| The sharer shares sound they did not mean to (a notification, another call in another application) | The default is on by Nick's decision; the mitigations are the labelled toggle before the picker, the "with sound" indicator, the share-sound mute and Stop within reach, the per-share consent in the audit log, and `share_audio_sources` showing that something is being mixed. Stated as accepted |
| Picture and sound from two participants drift | No sync across participants; measured in LE8; accepted for a screen share |
| The helper publishes something other than the mix (a compromised helper) | The helper is already trusted with the microphone and the gate; nothing new. Its track is bounded by the person's share grant and by conchd's passes, which end it with the share |
| Another local user reads the capture | Same-user is out of scope as everywhere; PipeWire's socket is the user's |
| Consent outlives intent | Every end of the share ends the consent (unchanged, #898); main sends `capturing:false` and the DELETE on every end it sees; the helper stops on owner loss; conchd's passes withdraw a person's share sources with the share |

## 13. Dependencies and licences

| Need | Where | Licence | New direct dependency? | Alternatives |
| --- | --- | --- | --- | --- |
| Capture an application output stream at 48 kHz stereo by `object.serial`, with `monitor_fallback` impossible to select; expose `pipewire.sec.pid`, `application.process.id` and `application.name` of each `Stream/Output/Audio` node in `NodeTable` (`AppStream` carries `application.name`, `media.name`, `node.name` and `object.serial` today, `src/pipewire.rs:78–88`; neither pid property is read yet) | **`dmn-audio`** (Nick's repository, MIT, already in `voice/deps-allowlist.txt`), a new commit pinned in `voice/conch-voice/Cargo.toml` | MIT | **No new crate.** A rev bump is a dependency change under the allowlist's own rule ("adding a line requires sign-off"; a rev bump is listed for Nick in §15 as the same kind of care) | (i) `pipewire` 0.10 (MIT OR Apache-2.0, already transitive through `dmn-audio`) as a **direct** dependency of `conch-voice`, building the captures in `voice/`: a new allowlist line, two owners of one PipeWire loop in one process, and the per-stream pid properties still needed from somewhere. (ii) The explicit-links route E, which needs `pipewire` directly as well |
| Read the process tree | `std::fs` over `/proc/<pid>/stat` in `conch-voice` | — | none | a `procfs` crate: unnecessary for one field |
| Publish a second local audio track with a source | `livekit` 0.9.4, already pinned | Apache-2.0 | none | — |
| Mix N rings | `conch-voice-audio`'s mixer | — | none | — |
| Main's preference, IPC and words | `desktop/`, `web/src/voice/native/` | — | none | — |

No Go dependency changes. No new `go.mod`, `web/deps-allowlist.txt` or `voice/deps-allowlist.txt` line under the recommendation; one `Cargo.lock` change (the `dmn-audio` commit). `cargo deny check` covers the transitive set as today.

## 14. Tests the implementation must ship with

### 14.1 Schema (`pkg/schema`, v3)

Golden fixtures for every v3 command and a state in each phase; conformance cases as §7 lists; the Rust twin in `conch-voice-control` reads them; the generated TypeScript for main; `voice_v1_frozen_test.go` and the v2 fixtures unchanged byte for byte; `schema-compat` green.

### 14.2 Helper (`voice/conch-voice`), no PipeWire needed

- The eligibility rule as a pure function over `NodeTable` snapshots and a fake process tree, table-driven: the helper's own pid excluded by `pipewire.sec.pid`; a descendant of the parent excluded; `application.name` match excluded; an unrelated stream eligible; a `pipewire-pulse`-fronted stream with the parent's tree in `application.process.id` excluded; an unreadable tree yields `unavailable no_process_tree` and zero captures.
- No capture spec ever has `capture_sink` true or a sink target: a test over every spec the module can build, and a compile-time absence of the `SinkMonitor` target in the helper's own code (a grep test, as `TestUpdateParticipantStaysInThePermissionFunction` is structural).
- The state machine: `set_share_audio` before `connected` is `not_connected`; without `companion_identity` is `unsupported`; `application` is `unsupported_scope`; stop within 500 ms; owner loss closes everything; `set_share_audio_mute` mutes the track and nothing else; `set_mute` and `set_deafen` leave the captures as they were.
- Mixing: N synthetic rings sum with the mixer's clip rule; an empty set publishes silence and reports `no_sources`.
- Marked `#[ignore]`, saying it needs PipeWire: two `pw-play` children and the helper's own playback of a tone into a null sink; the mix contains the two and none of the tone (an automated shadow of L-S6, not its replacement).

### 14.3 Server (`internal/server`)

- `publisher: person` records the consent and the rows, makes no `UpdateParticipant` call, and leaves the companion at `[screen_share]`; a companion track seen under a `person` consent is withdrawn as unclaimed with the existing row; `DELETE` clears it; every share end writes `voice_share_audio_stopped` once with the share's reason; the absent field means `companion`; `publisher` echoed in the response; `e2e/isolation` rows.
- `e2e/voice` scenario `companion` extended (live half): the managed helper, with a test-only synthetic share-audio source (as the tone microphone is), is told `set_share_audio` after the claim and the consent; a browser viewer receives exactly one `ScreenShareAudio` track for the stream, from `p<id>`, and the video from `p<id>.screen`; `sharing_audio` is true; the companion's audio publish is refused throughout; the operator's stop ends the track and writes the rows in order; before the claim the helper reports `publish_refused`.

### 14.4 By hand: the L-S6 feedback test, the gate (#922, LE8 row)

Nick at the keyboard on the Pop!_OS COSMIC machine, a **second machine speaking**, and a **measurement listener**: a third account in a browser on a third device (or the second machine), watching the share, recording the stream's audio track with `MediaRecorder` for the whole run, and reporting peak and RMS per 10 s window (a small page or script kept under `e2e/`, not shipped). Receipts go in the capability report with no desktop contents, tokens or conversation content.

| Step | Do | Pass when |
| --- | --- | --- |
| 1 | Join voice on the desktop; share the screen with sound; nothing else plays | the listener sees `sharing_audio` true and records digital silence (peak below −80 dBFS) for 60 s; `share_audio_sources` is 0 |
| 2 | The second machine speaks continuously for 60 s; nothing else plays locally | the listener's recording of the **share's** track stays below −80 dBFS throughout, while the listener hears the speech on the **voice** track; this is the feedback test |
| 3 | The second machine shares a screen with sound; the desktop watches it (B2: the companion plays it) while the second machine also speaks | the share's track from the desktop stays below −80 dBFS |
| 4 | Play a local video with sound in a browser | the listener hears it on the share's track; `share_audio_sources` is 1 |
| 5 | Start a second local application with sound; quit the first | the count follows; the sound follows |
| 6 | Share-sound mute; unmute | the listener's track goes silent and returns; the voice track is unaffected |
| 7 | Microphone mute; deafen; undeafen; unmute | the share's track is unaffected; deafen silences only what the desktop hears |
| 8 | Change the default sink; unplug and replug headphones | the share's track continues; no speech from the second machine appears |
| 9 | Restart PipeWire (`systemctl --user restart pipewire pipewire-pulse wireplumber`) | the share's track pauses and resumes; no speech appears |
| 10 | Lock the screen; unlock | the share and its sound end; `voice_share_audio_stopped` and `voice_share_stopped` are written, in that order |
| 11 | Share again; the operator stops it from the CLI | the track ends; rows in order |
| 12 | Share again; kill the helper; then share again and kill the renderer | the share ends each time; nothing is captured after; rows written |
| 13 | A clap on camera on the shared screen against its sound at the listener | the offset is recorded (A/V drift), not judged |

**Pass:** steps 1–3 and 7–9 show no returned speech in any window, and every end in 10–12 writes its rows. Any speech from the second machine on the share's track **fails the gate**, and the route is not offered until the cause is found. GNOME, KDE and X11 cells stay pending until those machines exist, as the report's rule says.

### 14.5 Desktop (`desktop/`, `web/src/voice/native/`)

The default and the remembered choice per profile; "Share without sound" sends no consent and no command; the order consent-then-command; every end sends command-then-DELETE; the 400 on `publisher` and each `share_audio_problem` have their words; the indicator and the independent mute; `make check`, `make web-check`, `make desktop-check`; a `security-reviewer` pass on main's part (it touches the share-audio path).

## 15. Decisions for Nick

| # | Decision | Options | Recommendation | Why |
| --- | --- | --- | --- | --- |
| **D-1** | **Architecture:** may the helper publish `ScreenshareAudio` on `p<id>` for a share whose picture the companion publishes? (#772's question; amends model B's "share audio is the companion's", §6, §17) | A (helper publishes); B (renderer publishes a helper-made virtual source); C (no Linux route); D, E, F as §4 | **A** | the only route that neither opens a monitor nor gives the renderer an audio capture; it uses what exists (the person's grant, `publisherOf`, `dmn-audio`, the SDK's track source); B breaks ADR-007's one-microphone-owner rule; C leaves the decision of #1552 unmet on Linux |
| **D-2** | **Schema version:** `conch.voice_control.v3` as §7 | as proposed; a smaller v3 (one command, one state field); defer | **as proposed** | a new version is forced by v2's own rule; the fields are the minimum for confirmed state and honest words; no free text, no gate command |
| **D-3** | **Server route:** how the consent for a person-published sound is recorded (amends #1552's "through `PUT …/companion/audio`" by one field) | S2 `publisher` field on the companion audio request (additive; older server 400); S4 a new route; S0 none (dangling companion grant) | **S2** | one place for a share's consent; no LiveKit call; the companion is never granted audio on Linux; S0 contradicts §6.4 |
| **D-4** | **Where the capture lives** and the dependency | `dmn-audio` (rev bump; Nick's repository; no new crate); `pipewire` as a direct dependency of `conch-voice` (allowlist line) | **`dmn-audio`** | one owner of the PipeWire loop in the helper's process; the app-stream capture exists there; the stereo rate and the pid properties are small additions |
| **D-5** | **Format:** 48 kHz stereo (needs `dmn-audio`'s stereo capture) or 48 kHz mono (closer to what exists) | stereo; mono | **stereo** | the sound is music and games; the web client already publishes stereo share audio; mono is the fallback if `dmn-audio`'s work is delayed |
| D-6 | "This application" scope on Linux: unsupported for now (L-S5) | unsupported; a compositor-specific window-to-pid route later | **unsupported for now** | the portal gives no pid; the vocabulary stays for Windows. Delegable |
| D-7 | The exclusion rule (§5.2): process tree plus `application.name`, fail closed | as proposed; pid only; name only | **as proposed** | the helper by protocol pid; Electron by two means; nothing capturable when the tree is unreadable. Delegable |
| D-8 | #1586's split (§16) | one XL issue; five issues | **five** | three repositories or crates and two owners; the by-hand gate stays in #922. Delegable |

Nick signs D-1 to D-5 (architecture, a schema version, a route on the share-audio path, a dependency change). D-6 to D-8 are his to delegate to the principal. The amendments of §17 are made in the PR that flips this note to Accepted, with his yes recorded here.

## 16. Implementation split, and #1586 re-sized

#1586 (D4-09) as filed is **L, estimate**. From this note it is **XL as one issue**: a schema version, a change in Nick's `dmn-audio`, a helper feature with a new PipeWire capture path, a server change on the share-audio path, main and web work, and a by-hand gate. Recommended: **promote the stub to five issues**, filed to the template when §15 is signed, each executable cold with this note; the stub itself becomes the helper issue (c), which is the one #1552 named. None touches the approval path (`.github/workflows/process-checks.yml`'s list); each says so.

| Id | Title | Owner | Size | Blocked by |
| --- | --- | --- | --- | --- |
| D4-09a | Schema: `conch.voice_control.v3` (`set_share_audio`, `set_share_audio_mute`, the four state fields), fixtures, TS and Rust conformance | protocol-designer | S | D-2 signed |
| D4-09b | `dmn-audio`: an application-stream capture at a chosen rate, stereo, by `object.serial`, with no monitor fallback; `pipewire.sec.pid` and `application.process.id` in `NodeTable` (in `njdaniel/dmn-audio`; then the rev bump in `voice/`) | Nick's repository; the voice-engineer may draft | M | D-4, D-5 |
| D4-09c (#1586 rewritten) | Helper: eligibility rule, captures, mixer, `ScreenshareAudio` publish on `p<id>`, v3 commands and state, the §14.2 tests and the `#[ignore]` PipeWire test; the test-only synthetic share-audio source for `e2e/voice` | voice-engineer | L | a, b |
| D4-09d | Server: `publisher` on the companion audio request and response, consent without a grant, the unclaimed rule, audit detail, `e2e/isolation` rows, the `companion` scenario's share-audio lines | server-engineer | S | a (for the e2e half: c) |
| D4-09e | Desktop: the per-profile default and toggle, consent-then-command, every end's command-then-DELETE, the words, the independent mute; `security-reviewer` pass | web-engineer (Electron brief) | M | a, c, d; #1582 |
| #922 LE8 | By hand: §14.4 on Nick's machine; the capability report's L-S5 (unsupported for now) and L-S6 (verified or refuted) rows | Nick, with a second listener | — | c, d, e |

Release rule: the Linux desktop offers "with sound" only in a release whose notes cite a passed L-S6 receipt in the capability report; before that, the toggle is absent and #1582's silent-share words stand.

## 17. Amendments to accepted text, for the PR that accepts this note

None is made now. On Nick's yes:

- **linux-desktop.md §6**, the sentence "voice audio is the helper's, share audio is the companion's": add "**for playback**; on Linux the share's sound is **captured and published by the helper on `p<id>`** (linux-share-audio.md), and the companion's grant is never widened to audio there". §6.4 first bullet: "On Linux, where §8 has no audio route yet, main never asks and the companion never has it" becomes "On Linux main records the consent with `publisher: person` and the companion is never granted audio". D1-6 in §0 and the §12 gates table gain a row for this note.
- **linux-desktop.md §8**: the table's "this application" row becomes **unsupported for now** with the portal reason; the "everything but the call" row becomes **the route of linux-share-audio.md §5, gated by L-S6**; "off by default" becomes "on by default where a route exists (#1552), chosen per profile, with one-click off"; the safety sentence is kept word for word. §10 gains the rows of §12 here, and the row "Share audio published without a press or a gate" is tightened as §12 says. §14 L-F10 points here.
- **voice-control-plane.md §13**, "The companion's share audio": "On Linux main never calls the route (no audio route exists there, §8)" becomes "On Linux main calls the route with `publisher: person` (linux-share-audio.md §8) and the companion is never granted the sound"; the `publisher` field and the audit detail are added.
- **windows-desktop.md §8**: a one-line pointer that the helper-publishes plumbing it named is the Linux design, and that W4 may adopt it.
- **client-experience.md UX-15**: the one-line default note of §9.
- **voice-multi-share.md §7.3**: "It shows no share text and plays share audio by the existing rules" gains "and on the Linux desktop publishes the person's share sound (linux-share-audio.md)".
- **#1552** and **#772**: a comment each, recording D-1 to D-5 and linking this note; #1586 rewritten as §16.

## 18. Sources

Repository (`aaea4d11`, 2026-11-09): `docs/design/linux-desktop.md`, `voice-control-plane.md`, `voice-multi-share.md`, `windows-desktop.md`, `conch-voice.md`, `client-experience.md`; `docs/reviews/linux-desktop-capabilities.md`; `pkg/schema/voice_control.go`, `voice_control_v2.go`, `voice_companion.go`, `voice_v1_frozen_test.go`; `internal/server/voice_permission_fn.go`, `voice_share.go`, `voice_companion_audio.go`, `routes.go`; `voice/conch-voice/src/livekit.rs`, `session.rs`, `cli.rs`, `audio_linux.rs`; `voice/deps-allowlist.txt`, `voice/Cargo.toml`, `voice/conch-voice/Cargo.toml`, `scripts/voice-pins.sh`; `web/src/voice/sdk.ts`, `presence.ts`; `e2e/voice/`; `desktop/main/voice/`. Issues #1552, #1578 (and its decision comment), #1579, #1582, #1586, #772, #922.

External, fetched 2026-11-09: [Electron 45.1.0 `session`](https://raw.githubusercontent.com/electron/electron/v45.1.0/docs/api/session.md) (`setDisplayMediaRequestHandler` audio, `setPermissionRequestHandler` media details, `setDevicePermissionHandler` device types) · [pipewire-props(7)](https://docs.pipewire.org/page_man_pipewire-props_7.html) (`target.object`, `node.autoconnect`, `pipewire.sec.pid`, client properties are advisory) · `dmn-audio` rev `39e7748` (`src/lib.rs`, `src/capture.rs`, `src/pipewire.rs`, `Cargo.toml`) · `livekit` 0.9.4 (`src/room/options.rs`, `TrackPublishOptions.source`). [OBS PipeWire audio capture](https://github.com/dimtpap/obs-pipewire-audio-capture) was fetched; its page states three sources and PipeWire 0.3.62, not the linking mechanism, which stays **(memory)**.

From memory, marked so in the text: Chromium's `getDisplayMedia` audio on Linux; Chromium reaching PipeWire through the PulseAudio API; LiveKit's unpublish on a permission revocation and its within-participant sync; the ScreenCast portal's `Start` result carrying no pid; the OBS plugin's explicit-links mechanism; `Core::create_object` for links in the `pipewire` crate.
