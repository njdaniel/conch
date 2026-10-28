# Design: the Windows desktop client (W1, issue #775)

- **Status:** Proposed, 2026-10-28. This is the research-and-design half of W1. No Windows machine was available, so nothing here was run on Windows: every claim that needs native evidence is **pending (needs hardware)** in the capability matrix, [docs/reviews/windows-desktop-capabilities.md](../reviews/windows-desktop-capabilities.md), with the experiment that would settle it. Nothing below is marked verified without a cited source.
- **Issues:** epic #756; this note is #775. Implementation children: #776 (audio and transport), #777 (push-to-talk), #778 (screen and scoped audio), #779 (profiles, tray, lifecycle), #780 (installer, signing, updates). Coordinated with the Linux desktop design #769, the shared shell #770 and the shared lifecycle #773.
- **Governed by:** [ADR-006](../adr/ADR-006-rust-voice-client.md) (the Rust voice client; its platform line is Linux with PipeWire, and §11 below proposes the amendment), [ADR-007](../adr/ADR-007-client-interfaces.md) (Electron hosts the shared React application; a native client manages a session-scoped helper over a versioned local contract), the managed-helper contract [tui-voice.md](tui-voice.md), the client design [conch-voice.md](conch-voice.md) and the UX rules [client-experience.md](client-experience.md).
- **Wire shapes:** nothing published changes. `conch.voice_control.v1` and `v2` are carried to Windows byte for byte (§3, §6). Two additions are proposed, each a new versioned name needing the owner's sign-off through the `schema-change` skill: `conch.voice_control_pipe.v1`, one line that attaches the Windows events pipe to its owner (§3.4), and `conch.voice_devices.v2`, the device list with a `backend` field (§2.6).
- **Sources** were checked on 2026-10-28 unless a line says otherwise; each is cited where it is used and listed in §15. A claim written from memory says so.

An ordinary Windows user installs one app, saves their servers, chats, joins voice when they choose, holds a key or Mouse 4 while a game has focus and is heard exactly while it is held, and shares a screen on configurations that were tried. Everything that makes that true today on Linux (the gate, the mixer, the control protocol, the supervision rules) is kept; what changes is the audio device layer, the key source, and how two processes are tied together on an operating system without `/proc/self/fd/3`, process groups or `SIGTERM`.

## 0. The proposals in one table

| # | Area | Proposed | Needs the owner |
| --- | --- | --- | --- |
| W1-1 | Target (§1) | Windows 11 x64, 25H2 (build 26200) on every edition, 24H2 (26100) only where still serviced (Enterprise, Education, LTSC); nothing older; no ARM64, no Windows 10, no WSL | no |
| W1-2 | Toolchain (§1) | Rust 1.96.1 on `x86_64-pc-windows-msvc`; the pinned LiveKit libwebrtc's `webrtc-win-x64-release.zip`; Electron pinned to the current stable at #770 | `scripts/voice-pins.sh` gains a Windows hash |
| W1-3 | Audio (§2) | WASAPI through the `wasapi` crate (MIT), `cfg(target_os = "windows")`, behind the existing `MicSource`/`SpeakerSink` traits; `cpal` is the alternative | new crate |
| W1-4 | Transport (§3) | Commands on standard input as today; events on an owner-created named pipe the helper opens by name with safe `std`; a one-line attach proof; owner loss by EOF on either channel | `conch.voice_control_pipe.v1` |
| W1-5 | Push-to-talk (§4) | Raw Input (`RIDEV_INPUTSINK`), keyboard and mouse, press and release from the device's own transitions; never hooks, never activation callbacks | new crate (`winit`) or a reviewed `unsafe` policy change |
| W1-6 | Boundaries (§5) | Ordinary user; no driver, hook, injection or elevation; elevated foreground and the secure desktop are pending experiments and will be labelled, not worked around | no |
| W1-7 | Where input lives (§6) | In the helper. No press or release crosses the control channel; v1/v2 unchanged; bindings are configuration | no |
| W1-8 | Media ownership (§7) | Model B: the helper is the one voice participant; Electron is a server-sanctioned companion for screen video; #769 decides, and conchd work follows | design decision with #769, then a conchd issue |
| W1-9 | Scoped audio (§8) | Off by default; application audio by process-tree loopback; whole-system only through an exclude-our-tree loopback; never endpoint loopback of the call's own playback | no (W4 scope) |
| W1-10 | Credentials, lifecycle, distribution (§9) | `safeStorage` (DPAPI) with session-only fallback; `credential: env` to the helper; per-user NSIS installer by electron-builder; Azure Artifact Signing or EV costed for the owner; manual verified updates first | new npm dev dependencies; signing account |

## 1. Target and toolchain pins

**Decision proposed.** The first Windows client supports **Windows 11 x64, version 25H2 (OS build 26200)** on every edition, and **24H2 (OS build 26100) only on the editions Microsoft still services** (Enterprise and Education to 2027-10-12; LTSC 2024 to 2029-10-09). Microsoft's release table on 2026-10-26 lists 25H2 serviced to 2027-10-12 for Home/Pro and 2028-10-10 for Enterprise/Education; 24H2 Home/Pro is already "End of updates"; 23H2 Home/Pro has ended and its Enterprise/Education servicing ends 2026-11-10 ([Windows 11 release information](https://learn.microsoft.com/en-us/windows/release-health/windows11-release-information), checked 2026-10-28). So a Home or Pro machine is supported on 25H2 and nothing older. 23H2 is out entirely. 26H1/26H2 are listed as upcoming and are not promised. Since 25H2 is the same servicing branch as 24H2 with features enabled on top (memory), the build-26100 and build-26200 rows of the matrix are expected to behave alike, and E1 to E12 run on 26200. Windows 10, ARM64 (`aarch64-pc-windows-msvc` is Tier 1 in Rust, but LiveKit ships a `win-arm64` libwebrtc that nobody here has built against) and WSL-hosted audio are outside this target, as #756 says.

**Pins.**

| What | Pin | Source and status |
| --- | --- | --- |
| Rust | 1.96.1 (`voice/rust-toolchain.toml`, unchanged) | repository |
| Rust target | `x86_64-pc-windows-msvc`, Tier 1 with host tools, "Windows 10+" | [rustc platform support](https://doc.rust-lang.org/rustc/platform-support.html) |
| libwebrtc | tag `webrtc-89d790b` (`scripts/voice-pins.sh`), asset `webrtc-win-x64-release.zip`, published 2026-08-19 | [release assets via the GitHub API](https://api.github.com/repos/livekit/rust-sdks/releases/tags/webrtc-89d790b); the Windows SHA-256 is **pending**: fetch and pin it on Windows (experiment E1) |
| LiveKit SDK | `livekit = "=0.9.4"` (unchanged) | repository |
| MSVC and Windows SDK | **pending.** The LiveKit README names clang 21 as a requirement for Linux and says nothing about Windows ([rust-sdks README](https://github.com/livekit/rust-sdks)). Whether `webrtc-sys` on Windows wants MSVC's `cl`, clang-cl, or both, and which Windows SDK, is E1's first finding | not verifiable without a build |
| Electron | the current stable when #770 creates `desktop/`: today that is **45.1.0** (Chromium 156.0.8078.49, Node 24.22.1, released 2026-10-27), with 44.10.0 and 43.8.4 the other supported majors ([releases.json](https://releases.electronjs.org/releases.json)); Electron supports "the latest three stable major versions" ([timelines](https://www.electronjs.org/docs/latest/tutorial/electron-timelines)) | pinned by #770, with Nick's sign-off as a new dependency |
| Node for building | `web/.nvmrc` 24.21.0 today; the Electron runtime bundles its own Node | repository |

**Options weighed.** (a) Windows 10 as well: 22H2 Home/Pro ended 2025-10-14 (memory; not re-checked), so supporting it means supporting an unpatched OS, and the application-loopback API that §8 relies on needs build 20348 or later ([ApplicationLoopback sample](https://learn.microsoft.com/en-us/samples/microsoft/windows-classic-samples/applicationloopbackaudio-sample/)). (b) The GNU target (`x86_64-pc-windows-gnu`, also Tier 1): LiveKit's prebuilt libwebrtc is built with MSVC's ABI and ships only `win-x64`; mixing toolchains over a prebuilt C++ library is the kind of problem the Linux build already paid for once (ADR-006's clang requirement). MSVC it is.

**Platform-neutral.** The Rust version, the `livekit` version, `forbid(unsafe_code)`, `cargo-deny` and the depgate are shared. Only `scripts/voice-pins.sh` grows a second libwebrtc hash and `scripts/voice-toolchain.sh` a second triple (`win-x64-release`), and `deny.toml`'s `[graph] targets` gains `x86_64-pc-windows-msvc` so that Windows-only dependencies are checked too (today it lists only Linux, so a crate pulled in under `cfg(windows)` would be invisible to the licence check).

**Open questions.** E1: does `webrtc-sys` build on `windows-latest` with the pinned libwebrtc, and with which compiler; does `pipewire_stubs.o` removal have a Windows analogue (it should not: the Windows archive has no PipeWire).

**Follow-up.** F1 (§14).

## 2. The audio backend

**Decision proposed.** The microphone and speakers on Windows are WASAPI, through the [`wasapi`](https://docs.rs/wasapi/latest/wasapi/) crate (0.26.0, MIT, published 2026-10-11), as a second implementation of the two traits of `conch-voice-audio` (`MicSource`, `SpeakerSink`, `Pacing::Ring`) beside the PipeWire one in `voice/conch-voice/src/audio.rs`. `dmn-audio` becomes a `[target.'cfg(target_os = "linux")'.dependencies]` entry and `wasapi` a `cfg(target_os = "windows")` one; the gate, the mixer, the jitter buffers, the session loop and the control protocol are untouched. **`forbid(unsafe_code)` stays on every crate.** The rule in CLAUDE.md is that native code is reached only through dependencies; `wasapi` is to Windows what `dmn-audio` is to Linux, and nothing is moved anywhere to dodge the rule.

### 2.1 Candidates

| Crate | Version, licence, last publish | Covers what we need | Does not | Note |
| --- | --- | --- | --- | --- |
| [`wasapi`](https://docs.rs/wasapi/latest/wasapi/) | 0.26.0, MIT, 2026-10-11 | WASAPI only; shared and exclusive mode; event-driven buffers; device enumeration and defaults; **device-change notifications** (added, removed, default changed); render-device loopback; a `record_application` example; "the presented API is all safe Rust" | the crate's docs do not spell out process-tree loopback (`ActivateAudioInterfaceAsync` with include/exclude); **pending** E5 | one maintainer (HEnquist). Builds on the `windows` and `windows-core` crates (MIT OR Apache-2.0, memory) |
| [`cpal`](https://docs.rs/cpal/latest/cpal/) | 0.18.2, Apache-2.0, 2026-08-16 ([crates.io](https://crates.io/api/v1/crates/cpal)) | cross-platform (WASAPI, CoreAudio, ALSA, PipeWire, ASIO); enumeration, defaults, input and output streams with error callbacks; data callback on a dedicated audio thread | **no device-change or default-device-change notification; no loopback mentioned** | would also serve macOS (#755). Its abstraction would have to be bent around the two missing features |
| [`windows`](https://crates.io/crates/windows) directly | Microsoft's bindings | everything | its API is `unsafe` throughout | cannot be used under `forbid(unsafe_code)` |

**Why `wasapi`.** The helper's device rules are strict: a named device that goes away is `capture: unavailable` and is never replaced; following `default` means following the default as it changes; a switch during a transmission shuts the gate ([tui-voice.md §20.5](tui-voice.md)). Those rules need to be told when a device is removed or the default changes, which is `IMMNotificationClient` ([Microsoft Learn](https://learn.microsoft.com/en-us/windows/win32/api/mmdeviceapi/nn-mmdeviceapi-immnotificationclient)), and `wasapi` exposes it while `cpal` does not. It is also Windows-only, so it adds nothing to the Linux build and no cross-platform layer that both backends would have to fit. `cpal` stays the alternative if E3 finds `wasapi`'s buffer model or notifications wanting; choosing it would be a reasoned revision, not a fallback made in silence.

### 2.2 Shape of the Windows audio path

```text
WASAPI capture (event-driven, shared) ─► capture ring ─► transmit gate ─► LiveKit track
remote tracks ─► jitter buffers ─► mixer ─► playback ring ─► WASAPI render (event-driven, shared)
```

- **Threads.** `wasapi`'s event-driven mode wakes a thread of ours on the engine's event each period; that thread moves samples between the WASAPI buffer and the lock-free rings and does nothing else, as `dmn-audio`'s callbacks do. The transmit and mix tasks look at the rings every `RING_LOOK` as they do today; nothing in the pacing changes (`Pacing::Ring`: the sound card's clock sets the pace).
- **Format.** Everything inside is 48 kHz mono f32 in 10 ms frames. Shared-mode WASAPI gives the engine's mix format; the plan is to ask the engine to convert (`AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM` with `SRC_DEFAULT_QUALITY`; from memory, Windows 8.1 and later) so that no resampler crate is needed. If `wasapi` does not expose those flags, or the engine refuses mono, the helper downmixes and resamples itself; a resampler would be one more crate and is **pending** E3.
- **Period.** 10 ms is requested; `IAudioClient3` lets a client ask for a specific periodicity in shared mode ([About WASAPI](https://learn.microsoft.com/en-us/windows/win32/coreaudio/wasapi)). The engine may give its default. The ring targets (`TARGET_FILL`, `CAPTURE_RING_MS`) are tuned per backend, not shared constants.
- **Device identity.** A device is named by its **endpoint ID string**, which Microsoft says "uniquely identifies the device with respect to all of the other audio endpoint devices in the system" and must be treated as opaque ([IMMNotificationClient](https://learn.microsoft.com/en-us/windows/win32/api/mmdeviceapi/nn-mmdeviceapi-immnotificationclient)). Its form is `{0.0.0.00000000}.{guid}` (memory), ASCII, under sixty bytes, with no `"`, `\` or control character: it fits the rule a `set_devices` name must meet (1 to 255 bytes, one line, writable into `voice.toml` between quotes; [tui-voice.md §20.3](tui-voice.md)). So **`set_devices` carries endpoint IDs on Windows and PipeWire node names on Linux, with no change to the wire shape**; the text of §20.3 that says "a PipeWire `node.name`" becomes "the backend's stable device name" when #776 lands. The friendly name is the description. Stability across replugs is an E4 observation.
- **Device loss.** A stream error of `AUDCLNT_E_DEVICE_INVALIDATED` ("Frequently, the application can recover from this error", [About WASAPI](https://learn.microsoft.com/en-us/windows/win32/coreaudio/wasapi)) or an `OnDeviceRemoved` for the device in use is the device going away: the gate shuts with no tail, the state shows `capture: unavailable` and `no_microphone`, and nothing is substituted. `OnDefaultDeviceChanged` matters only when the side follows `default`; then the stream moves to the new default, and if a transmission is under way it ends first, as a `set_devices` to a new microphone does (§20.5). Callbacks "must be nonblocking" and must not release the final reference to an MMDevice object (same page): the notification handler posts to the session loop and returns.
- **Microphone privacy.** Windows 11's "Let desktop apps access your microphone" applies to all desktop apps at once ("Desktop apps cannot be individually toggled"), and Microsoft warns that desktop apps "might still be able to access your camera or microphone even when these settings are turned off" ([Windows camera, microphone and privacy](https://support.microsoft.com/en-us/windows/windows-camera-microphone-and-privacy-a83257bc-e990-d54a-d212-b5e41beba857)). What the helper sees when the toggle is off (an error at `Initialize`, or silence) is **pending** E4; whichever it is, the state says `no_microphone` or the start fails `audio_unavailable`, and the client's text names the setting.
- **Echo.** WASAPI has `IAcousticEchoCancellationControl`, which lets a capture client name the render endpoint as the reference ([About WASAPI](https://learn.microsoft.com/en-us/windows/win32/coreaudio/wasapi)). That is a possible answer to #186 on Windows, through the engine rather than the SDK; it is not promised, not measured, and is F10. Until measured, the Windows helper says what the Linux one says: wear headphones.

### 2.3 Standalone commands

`conch-voice join`, `devices`, `config` run on Windows with the same semantics. `conch-voice keys <device>` reads an evdev device and does not exist on Windows (§4.5 says what replaces it).

### 2.4 Toolchain cost

`wasapi` depends on the `windows` crate family, which is large to compile (memory: minutes on first build). It is behind `cfg(windows)` and costs Linux nothing. `deny.toml`'s licence allow-list already holds MIT and Apache-2.0; confirm the exact licences of the Windows-only transitive crates at E1 (`cargo deny check` with the Windows target added).

### 2.5 What stays platform-neutral

`conch-voice-audio` (gate, buffers, mixer, test sources and sinks), `conch-voice-control` (configuration, push-to-talk machine, policy), `conch-voice-api` (REST, presence, wire types), `session.rs`, `transmit.rs`, `receive.rs`, `managed.rs` apart from the channel plumbing of §3. The `MicSource`/`SpeakerSink` traits are the platform boundary and gain nothing Windows-specific.

### 2.6 One schema addition: `conch.voice_devices.v2`

`conch.voice_devices.v1` has a field `pipewire: ok | unavailable` ([pkg/schema/voice_devices.go](../../pkg/schema/voice_devices.go)). A Windows helper writing `pipewire: ok` would be lying. Proposed: `conch.voice_devices.v2`, identical except that `pipewire` is replaced by `backend` (`pipewire` or `wasapi`) and `audio` (`ok` or `unavailable`). v1 is immutable; the Linux helper may keep writing v1 or move to v2 when the TUI reads it. Golden fixtures in `pkg/schema/testdata/voice-devices-v2/` would include one Windows document with endpoint-ID names. This is a wire shape between the helper and two clients (TUI, Electron), so it lives in `pkg/schema` and the TypeScript binding is generated. Needs the owner's sign-off under the `schema-change` skill; F2.

**Open questions.** E3 (format negotiation, period, glitch-free 10 ms), E4 (device identity across replug, USB and Bluetooth, privacy toggle, default-change follow). **Follow-up:** F2, F3, F10.

## 3. The managed transport on Windows

**Decision proposed.** The owner (the Electron main process; later perhaps the Go TUI) starts `conch-voice.exe managed --events-pipe <name>` with **standard input as the command channel, exactly as today**, and **a named pipe the owner created as the events channel** in place of file descriptor 3. The helper opens the pipe by name with `std::fs::OpenOptions` (read and write), writes one attach line proving it is the process the owner started, then the hello, and the session proceeds in v1 or v2 with no change to any message. Owner loss is **EOF on standard input or EOF on the events pipe, whichever comes first**; the parent-pid watch of Linux is replaced by that second EOF.

### 3.1 Why not file descriptor 3

Node's `child_process.spawn` can open a fourth pipe (`stdio: ['pipe', 'pipe', 'pipe', 'pipe']`), and the docs say extra pipes "are not actual Unix pipes and therefore the child process can not use them by their descriptor files" ([Node child_process](https://nodejs.org/api/child_process.html)). On Windows libuv passes the handle to the child, but a Rust process has no safe way to take an inherited handle by number: `FromRawHandle::from_raw_handle` is `unsafe`. Go's `os/exec` says outright "ExtraFiles is not supported on Windows" ([os/exec](https://pkg.go.dev/os/exec#Cmd)). So fd 3 is a Linux fact, and the issue's wording ("dedicated protocol channel separate from native stdout, tightly scoped inherited handles or appropriately protected/authenticated named pipes") is right to offer the alternatives.

### 3.2 Options weighed

| Option | For | Against |
| --- | --- | --- |
| **A. Events on standard output** (two anonymous pipes, nothing else) | No name, no secret, no new shape; Node and Go both do it; Rust `std::io::stdout` is safe | The Linux design moved events off fd 1 because anything linked into the process may write there; libwebrtc is C++ and `webrtc-sys` is not ours. A stray line is a `bad_event` and the owner stops the helper: it fails closed, but a voice session is lost to a log line. Whether libwebrtc writes to stdout on Windows is **pending** E2 |
| **B. Events on an owner-created named pipe, attach proof over it** (proposed) | Keeps the Linux invariant that nothing but the protocol writer touches the channel; needs no Win32 call on either side (Node `net.createServer().listen(path)`; Rust `OpenOptions` on `\\.\pipe\...`); owner loss is an EOF the helper can read | A name in the pipe namespace is enumerable by every local user, and the default DACL grants Everyone read (§3.5), so the connection has to be authenticated: one new one-line shape |
| C. An inherited handle number in argv, `from_raw_handle` in the helper | Smallest surface | `unsafe`; refused |
| D. A localhost TCP listener | Trivially portable | Rejected by #775 ("No unauthenticated localhost listener") and by the Linux design: any process on the machine can connect, and a browser can be made to |
| E. The helper creates the pipe and the owner connects | The Rust side would hold the DACL | Creating a named pipe with a security descriptor needs Win32 (`CreateNamedPipeW` with `SECURITY_ATTRIBUTES`), `unsafe` in Rust; and Node cannot verify a server's pid either |

### 3.3 The channels

| Channel | Created by | Owner's end | Helper's end | Carries |
| --- | --- | --- | --- | --- |
| standard input | libuv (`stdio[0] = 'pipe'`): an anonymous named pipe, the child end a synchronous inherited handle ([libuv `pipe.c`](https://raw.githubusercontent.com/libuv/libuv/v1.x/src/win/pipe.c): `uv__create_stdio_pipe_pair`, client end without `FILE_FLAG_OVERLAPPED` unless asked) | writes commands | `std::io::stdin`, read with `read_bounded` as today | commands, owner → helper. EOF is owner loss |
| events pipe | the owner: `net.createServer().listen('\\\\.\\pipe\\conch-voice-' + id)`, `id` 128 random bits in hex | reads lines; writes nothing, ever | `OpenOptions::new().read(true).write(true).open(name)` (`CreateFileW` on a pipe path; standard behaviour, **to be confirmed in E2** since Rust's docs do not describe it) | events, helper → owner. The helper also **reads** it on a thread of its own: the owner never writes, so the only thing that read returns is EOF or an error, and either is owner loss |
| standard output | libuv pipe | drained and discarded | whatever the process and its libraries print | nothing of the protocol |
| standard error | libuv pipe | bounded, sanitized tail (Diagnostics, as §14 of tui-voice.md) | the logger | diagnostics |

The pipe name is passed in argv. It is not a secret: argv is readable by every process of the same user, and the pipe namespace is listable by anyone, which is exactly why it carries no authority. The owner spawns with `windowsHide: true` ("Hide the subprocess console window that would normally be created on Windows systems", [Node child_process](https://nodejs.org/api/child_process.html)) so that a console does not flash, and without `detached`.

### 3.4 Attach: proving the connection is the helper's

A random 256-bit **nonce** goes to the helper in its **environment** (`CONCH_VOICE_ATTACH`), beside `CONCH_TOKEN` when `credential: env` is used, and nowhere else: not in argv, not in `voice.toml`. The environment of a process is readable by processes of the same user and by administrators, not by other ordinary users; the same reasoning that already puts `CONCH_TOKEN` there ([tui-voice.md §9](tui-voice.md)). The helper's first line on the events pipe is:

```json
{"schema": "conch.voice_control_pipe.v1", "kind": "attached", "nonce": "<base64url, 43 characters>"}
```

and its second line is the v1 hello. The owner accepts, per pipe, **the first connection whose first line is a well-formed `attached` with the expected nonce, received within `VoiceControlHelloTimeoutMS` of the spawn**; every other connection is closed without a byte written to it, and once a connection is accepted the listener stops accepting. A squatter who connects first therefore gets nothing and costs the helper one `ERROR_PIPE_BUSY` retry at most (libuv's listener is `PIPE_UNLIMITED_INSTANCES`, so the helper's connection is not blocked by the squatter's). The helper never reuses a nonce and forgets it after writing the line.

**About the principle.** The Linux contract says no secret crosses the channel in either direction, so that a transcript can be logged and shown without a second thought ([tui-voice.md §4](tui-voice.md), [pkg/schema/voice_control.go](../../pkg/schema/voice_control.go)). The nonce is a one-time proof whose only use is spent the moment the owner reads it; a transcript that shows it reveals nothing reusable. The property that matters is kept: **no login token, join token or room name is ever on the channel.** The attach line is a new wire shape and must live in `pkg/schema` with golden fixtures and a `sensitive.json` of its own; the Go reader applies the framing rules of §3 of tui-voice.md (one object per line, 4096 bytes, unknown fields ignored). It is Windows-only in use but platform-neutral in shape, so a Linux owner that later wanted a named pipe could use it. **Needs the owner's sign-off** (schema) and a `security-reviewer` pass on this section; F4.

Why not pass the nonce on standard input before `start`: it would change the first-line rule of v1 (the first command is `start`) and put a secret-shaped value in the command transcript; the environment already carries one secret by design and the attach line is read only by the owner.

### 3.5 Who can connect, and what they get

- libuv creates the listener with `PIPE_ACCESS_DUPLEX | FILE_FLAG_OVERLAPPED | WRITE_DAC` (first instance also `FILE_FLAG_FIRST_PIPE_INSTANCE`), `PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT`, `PIPE_UNLIMITED_INSTANCES`, and **`NULL` security attributes**; `PIPE_REJECT_REMOTE_CLIENTS` is **not** set ([libuv `pipe.c`](https://raw.githubusercontent.com/libuv/libuv/v1.x/src/win/pipe.c), `pipe_alloc_accept`; the version shipped in the pinned Electron is to be re-read at #770).
- With `NULL` security, "the ACLs in the default security descriptor for a named pipe grant full control to the LocalSystem account, administrators, and the creator owner. They also grant read access to members of the Everyone group and the anonymous account" ([CreateNamedPipeW](https://learn.microsoft.com/en-us/windows/win32/api/namedpipeapi/nf-namedpipeapi-createnamedpipew)).
- `FILE_FLAG_FIRST_PIPE_INSTANCE` means a squatter who pre-creates the name makes the owner's `listen` fail with `ERROR_ACCESS_DENIED` (same page); the owner then picks a new random name. The owner never connects as a client to a pipe it did not create, so there is no server to impersonate.
- **Consequence.** Any local user, and (because remote clients are not rejected and the anonymous account has read access) possibly a remote one over SMB if the firewall and null-session policy allow it, can open the pipe and read. They read nothing: the owner writes nothing on the events pipe, and a connection that does not attach is closed. They can write: the owner reads at most one line of at most 4096 bytes from an unattached connection and closes it. The residual is a denial-of-service against the attach (connections exhausting instances) and the fact of the pipe's existence, which names the product. Both are in the review document, with the hardening that a Win32-capable owner (the Go TUI) could add later: a DACL for the user's SID only, `PIPE_REJECT_REMOTE_CLIENTS`, and `GetNamedPipeClientProcessId` compared with the spawned pid.

### 3.6 Owner loss, stop and signals

| Linux (tui-voice.md §8) | Windows | Why |
| --- | --- | --- |
| EOF on standard input | the same | the kernel closes a dead process's handles on both systems |
| parent pid changes (`std::os::unix::process::parent_id`, polled every `VoiceControlOwnerPollMS`) | **EOF or error on the events pipe**, read by a thread that only ever sees the end | `parent_id` is Unix-only in `std`; there is no safe way to wait on another process on Windows. The events pipe's server end is in the owner alone and not inheritable (libuv does not mark listeners inheritable), so a stray inherited standard-input handle in some other process cannot keep the helper alive. Whether libuv's `uv_spawn` can leak the owner's ends into Electron's many other children is **pending** E6 |
| a write to fd 3 that fails is logged (`managed.rs`, `Events`) | **a write to the events pipe that fails is owner loss** | today the writer only logs; on Windows a broken pipe on write is the same fact as EOF on read and is treated as such. Making Linux do the same is a small, separate change (F5) |
| `SIGTERM`/`SIGINT` are `leave` with reason `signal` | tokio's `signal::windows` (`ctrl_c`, `ctrl_break`, `ctrl_close`, `ctrl_shutdown`) are `Input::Signal` | with `windowsHide` there is no console, so these arrive only from a logoff or shutdown; the reason stays `signal` |
| owner's stop: `leave`, close stdin, `SIGTERM` at `StopTermAfterMS`, `SIGKILL` at `StopKillAfterMS`, reap | `leave`, close stdin **and close the events server**; at `StopKillAfterMS`, `child.kill()`; wait for `exit`. There is no graceful signal: on Windows `kill()` "terminate[s] the process forcefully and abruptly" ([Node child_process](https://nodejs.org/api/child_process.html)) | `StopTermAfterMS` has no Windows meaning and is skipped; the helper already has two reasons to leave by then |
| `Setpgid`, signals only to the `*os.Process` | no process groups; Node's `kill()` acts on the process handle libuv holds (memory), never on a pid it did not spawn and never by name | the same rule: never `taskkill`, never a process name, never another profile's helper |
| `PR_SET_PDEATHSIG` rejected | Job Objects with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` would do it ("closing the last job object handle terminates all associated processes", [Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)), but Node exposes no API for them | kept as the hardening a Win32-capable owner could add; not required for the bounds, which the two EOFs and the helper's own watchdog give |

The bounds are the contract's and are unchanged: gate shut within `VoiceControlGateShutMS` of noticing, capture stopped within `VoiceControlCaptureStopMS`, process gone within `VoiceControlOwnerLossExitMS`; measured in E6.

### 3.7 Credentials to the helper

The Electron shell holds the token (§9.1) and starts the helper with `credential: env` and `CONCH_TOKEN` in the child's environment, as the TUI does when its own token came from the environment. **`credential: stored` is not honoured by the Windows helper in the first version:** the stored-login check in the Rust client is "a regular file, not readable by others" by Unix mode bits, which has no meaning on NTFS; rather than invent an ACL check, the Windows helper reads no `credentials.json` and answers a `stored` start with `failed`, reason `not_signed_in` (literally true: it has no stored login). No new reason, no schema change. The environment is scrubbed as on Linux (`CONCH_*`, `RUST_LOG`, `RUST_BACKTRACE` removed and re-set deliberately), plus `CONCH_VOICE_ATTACH`.

**Platform-neutral.** Every message, every bound, the handshake, the ordering rules, the state machine, the owner-side `Status` and the fixtures. `managed.rs` gains a Windows channel module (open the pipe, write attach, read-for-EOF) and loses nothing.

**Open questions.** E2 (pipe open by name from safe `std`; libwebrtc's stdout behaviour), E6 (owner crash with the key held; stdin leak through Electron's other children; measured exit times). **Follow-up:** F4, F5.

## 4. Global push-to-talk

**Decision proposed.** The helper registers for **Raw Input** from the keyboard and mouse top-level collections with `RIDEV_INPUTSINK | RIDEV_DEVNOTIFY` on a message-only window of its own, and turns the device's own **transitions** into presses and releases: a keyboard `RI_KEY_MAKE` while the bound key is up is a press, `RI_KEY_BREAK` is a release; `RI_MOUSE_BUTTON_4_DOWN`/`_UP` and `RI_MOUSE_BUTTON_5_DOWN`/`_UP` likewise. A `MAKE` while the key is already down (auto-repeat) is nothing. These feed the existing push-to-talk machine in `conch-voice-control` through the same `Input::Key` the evdev watcher uses; nothing downstream knows the operating system.

### 4.1 What the documentation establishes

- "An application can receive data when it is in the foreground and when it is in the background (if registered with `RIDEV_INPUTSINK`)" ([Raw Input Overview](https://learn.microsoft.com/en-us/windows/win32/inputdev/about-raw-input), page dated 2025-07-14). Registration needs a window (`hwndTarget`); a message-only window will do.
- Mouse buttons are reported as transitions: `RI_MOUSE_BUTTON_4_DOWN` 0x0040 "XBUTTON1 changed to down", `RI_MOUSE_BUTTON_4_UP` 0x0080, `RI_MOUSE_BUTTON_5_DOWN` 0x0100, `RI_MOUSE_BUTTON_5_UP` 0x0200 ([RAWMOUSE](https://learn.microsoft.com/en-us/windows/win32/api/winuser/ns-winuser-rawmouse)). Mouse 4 and 5 are therefore first-class, with their own up events; this is what the issue asks for.
- Keyboard events carry `MakeCode` (the scan code), `Flags` with `RI_KEY_MAKE` 0 ("The key is down") and `RI_KEY_BREAK` 1 ("The key is up"), the `E0`/`E1` prefixes, and `VKey` ([RAWKEYBOARD](https://learn.microsoft.com/en-us/windows/win32/api/winuser/ns-winuser-rawkeyboard)). Bindings are by scan code with its prefix, not by virtual key, so a layout change does not move the talk key (the Linux client binds by `KEY_*` code for the same reason).
- Microsoft itself steers away from hooks: "In most cases where the application needs to use low level hooks, it should monitor raw input instead" ([LowLevelKeyboardProc](https://learn.microsoft.com/en-us/windows/win32/winmsg/lowlevelkeyboardproc)).

### 4.2 Options weighed

| Backend | Press and release | Background | Why not |
| --- | --- | --- | --- |
| **Raw Input** (proposed) | yes, from device transitions | yes (`RIDEV_INPUTSINK`) | — |
| `WH_KEYBOARD_LL`/`WH_MOUSE_LL` hooks | yes | yes | "If the hook procedure times out ... on Windows 7 and later, the hook is silently removed without being called. There is no way for the application to know whether the hook is removed"; the cap is 1000 ms ([LowLevelKeyboardProc](https://learn.microsoft.com/en-us/windows/win32/winmsg/lowlevelkeyboardproc)). A removed hook is a release never seen. Global hooks are "a shared resource" to be "restricted to special-purpose applications" ([SetWindowsHookExW](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-setwindowshookexw)); hooks are also what anti-cheat software looks for (memory, §5) |
| Electron `globalShortcut` / `RegisterHotKey` | activation only | yes | no release; swallows the key from the game. ADR-007 already says "Keyboard shortcuts alone are insufficient proof" |
| polling `GetAsyncKeyState` | by sampling | yes | misses presses shorter than the poll, costs a wake per poll, and is Win32 FFI anyway |
| a kernel driver, injection into the game | — | — | excluded by #756 ("No driver/injection/anti-cheat bypass") |

### 4.3 The dependency question

Raw Input is Win32: `RegisterRawInputDevices`, a window class, a message loop, `GetRawInputData`. Under `forbid(unsafe_code)` the helper cannot call them. Three routes, in the order to try them:

1. **[`winit`](https://docs.rs/winit/latest/winit/) 0.30.13** (Apache-2.0, published 2026-10-24; [crates.io](https://crates.io/api/v1/crates/winit)). Its Windows backend registers exactly `RIDEV_DEVNOTIFY | RIDEV_INPUTSINK` for `DeviceEvents::Always` ("receive events even if we're not in the foreground"), on both the mouse and keyboard collections, and maps `RI_MOUSE_BUTTON_4_DOWN`/`UP` to a pressed/released state for button index 3 ([`raw_input.rs` at v0.30.13](https://raw.githubusercontent.com/rust-windowing/winit/v0.30.13/src/platform_impl/windows/raw_input.rs)); `DeviceEvent::Key` and `DeviceEvent::Button` are "delivered regardless of input focus" ([DeviceEvent](https://docs.rs/winit/latest/winit/event/enum.DeviceEvent.html)). `EventLoopBuilderExtWindows::with_any_thread` lets the loop run off the main thread, which the helper needs because its main thread is tokio's ([EventLoopBuilderExtWindows](https://docs.rs/winit/0.30.13/winit/platform/windows/trait.EventLoopBuilderExtWindows.html)). **Against:** it is a windowing framework in an audio helper, with a large dependency tree that `cargo deny` must clear; 0.31 is in beta and may change `DeviceEvent`; its `MAKE`/`BREAK` to `ElementState` mapping lives in a file not read here (the raw-input file does not read the break flag), and must be checked in E7 before trust. Try it first, `default-features = false`, `cfg(windows)` only.
2. A smaller maintained crate exposing Raw Input safely. None was evaluated here (crates such as `multiinput` exist but were **not checked** for maintenance or licence); E7 includes a short survey before settling on `winit`.
3. **A reviewed policy change:** one crate, `conch-voice-input-windows`, allowed `unsafe` only in a leaf module that binds `windows-sys` for the dozen calls needed, reviewed line by line, with `forbid(unsafe_code)` kept everywhere else. This needs an amendment to ADR-006 and CLAUDE.md rule 6 and is the fallback, not the plan; #775 says "Do not move code to another package/repository merely to evade that rule", and this route does not: it names the exception and asks for it.

Each route is a new dependency or a policy change and **needs Nick's sign-off**; F6.

### 4.4 Fail-closed rules on Windows

Every rule of [conch-voice.md §3 and §4](conch-voice.md) applies. The Windows specifics:

- **Repeats never hold or reopen the gate.** The machine ignores a `MAKE` for a key it already holds; only `BREAK` releases. Same as ignoring evdev value 2.
- **Device removal is a release.** `RIDEV_DEVNOTIFY` delivers `WM_INPUT_DEVICE_CHANGE` with `GIDC_REMOVAL` ([Raw Input Overview](https://learn.microsoft.com/en-us/windows/win32/inputdev/about-raw-input) describes device-change delivery; the constant is from memory). A removal while a key is held on that device shuts the gate as `key_device_lost` does on Linux; the state shows `ptt: lost` until a device is present again, and every press is refused meanwhile. Unlike Linux, there is no single configured device: the binding applies to every keyboard (or mouse), so `lost` means no device of that class is attached.
- **Missed releases.** A release can be missed when input goes to another desktop (the UAC prompt, the lock screen, Ctrl+Alt+Del) or when a higher-integrity window takes the foreground, if Raw Input is not delivered there (§5). Three things bound the damage: `max_transmit_secs` (default 120, at most 600) ends any single press; the owner sends `set_mute true` on `powerMonitor`'s `lock-screen` and `leave` on `suspend`, both of which Electron documents for Windows ([powerMonitor](https://www.electronjs.org/docs/latest/api/power-monitor)); and the state carries `transmitting: true` the whole time, so the tray and window show it. Whether a `BREAK` that happened on the secure desktop is delivered afterwards is E8; if it is not, the helper's own observation of a resync is impossible from safe code (`GetAsyncKeyState` is FFI), and the three bounds above are the answer. #773's rule holds: after lock or suspend, transmission needs a fresh press and, after `set_mute`, a deliberate unmute.
- **Focus changes alone do nothing.** Holding the key across Alt-Tab keeps transmitting; a focus change neither opens nor shuts the gate. Raw Input is not affected by which window is foreground (above), which is the point.
- **Nothing but the bound transitions leaves the input module.** As on Linux: the module yields `KeyEvent` (talk, mute or deafen went down or up) and `DeviceState`, never a scan code, never a count, never a time; Raw Input delivers the whole keyboard stream to the registered window, so the same keylogger-shaped warning the Linux README gives applies, in a milder form (no exclusive grab, no cross-session reach: Raw Input is per desktop session).

### 4.5 Configuration

`voice.toml` `[keys]` on Windows names bindings by scan code and by mouse button, not by device path: `talk = "sc:0x29"` (grave), `talk = "mouse:4"`, with the `E0` prefix as `sc:0xE0,0x1D`. The exact grammar is #777's; the rule is that it is a closed, parseable form that `conch-voice config set` writes and that never names a path. **Binding capture for the UI** happens in the Electron window during configuration, which has focus then: the renderer reads `KeyboardEvent.code` and `MouseEvent.button` (DOM button 3 and 4 are Mouse 4 and 5) and the shell translates to the `[keys]` form and writes it with `config set`. The helper is not involved and no raw input leaves it. The `conch-voice keys` terminal command is Linux-only; a Windows equivalent that prints scan codes to a terminal may be added for diagnostics but is not needed for the product. **Rebinding during a session** restarts the helper (`leave`, `join`) in the first version; a `set_keys` command would be a `conch.voice_control.v3` matter and is not proposed now (§6).

**Platform-neutral.** The push-to-talk machine, the `KeyEvent`/`DeviceState` vocabulary, `ptt`/`ptt_problem` in the state, the release tail and `max_transmit_secs`.

**Open questions.** E7 (winit or alternative: press and release of a key and of Mouse 4 in the background, in a windowed, borderless and exclusive-fullscreen game, with a second listener confirming; repeats; device unplug while held), E8 (secure desktop and elevated foreground), E9 (anti-cheat titles). **Follow-up:** F6, F7.

## 5. Privilege and security boundaries

**Decision proposed.** The app and the helper run as the signed-in user at medium integrity, with no driver, no service, no hook, no injection, no elevation prompt and no instruction to disable protection. Where Windows does not deliver input to a medium-integrity process, the client says so and does not work around it.

- **Elevated foreground.** Windows' integrity mechanism gives "elevated users ... high" and standard processes medium ([Mandatory Integrity Control](https://learn.microsoft.com/en-us/windows/win32/secauthz/mandatory-integrity-control)). User Interface Privilege Isolation blocks lower-integrity processes from sending messages to and hooking higher-integrity windows (memory; the MIC page read for this note does not spell the UIPI list out). **Whether Raw Input registered with `RIDEV_INPUTSINK` is delivered while an elevated game is foreground is not stated on the pages read, and is E8.** If it is not, the capability matrix marks "PTT with an elevated game in the foreground" **unsupported** and the client's diagnostics say "this game runs as administrator; Conch does not, and cannot see your key while it has focus." Running Conch elevated is not offered.
- **Secure desktop.** The UAC prompt, the lock screen and Ctrl+Alt+Del switch to a desktop where ordinary applications have no windows; no input reaches the helper while it is shown. A press that began before the switch may have its release there (§4.4). Lock is observed by the owner through `powerMonitor`'s `lock-screen` ([powerMonitor](https://www.electronjs.org/docs/latest/api/power-monitor)); the helper itself would need `WTSRegisterSessionNotification` and `WM_WTSSESSION_CHANGE` ([Microsoft Learn](https://learn.microsoft.com/en-us/windows/win32/api/wtsapi32/nf-wtsapi32-wtsregistersessionnotification)), which is Win32 and is only reachable through whichever input dependency §4.3 settles on.
- **Anti-cheat.** Raw Input registration is a passive, documented, user-mode API with no hook, no DLL and no handle into the game. That is the strongest statement that can be made from documentation. Whether a given anti-cheat system tolerates a background raw-input listener is a property of that system and that title, is not documented by vendors, and is **E9 per title**, recorded with versions; the matrix has one row per tested title and no general claim. Hooks (§4.2) would have made this worse, which is one more reason they were not chosen.
- **Microphone privacy toggle.** §2.2.
- **No listener, no secrets in argv.** §3.3–§3.5.
- **The renderer.** Context isolation, sandbox, no Node integration, validated IPC: #770's rules, unchanged on Windows; W5 verifies them on Windows (#779). The helper is started only from the main process.

**Open questions.** E8, E9. **Follow-up:** none beyond W3/W6 acceptance.

## 6. Where input lives

**Decision proposed.** **Input lives in the helper**, on Windows as on Linux. No command of `conch.voice_control.v1` or `v2` presses or releases, and none is added: the rule "the gate is opened by a press and by nothing else; nothing on this channel can open the transmit gate" ([pkg/schema/voice_control.go](../../pkg/schema/voice_control.go); [conch-voice.md §3](conch-voice.md)) stands. Bindings are configuration (`voice.toml [keys]`, written by `config set`), not protocol. **No versioned control extension is needed for input in the proposed design.**

Why this is the right place: the gate is in the helper, next to the capture ring; a press that travelled from another process would add a hop and a channel that could be driven by anything that can write to it. The Linux design chose this for the same reason, and the Windows backend changes only where the press comes from.

When a control extension **would** be needed, so that nobody adds it by accident:

- if #769 chooses model C of §7 (Chromium owns the microphone, the helper is reduced to an input source), presses must cross a channel to the shell. That is a **new** versioned shape, `conch.voice_input.v1` (helper → shell, `pressed`/`released` with no key identity), not a repurposed v1/v2 command, and the gate it drives is the browser one (`web/src/voice/gate.ts`) with its own fail-closed rules;
- live rebinding (`set_keys`) or reporting which binding is in force would be `conch.voice_control.v3`, with the hello negotiation of §20.2 of tui-voice.md. Not proposed now (§4.5).

The desktop shell needs no helper change to show binding state: `ptt` (`ready`, `not_configured`, `lost`, `unknown`) and `ptt_problem` are already in every state.

## 7. Native voice plus screen capture, with #769

**Decision proposed, for #769 to confirm:** **Model B.** The Rust helper is the person's one LiveKit participant for voice (publishes the microphone, plays remote voice). Screen video is captured and published by Electron (Chromium's `desktopCapturer` and the LiveKit JS SDK) as a **companion participant that conchd sanctions**: a second, short-lived grant tied to the voice session, with `can_publish` for screen video (and the scoped audio of §8) only, `can_subscribe` for video only, hidden from occupancy and presence by conchd, and ended when the voice session ends. One microphone publisher, one voice playback path, no duplicate-identity bypass: the companion is a different LiveKit identity that conchd issues and recognises as the same person.

### 7.1 Why a decision is needed at all

Today one person is one LiveKit participant. The helper connects as that participant (`duplicate_identity` is the failure when something else already holds it: `event-state-failed-duplicate-identity.json`), and the browser connects as that participant. A desktop app whose voice is in the helper and whose screen is in Chromium would be two connections for one person, and LiveKit's rule for a reused identity is to drop the earlier connection (memory of LiveKit behaviour; conchd's own check refuses the second). Watching a share also needs a participant, so the problem is not only publishing.

### 7.2 Options

| Model | Voice | Screen | Participants | Cost | Verdict |
| --- | --- | --- | --- | --- | --- |
| **A. Everything in the helper** | helper | helper publishes screen video from frames the shell captures, and plays received video into... nothing it can show | one | the helper would decode remote video and hand frames to the shell over a pipe; libwebrtc's own desktop capturer would be a second capture stack to validate on Windows (`pipewire_stubs.o` was stripped on Linux for a reason). Months, not days | no |
| **B. Helper for voice, Electron companion for screen** (proposed) | helper | Electron, LiveKit JS, as a companion participant conchd issues | two, one hidden | conchd: a companion grant (identity derivation, hidden from presence/occupancy, torn down with the parent session, share claims resolve to the person); web: the viewer's share lookup must find the sharer's companion. Server work is one issue with its own review; no change to the published voice-control schemas | yes, if #769 agrees |
| C. Everything in Chromium, helper as input only | Chromium (`getUserMedia`, LiveKit JS), with Chromium's echo cancellation and noise suppression | Chromium | one | a new `conch.voice_input.v1` channel (§6); the gate is the browser gate; the native capture path, the native mixer and ADR-006's rationale for low-latency capture are set aside on desktop; #186 (echo) is solved by Chromium for free | a real alternative; it changes what the helper is for, and the epic's table says native audio through the helper. #769 should weigh it with eyes open |
| D. Phase one without screen on the desktop | helper | none; a browser tab for shares | one | nothing new | the honest fallback if B's conchd work is not yet done when W2/W3 are: text and voice ship, screen waits for W4 |

### 7.3 What model B asks of others

- **conchd (follow-up F8):** a `POST /v1/voice/session/companion` (or a field on the existing session request) that, given a live voice session, issues a second join grant with identity `<principal>:screen`-shaped (exact scheme to be decided with the voice-control-plane design), video-only permissions, the same room, the same lifetime; presence and occupancy fold the companion into the person; share claims, stops and revocation apply to the person and so to both; the poller treats a companion without a parent as stale and removes it. API parity holds: it is a REST route like the others, usable by any client.
- **web (`web/src/voice/`):** the viewer resolves a share to a participant through conchd's presence (`sharing` principal) rather than assuming the sharer's own identity publishes it.
- **Linux desktop (#769):** the same model; nothing here is Windows-specific. That is why #769 decides and this note proposes.

**Platform-neutral.** All of §7. **Open questions.** Model B vs C is the one architecture-blocking question of W1 beyond the dependency choices; E10 (two connections for one person against the current conchd and LiveKit, as #769 also plans to test). **Follow-up:** F8.

## 8. Application audio vs loopback

**Decision proposed.** Three audio scopes for a share, each named truthfully, **off by default**:

| Scope | Mechanism | Rebroadcasts the call? | Status |
| --- | --- | --- | --- |
| none | — | no | default |
| **this application** | process-tree loopback: `ActivateAudioInterfaceAsync` with `AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK`, include the picked window's process tree; "only audio from the specified process, and its children, will be captured"; "requires Windows 10 build 20348 or later" ([ApplicationLoopback sample](https://learn.microsoft.com/en-us/samples/microsoft/windows-classic-samples/applicationloopbackaudio-sample/), page dated 2026-02-19). Natively, through the `wasapi` crate if E5 confirms it exposes the include/exclude modes | no: the helper's playback is not in that tree | pending E5 |
| **everything but the call** | the same API with the **exclude** mode on our own process tree (the helper and the Electron tree): "capturing all system audio except those from the specified process (and its children)" (same page) | no, by construction | pending E5 |
| whole output through Chromium | Electron's `setDisplayMediaRequestHandler` with `audio: 'loopback'` or `'loopbackWithMute'`: "will capture system audio, and is currently only supported on Windows" ([session](https://www.electronjs.org/docs/latest/api/session)); `loopbackWithMute` is listed and not defined on that page (memory: it mutes local playback of what is captured) | **yes**: endpoint loopback captures the helper's playback, so the room hears itself | **not offered** as "system audio"; at most an explicit "share everything including this call's voices" with a warning, if W4 wants it |

Per-window audio in Chromium's `getDisplayMedia` (the `windowAudio` hint) was a specification proposal in June 2025 ([W3C webapps list](https://lists.w3.org/Archives/Public/public-webapps-github/2025Jun/0114.html); [Screen Capture draft](https://www.w3.org/TR/2026/WD-screen-capture-20260716/)); whether the pinned Chromium ships it was not established here and is E5's last item. If it does, it is the simplest route for "this application" under model B, because the companion already publishes the video.

Under model B, scoped audio captured natively by the helper would be a second audio track that has to reach the same share the companion publishes; that plumbing (helper track to the companion's participant, or the helper publishing a `screen_share_audio` track on its own participant that viewers associate through presence) is W4's design and not decided here. **Feedback avoidance** is structural, not a filter: only the two process-loopback modes are offered as application or system audio, and both exclude the call's own playback. **Unsupported fallback:** when process loopback is unavailable (older build, the API refusing, the picked source having no audio session: "the capturing process receives silence", same sample page), the picker says "sound from this application is not available here" and shares the picture; it never widens to the whole output on its own. Source closure, minimised windows, exclusive fullscreen, protected content, multi-monitor and DPI changes are capture-side facts for W4 to record per configuration (E11), with "unsupported" as an allowed answer.

**Platform-neutral.** The three-scope vocabulary and the off-by-default rule, which Linux (PipeWire application streams) and macOS can implement by their own means. **Open questions.** E5, E11. **Follow-up:** W4 itself (#778); F9 if a schema is needed for the audio scope in share claims.

## 9. Credentials, lifecycle, installer, signing and updates

### 9.1 Credentials

**Decision proposed.** The shell keeps each server profile's token with Electron's `safeStorage`, whose Windows keys "are generated via DPAPI" and protect content from other users "but not from other apps running in the same userspace" ([safeStorage](https://www.electronjs.org/docs/latest/api/safe-storage)). That is stated to the user in those words. The asynchronous API is used (`isAsyncEncryptionAvailable()`; the synchronous API "will be removed in Electron 46"). If encryption is unavailable, the profile offers **session-only login** and says why; nothing plaintext-equivalent is persisted. The token reaches the helper as `CONCH_TOKEN` with `credential: env` (§3.7) and is never in argv, logs or the renderer's storage. Per-profile isolation, logout clearing, late-response fencing and no credential transfer on redirect are #770's rules; W5 (#779) verifies them on Windows.

### 9.2 Lifecycle

Close quits and stops media by default; close-to-tray is explicit opt-in with a visible, truthful tray state and a Quit; no hidden session if the tray is unavailable (#773). On `lock-screen` the owner sends `set_mute true`; on `suspend`, `leave`; on `resume`/`unlock-screen` nothing joins or unmutes by itself ([powerMonitor](https://www.electronjs.org/docs/latest/api/power-monitor): both events on Windows). Startup launch is off by default and never joins voice. Single instance through `app.requestSingleInstanceLock()`, with the lock scoped so that an isolated test instance (a different `userData`) does not collide ([launch from URL](https://www.electronjs.org/docs/latest/tutorial/launch-app-from-url-in-another-app)). Invite links: `app.setAsDefaultProtocolClient` for the documented scheme, the URL arriving as the last element of `commandLine` in `second-instance` or in `process.argv` on a cold start (same page); it is parsed strictly, shows the destination server, requires a click to join, never joins voice, and is never logged whole. A stable `AppUserModelID` set with `app.setAppUserModelId` and matched by the installer's shortcut, which Windows notifications and taskbar pinning require ([notifications](https://www.electronjs.org/docs/latest/tutorial/notifications); [autoUpdater](https://www.electronjs.org/docs/latest/api/auto-updater)).

### 9.3 Installer

**Decision proposed.** One **per-user installer** that needs no administrator, built by **electron-builder's NSIS target** (assisted, not one-click; per-user; no elevation), bundling `conch-voice.exe` beside the app. **From memory, not re-verified** (the electron-builder site returned nothing to this session twice): its NSIS defaults are `oneClick: true` and `perMachine: false`, so the per-user mode is the default and `oneClick` is what to turn off. Confirmed at #780 before anything is pinned.

| Option | For | Against |
| --- | --- | --- |
| **electron-builder, NSIS** (proposed) | conventional installer and uninstaller; per-user without admin; install directory choosable; widely used; MIT | a second toolchain's opinions; later automatic updates would use `electron-updater`, not Electron's built-in `autoUpdater` |
| Electron Forge, Squirrel.Windows | Electron's own recommended path for signing Squirrel installers ([code signing](https://www.electronjs.org/docs/latest/tutorial/code-signing)); "no-prompt, no-hassle, no-admin"; creates the shortcut with the `AppUserModelID`; Electron's `autoUpdater` works with it later ([Forge maker](https://www.electronforge.io/config/makers/squirrel.windows); [autoUpdater](https://www.electronjs.org/docs/latest/api/auto-updater)) | no install-directory choice; the app runs during install, update and uninstall and must handle Squirrel's startup events; package names cannot contain spaces; builds on Windows or Linux-with-mono-and-wine |
| Forge, WiX MSI | enterprise-friendly | per-machine by nature; admin |
| MSIX | Store and sideload; its own updater | deferred by #756 |

Either Electron tooling route is a **new npm dev dependency set** (Nick's sign-off, `deps-allowlist.txt` for the desktop workspace, licences checked); F11.

### 9.4 Signing

Windows "requires software to be signed with an 'extended validation' certificate" for the warnings to go away, EV keys must live in hardware or a cloud signer, and **Azure Artifact Signing** (renamed from Trusted Signing) is "the cheapest option" but "currently limited to developers in certain countries" ([Electron code signing](https://www.electronjs.org/docs/latest/tutorial/code-signing); [Artifact Signing overview](https://learn.microsoft.com/en-us/azure/artifact-signing/overview), which describes Basic and Premium SKUs and identity validation but states no eligibility or pricing on that page). Electron's page says Artifact Signing "gets rid of SmartScreen warnings"; Microsoft's own overview makes no SmartScreen statement, and #756 is right that signing is not a reputation guarantee. **For the owner to decide:** which signer (Artifact Signing, if eligible as an individual in his country; else an EV certificate with a cloud HSM such as DigiCert KeyLocker), its yearly cost, and the account it lives in. Everything shipped is signed: installer, uninstaller, the app executable, and `conch-voice.exe`. Unsigned private previews are labelled as such and never come with instructions to disable SmartScreen or antivirus.

### 9.5 Updates

First version: **manual, verified.** The app checks the release feed, shows the new version, and opens the signed installer's download in the browser; it verifies nothing weaker than the signature and a published SHA-256; it never restarts during a call; the helper's hello and the shell's version check guard a mismatch. Automatic updates are deferred by #756; when wanted, the choice of §9.3 decides the mechanism.

**Platform-neutral.** Profiles, isolation, lifecycle states, invite-link rules, version display: #770/#773. **Open questions.** E12 (clean-machine install, upgrade, uninstall, two profiles), the owner's signing decision. **Follow-up:** F11, F12.

## 10. What stays unchanged on Linux

- `dmn-audio`, evdev key reading, fd 3, process groups, `SIGTERM`: all kept, all behind `cfg(target_os = "linux")` where a Windows counterpart exists.
- `voice/Cargo.toml`: `unsafe_code = "forbid"` on every crate; `unwrap_used`/`expect_used` denied.
- Every golden fixture under `pkg/schema/testdata/voice-control*` byte-identical; `scripts/schema-compat.sh` unchanged in what it checks.
- `make check`'s Rust leg runs on Linux as today; Windows builds and tests are a separate CI job on a Windows runner (F1), never a gate on Linux work.
- `conch` (the TUI) does not gain Windows voice in this epic; the Windows transport does not preclude it (a Go owner can create a named pipe through `golang.org/x/sys/windows`, already an indirect module, and could add the DACL and client-pid check of §3.5).

## 11. Proposed amendment to ADR-006 (not an edit; for the owner)

> **Platform (amended):** Linux with PipeWire, through `dmn-audio`; and Windows 11 x64 with WASAPI, through the `wasapi` crate, and Raw Input through [the crate §4.3 settles on], each behind `cfg(target_os)`. The audio and key-source layers are the only platform-specific code; the gate, mixer, session loop and control protocol are shared. Other platforms still need a decision of their own.
>
> **Dependencies (amended):** the allowlist gains `wasapi` and [the input crate], Windows-only. `scripts/voice-pins.sh` pins the Windows libwebrtc archive by hash as it pins the Linux one. `deny.toml` checks both targets.
>
> **Consequence added:** CI gains a Windows job for `voice/`; the Linux leg of `make check` is unchanged.

If route 3 of §4.3 is taken, the amendment also names the one crate allowed `unsafe` and the review rule for it, and CLAUDE.md rule 6 changes with it. Written here as text for Nick to accept, reject or edit; ADR-006 itself is untouched by this PR.

## 12. Dependencies and decisions needing the owner's sign-off

| Item | Kind | Section |
| --- | --- | --- |
| `wasapi` crate (MIT), Windows-only | new Rust dependency, `voice/deps-allowlist.txt`, `deny.toml` target | §2 |
| `winit` (Apache-2.0) **or** another Raw Input crate **or** a reviewed `unsafe` leaf crate (policy change, ADR-006 and CLAUDE.md) | new Rust dependency or policy | §4.3 |
| A resampler crate, only if WASAPI's own conversion cannot be used | new Rust dependency, conditional | §2.2 |
| `scripts/voice-pins.sh`: Windows libwebrtc hash; `voice-toolchain.sh`: second triple | pin change ("needs the same care as a dependency change") | §1 |
| Electron (pinned version) and electron-builder or Forge, with their licences | new npm dependencies in `desktop/` | §1, §9.3 |
| `conch.voice_control_pipe.v1` | new schema, fixtures, Go reader, Rust writer, TS binding | §3.4 |
| `conch.voice_devices.v2` | new schema version | §2.6 |
| Media ownership model B (and the conchd companion grant that follows) | design decision with #769 | §7 |
| Signing account and certificate | purchase and account | §9.4 |
| ADR-006 amendment | ADR | §11 |

## 13. Traceability: #775 acceptance criteria to sections and matrix rows

Row ids are those of [docs/reviews/windows-desktop-capabilities.md](../reviews/windows-desktop-capabilities.md).

| #775 acceptance criterion | Section | Matrix rows | Experiments |
| --- | --- | --- | --- |
| Windows 11 x64 target; builds; Electron/Chromium, Rust/MSVC/SDK, LiveKit/libwebrtc pins; Win10/ARM64/WSL out | §1 | T1–T4 | E1 |
| Capture/playback backend; WASAPI through a safe abstraction; neutral gating kept; Linux kept; no `unsafe` | §2, §10 | A1–A7 | E3, E4 |
| Windows-safe transport: separate channel, scoped handles or authenticated pipes, owner death, ownership, bounded shutdown, no listener, no secrets in argv | §3 | X1–X7 | E2, E6 |
| Global keyboard and Mouse 4 press AND release; Raw Input or documented alternative; foreground/background/fullscreen; no activation-only, no repeats | §4 | P1–P12 | E7 |
| Ordinary user; elevated and secure desktop; conflicts; hot-unplug; lock/suspend; missed release; no admin/driver/injection/anti-cheat bypass | §5, §4.4 | P5, P6, P9–P12, L1–L4, G1 | E8, E9 |
| Input in the helper vs versioned control extension; no silent v1/v2 change | §6 | X8 | — |
| Native voice plus Electron screen ownership with #769; one mic publisher, one playback; identity policy | §7 | M1–M3 | E10 |
| Application audio vs loopback; opt-in; feedback avoidance; unsupported fallback; permissions, closure, GPU/fullscreen limits | §8 | S1–S6 | E5, E11 |
| Credential, lifecycle, installer/signing/update adapters on #770/#773; per-user x64 installer; one tool | §9 | C1–C3, L1–L6, D1–D5 | E12 |
| Reproducible matrix and security review; accepted contracts; concrete follow-ups; owner approval for deps/licences/schema/ADR | §12–§14, the review document | all | all |

## 14. Follow-up issues (for the principal to file)

| Id | Title | Scope (one line) | Blocks |
| --- | --- | --- | --- |
| F1 | Windows CI job for `voice/` | `windows-latest` runner, MSVC, pinned libwebrtc `win-x64` by hash, `cargo build/test/deny` with the Windows target; records E1's compiler finding | #776 |
| F2 | `conch.voice_devices.v2`: backend-neutral device list | schema, fixtures, Go reader, TS binding; Rust writer on both backends | #776 (device UI) |
| F3 | `conch-voice-audio`/`audio.rs` split into backends | move the PipeWire implementation under `cfg(linux)` with no behaviour change, so #776 adds a sibling | #776 |
| F4 | `conch.voice_control_pipe.v1`: attach line for a named-pipe events channel | schema, fixtures, `sensitive.json`, Go reader (for a future Go owner), Rust writer; security-reviewer pass | #776 |
| F5 | Events-write failure is owner loss | `managed.rs`: a failed write on the events channel arms the owner-loss path on every platform; test with a closed fd 3 | #776 (small, independent) |
| F6 | Raw Input dependency decision | E7's survey and spike result; one of `winit`, another crate, or the `unsafe` leaf crate with ADR-006/CLAUDE.md amendment; Nick signs | #777 |
| F7 | Windows key bindings in `voice.toml` and `config set` | the `sc:`/`mouse:` grammar, `KeyBindings` parsing under `cfg(windows)`, shell-side binding capture spec | #777 |
| F8 | conchd: companion participant for screen media | route, grant, identity scheme, presence/occupancy folding, teardown with the parent session; e2e against LiveKit; decided with #769 | #778, Linux D4 |
| F9 | Share audio scope on the wire | only if W4 needs the scope (`none`/`application`/`system_excluding_call`) in share claims or presence | #778 |
| F10 | Measure WASAPI acoustic echo cancellation | `IAcousticEchoCancellationControl` as a Windows answer to #186; measure before offering | none (research) |
| F11 | `desktop/` Windows packaging: electron-builder NSIS per-user | tool choice confirmed against its docs, allowlist entries, licences, bundling `conch-voice.exe`, `AppUserModelID`, URI handler registration | #780 |
| F12 | Signing decision record | signer options, eligibility, yearly cost, account ownership; for Nick | #780 |
| F13 | Windows helper refuses `credential: stored` | documented `not_signed_in` on Windows until an ACL-based stored-login check exists; test | #776 |
| F14 | Hardware run of the W1 experiments | E1–E12 on a Windows 11 x64 machine, filling the matrix; the other half of this issue | #776–#780 acceptance |

## 15. Sources

All checked 2026-10-28 from this session unless noted.

Microsoft Learn: [Raw Input Overview](https://learn.microsoft.com/en-us/windows/win32/inputdev/about-raw-input) (2025-07-14) · [RAWMOUSE](https://learn.microsoft.com/en-us/windows/win32/api/winuser/ns-winuser-rawmouse) · [RAWKEYBOARD](https://learn.microsoft.com/en-us/windows/win32/api/winuser/ns-winuser-rawkeyboard) · [SetWindowsHookExW](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-setwindowshookexw) · [LowLevelKeyboardProc](https://learn.microsoft.com/en-us/windows/win32/winmsg/lowlevelkeyboardproc) · [Mandatory Integrity Control](https://learn.microsoft.com/en-us/windows/win32/secauthz/mandatory-integrity-control) · [About WASAPI](https://learn.microsoft.com/en-us/windows/win32/coreaudio/wasapi) · [IMMNotificationClient](https://learn.microsoft.com/en-us/windows/win32/api/mmdeviceapi/nn-mmdeviceapi-immnotificationclient) · [Application loopback audio capture sample](https://learn.microsoft.com/en-us/samples/microsoft/windows-classic-samples/applicationloopbackaudio-sample/) (2026-02-19) · [CreateNamedPipeW](https://learn.microsoft.com/en-us/windows/win32/api/namedpipeapi/nf-namedpipeapi-createnamedpipew) · [UpdateProcThreadAttribute](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute) · [Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects) · [WTSRegisterSessionNotification](https://learn.microsoft.com/en-us/windows/win32/api/wtsapi32/nf-wtsapi32-wtsregistersessionnotification) · [Windows 11 release information](https://learn.microsoft.com/en-us/windows/release-health/windows11-release-information) (2026-10-26) · [Artifact Signing overview](https://learn.microsoft.com/en-us/azure/artifact-signing/overview) · [Windows camera, microphone and privacy](https://support.microsoft.com/en-us/windows/windows-camera-microphone-and-privacy-a83257bc-e990-d54a-d212-b5e41beba857).

Electron: [safeStorage](https://www.electronjs.org/docs/latest/api/safe-storage) · [code signing](https://www.electronjs.org/docs/latest/tutorial/code-signing) · [session (setDisplayMediaRequestHandler)](https://www.electronjs.org/docs/latest/api/session) · [autoUpdater](https://www.electronjs.org/docs/latest/api/auto-updater) · [powerMonitor](https://www.electronjs.org/docs/latest/api/power-monitor) · [notifications](https://www.electronjs.org/docs/latest/tutorial/notifications) · [launch app from URL](https://www.electronjs.org/docs/latest/tutorial/launch-app-from-url-in-another-app) · [timelines](https://www.electronjs.org/docs/latest/tutorial/electron-timelines) · [releases.json](https://releases.electronjs.org/releases.json) · [Forge Squirrel.Windows maker](https://www.electronforge.io/config/makers/squirrel.windows).

Node and libuv: [child_process](https://nodejs.org/api/child_process.html) · [libuv `src/win/pipe.c` (v1.x)](https://raw.githubusercontent.com/libuv/libuv/v1.x/src/win/pipe.c).

Rust and crates: [platform support](https://doc.rust-lang.org/rustc/platform-support.html) · [`wasapi` 0.26.0](https://docs.rs/wasapi/latest/wasapi/) · [`cpal` 0.18.2](https://docs.rs/cpal/latest/cpal/), [crates.io](https://crates.io/api/v1/crates/cpal) · [`winit` 0.30.13 DeviceEvent](https://docs.rs/winit/latest/winit/event/enum.DeviceEvent.html), [EventLoopBuilderExtWindows](https://docs.rs/winit/0.30.13/winit/platform/windows/trait.EventLoopBuilderExtWindows.html), [`raw_input.rs`](https://raw.githubusercontent.com/rust-windowing/winit/v0.30.13/src/platform_impl/windows/raw_input.rs), [crates.io](https://crates.io/api/v1/crates/winit).

LiveKit: [rust-sdks README](https://github.com/livekit/rust-sdks) · [release `webrtc-89d790b` assets](https://api.github.com/repos/livekit/rust-sdks/releases/tags/webrtc-89d790b).

Go: [os/exec `Cmd.ExtraFiles`](https://pkg.go.dev/os/exec#Cmd).

W3C: [Screen Capture draft 2026-07-16](https://www.w3.org/TR/2026/WD-screen-capture-20260716/) · [`windowAudio` TAG review thread, June 2025](https://lists.w3.org/Archives/Public/public-webapps-github/2025Jun/0114.html).

Not re-checked, written from memory and marked so in the text: `AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM`; the endpoint ID string's form; `GIDC_REMOVAL`; UIPI's exact list of blocked operations; LiveKit's behaviour on identity reuse; `loopbackWithMute`'s meaning; electron-builder's NSIS defaults; Windows 10 22H2's end of servicing; libuv's use of the process handle in `kill`; the `windows` crate family's licence; Windows 10 build 20348 corresponding to Windows Server 2022 and Windows 11 all builds.
