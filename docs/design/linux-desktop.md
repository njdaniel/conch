# Design: the Linux desktop client and the shared shell contracts (D1, issue #769)

- **Status:** Proposed, 2026-10-28. Codex's draft (PR #887, commit 6f610c4) established the inventory, the LiveKit displacement receipt and most of the shell boundary; this revision by the principal keeps that evidence and structure, **reconciles media ownership with the merged Windows design** ([windows-desktop.md](windows-desktop.md), #775 as merged in #889), and makes the shared contracts platform-neutral. D1 stays open until Nick signs the architecture; no child of #754 becomes Ready by this text. Nothing here was run on a physical input device, with a second listener, against a keyring, or as an installed package: every such row is **pending** in the [capability report](../reviews/linux-desktop-capabilities.md) with the experiment that would settle it.
- **Issues:** epic #754; this note is #769. Children: #770 (shared shell), #771 (native voice and PTT), #772 (screen and scoped audio), #773 (tray, notifications, lifecycle), #774 (packaging and release). Coordinated with the Windows design #775 and its follow-ups #890–#906, which this note maps onto where they are shared.
- **Governed by:** [ADR-006](../adr/ADR-006-rust-voice-client.md), [ADR-007](../adr/ADR-007-client-interfaces.md) (Electron hosts the shared React application; a native client manages a session-scoped helper over a versioned local contract; "one owner captures/publishes a given microphone stream per session"), the managed-helper contract [tui-voice.md](tui-voice.md), [conch-voice.md](conch-voice.md), [voice-control-plane.md](voice-control-plane.md) and the UX rules [client-experience.md](client-experience.md).
- **Wire shapes:** nothing published changes. `conch.voice_control.v1` and `v2` are reused byte for byte. Two additions already proposed by the Windows design are reused here rather than invented twice: `conch.voice_control_pipe.v1` (the attach line, #893), which this note extends to a Linux endpoint (§5), and the companion grant on the server (#897). Each needs the owner's sign-off through the `schema-change` skill where it is a wire shape.
- **Evidence** is the draft's: the read-only inventory ([spikes/linux-desktop/](spikes/linux-desktop/README.md); receipts under [reviews/linux-desktop/](../reviews/linux-desktop/)) and the synthetic LiveKit probe (`duplicate-first.log`, `duplicate-second.log`). Sources were checked on 2026-10-28; a claim written from memory says so.

A person installs one package on Pop!_OS, saves their servers, chats, joins voice when they choose, holds a key or Mouse 4 while a game has focus and is heard exactly while it is held, and shares or watches a screen on configurations that were tried. The shell, the profiles, the IPC boundary and the lifecycle rules below are **shared** with Windows and macOS (#770 owns them); the audio device layer, the key source and the transport plumbing are per platform. The one decision this note exists to make is who owns which media stream, and it makes it the same way on both platforms.

## 0. Decisions proposed for review

| # | Area | Proposed | Needs the owner |
| --- | --- | --- | --- |
| D1-1 | Shell (§2, §3) | Package the existing React application locally in Electron; platform adapters for transport, authentication, media, notifications and lifecycle; browser builds unchanged | Electron pin and build tooling: new dependencies |
| D1-2 | Authority (§3, §4) | Main owns profiles, credentials, public REST/WS calls, IPC validation and one session-owned helper; the renderer never holds a bearer or a LiveKit token | no |
| D1-3 | **Media ownership (§6)** | **Model B on both platforms:** the Rust helper is the person's one voice participant; Electron is a conchd-issued **companion participant** under a derived identity for screen video (and scoped audio where a route exists); one playback owner per audio source (#906); watching needs the companion (B2) or a hand-over | the architecture: Nick's sign-off; server work #897 |
| D1-4 | Transport (§5) | One shared semantic contract: commands from the owner, events from the helper, authority only on the command channel, owner loss by EOF on either, bounded stop. Linux implementation for an Electron owner: a Unix socket in `$XDG_RUNTIME_DIR` plus the attach line; fd 3 stays for the Go TUI | `conch.voice_control_pipe.v1` (#893), extended to Linux |
| D1-5 | Input (§7) | Input lives in the helper; no press or release crosses the control channel; backends: the existing per-device evdev reader, a GlobalShortcuts portal backend where the interface exists, a native X11 backend for X11 sessions; no backend switch mid-hold | a D-Bus crate for the portal backend; an X11 crate: new dependencies |
| D1-6 | Capture and audio (§8) | Chromium's portal capture in the companion; share audio off by default; application audio only through a demonstrated exclusion route; never a bare output monitor | no (#772 scope) |
| D1-7 | Lifecycle (§9) | Close quits; tray opt-in; lock and suspend close transmission and capture, observed through logind; resume needs a fresh Join and press | no |
| D1-8 | Distribution (§9) | x86_64 `.deb`, per-user data, one active server per instance, manual verified updates first | packaging tooling: new dependencies (#774) |
| D1-9 | Server compatibility (§9) | An explicit tested allowlist of server releases, not a semver range, during alpha | no |

## 1. Existing behaviour to preserve

| Source at baseline `cace5b0` | Consequence |
| --- | --- |
| `internal/server/voice.go`, `voiceIdentity`, `voiceSession` | The participant identity is exactly `p<principal-id>`. **conchd does not refuse a second session for it:** `voiceSession` records the credential as a holder and signs a token on every request (`TestVoiceSessionRecordsTheHolder` expects 200 for "the same session again"). What limits one person to one connection is LiveKit, not conchd (§6.1). |
| `internal/server/voice_presence.go`, `parseVoiceIdentity` / `voiceEntitlement` | A suffixed or arbitrary identity is not a second authorised device today. The companion of §6 is therefore server work (#897), never a client trick. |
| `internal/server/livekit/token.go`, `JoinToken` | A join token initially permits only the microphone source; subscribe is enabled, data publication disabled. |
| `internal/server/voice_share.go`, `startShare`; `livekit/client.go`, `Permission` | Screen video and audio need the existing share claim and a permission update on the connected participant. Under model B the claim resolves to the person and the permission lands on the companion (#897). |
| `voice/conch-voice/src/managed.rs`; `pkg/schema/voice_control*.go` | v1/v2 offer lifecycle, mute/deafen, state, and in v2 device and talk-mode changes. Neither offers a press, a release or frame transport, and neither gains one. |
| `voice/conch-voice/src/managed.rs`, `open_event_pipe` | The helper requires file descriptor 3 to be a **FIFO**, checked before and after reopening it through `/proc/self/fd/3`. §5 explains why that rules out an Electron owner's fd 3 as it stands. |
| `voice/conch-voice/src/livekit.rs:129`, `151–163` | `auto_subscribe = true` and every remote audio track is mixed. Under model B this changes (#906). |
| `internal/cli/voicehelper/` | The Go supervisor's framing, version selection, bounded stop, environment scrubbing and ownership rules are the reference semantics; the supervisor itself stays Go. An Electron owner reimplements the semantics in main (§5). |
| `web/src/voice/sdk.ts`, `connection.ts` | The browser client captures and publishes microphone and screen on one JS room, **as the person's identity, watch-only mode included** (`publish = grant.can_publish && !this.state.watchOnly`). It must never run beside native voice as the person (§6.2, model D). |
| `scripts/voice-pins.sh` | The linked libwebrtc has `pipewire_stubs.o` removed so that `dmn-audio` can use PipeWire; libwebrtc's built-in screen capturer is unusable in the helper's process. Model B does not need it (§6.3). |

The helper verifies the selected principal (`GET /v1/whoami` against `principal_id`) before opening media. Preserve that even though main already authenticated the account.

## 2. Layout and build

```text
desktop/
  main/       app, profiles, credential vault, public API/WS transport,
              IPC validation, helper supervisor, lifecycle
  preload/    frozen narrow contextBridge surface; no generic ipcRenderer
  platform/   Linux/Windows/macOS host implementations behind shared semantics
  build/      Electron build, licences, local asset manifest, package metadata
  tests/      packaged app, malicious renderer, two-server fixtures
web/src/platform/
  browser/    current same-origin API/storage/LiveKit implementations
  desktop/    typed preload client and native media UI adapter
  index.ts    build-selected adapter; no Electron dependency in browser graph
voice/        existing helper and shared Rust crates, reviewed extensions later
pkg/schema/   authoritative public and local control definitions
```

Electron **45.1.0** is the investigation candidate (Chromium 156.0.8078.49, Node 24.22.1, released 2026-10-27; [releases.json](https://releases.electronjs.org/releases.json)). It is not installed, approved or runtime-tested by this change; #770 pins it with Nick's sign-off, and the packaged probe records `process.versions` before anything is claimed. React, Vite and LiveKit JS keep the pins in `web/package-lock.json`; the native SDK is `livekit = 0.9.4` with locked `libwebrtc 0.3.51`.

Build web independently with `make web-build`. A separate desktop build selects the desktop adapter and bundles its assets plus a compatible helper. Production starts neither Vite nor a localhost HTTP server. Keep Node off the renderer dependency graph. `go generate ./pkg/schema` and `make schema-ts` remain the type-generation path; any new local wire shape starts in `pkg/schema` under `schema-change`, with fixtures and generated TypeScript and Rust conformance. The IPC method list of §4 is a semantic inventory, not a second JSON protocol.

## 3. Local UI, network and authentication boundary

Serve an immutable packaged asset allowlist at **`conch-app://ui/`**, registering a standard, secure scheme before Electron `ready`. Validate exact scheme, host and normalised paths; reject traversal, encoded separators, userinfo, alternate ports and unknown assets. Never load a server's `/app/` or arbitrary remote HTML in this window. Grant the scheme no privilege that bypasses CSP, CORS or Fetch checks. Whether display capture works from this origin is a packaged-probe gate (report row L-S1).

`BrowserWindow` uses `nodeIntegration: false`, `contextIsolation: true`, `sandbox: true`, `webSecurity: true`, a fixed preload and no webviews. Reject navigation, redirects, new windows and unsolicited downloads. Only explicit user link actions open canonical `https:`/`http:` URLs in the OS browser; reject executable, custom and `file:` schemes and credentials in URLs. Links never inherit Conch authorisation. Attachments are untrusted bytes, not app assets.

Proposed desktop CSP: default, script, style and font sources restricted to packaged self; `object-src 'none'; base-uri 'none'; frame-src 'none'; frame-ancestors 'none'; form-action 'none'; connect-src 'none'`. Image previews may use `blob:`. The React application's inline styles need an explicit audit (nonce, stylesheet or a narrowly documented style-attribute allowance), not a broad exception. No `unsafe-eval`, remote scripts, service workers or credential-bearing remote image loads. Media and display permission handlers deny by default and grant only to this main frame for the current explicit user action; under model B the renderer's **display** permission is granted for the picker, and its **microphone** permission stays denied (the microphone is the helper's).

**Main is a native public-API client.** It makes authenticated REST calls and WS upgrades itself, with the bearer header as the CLI does and no manufactured `Origin`; no new cookie authentication, local proxy, exposed localhost socket or server exception for Electron. Main maps reviewed operation ids to fixed public routes and schema validators; it accepts no arbitrary URL, header map or fetch command. Only the selected profile controls the base address: root-mounted HTTPS origins; plain HTTP only for explicit loopback development; reject userinfo, fragments, unexpected base paths and non-HTTP schemes. Private-CA deployments install the CA in the trust stores and test both main's and the helper's trust paths. Never override certificate failures (`NODE_TLS_REJECT_UNAUTHORIZED`, Chromium certificate errors) or CORS. Redirects on authenticated operations fail closed; credentials never follow a redirect to another origin. The same restriction applies to the LiveKit endpoint and to the helper's transport.

## 4. Trusted IPC and profile isolation

| Method family proposed | Input and authority | Result and limits |
| --- | --- | --- |
| Profile list, add, rename, remove, select | User-selected canonical origin; opaque profile id for changes | Display metadata only; main binds server and account and increments the session generation |
| Login, challenge, sign-out | Existing schema-derived password, invite, device-link and TOTP operations; secrets accepted only for that operation | Main consumes token fields and returns sanitised account or challenge state, never a bearer. Passwords and re-auth proofs are not persisted or logged |
| API domain operations | Allowlisted operation plus its generated request type | Generated response validation, bounded bytes, credential fields consumed in main, sanitised errors. Operator re-auth proofs stay main-owned with expiry |
| Feed subscribe, unsubscribe | Approved feed kind, channel, session, cursor | Main owns socket handles, rate limits, queue bounds and teardown; sends validated events |
| Voice join, leave, mute, deafen, devices, mode | Current profile and channel; explicit Join required | Confirmed helper state, never optimistic transmit state. **No renderer press or release method** |
| Share start, stop, watch | Current room, explicit picker action, user-selected source | Validated share state; captures scoped to the current generation; no source-id bypass |
| Notifications, external link, clipboard attachment | Fixed notification fields or validated URL; explicit user action for clipboard and file reads | No shell, arbitrary path, credential getter or general IPC forwarding |

On **every invocation and subscription**, main checks `event.sender` against the owned `webContents`, `senderFrame` against its live main frame, the exact local origin and the expected document generation, plus schema, size and rate limits; it rejects subframes, destroyed senders, devtools and other windows, and stale handles. The preload exposes named functions and unsubscribe callbacks, never an Electron event object or a caller-chosen channel. A compromised renderer can exercise the account's allowed UI actions; it cannot escape into host operations. Server authorisation stays authoritative, guests and operator TOTP included.

Profiles are keyed by a random local id mapped to canonical origin plus verified principal, never by display name or hostname. Preferences and drafts are scoped by profile; no renderer token storage. One in-memory Electron session partition per active profile generation, cleared on switch and sign-out. Two instances use independent data roots and profile locks, or focus the existing instance for the same root; no competing credential writers. One helper belongs to one instance, never a global singleton. On switch: stop media, close sockets, abort requests, clear renderer state and release the helper before activating the next profile; every async completion carries a generation check. Removing a profile deletes its vault entry and scoped cache; sign-out revokes through the API where supported and forgets locally even when offline, saying that offline deletion cannot prove server revocation.

### 4.1 Credential storage and helper hand-off

Main uses Electron `safeStorage` with encrypted credential files under a private user data root (0700 directory, 0600 files, atomic replacement). On Linux the keys come from "a secret store that varies depending on your window manager and system setup", and "if no secret store is available, items stored in using the `safeStorage` API will be unprotected"; the asynchronous API's providers include the Portal Secret interface and the Secret Service, the synchronous API is removed in Electron 46 ([safeStorage](https://www.electronjs.org/docs/latest/api/safe-storage)). The inventory observed a Secret Service and a `Secret` portal v1 on this machine, which is presence, not proof of protection. Unknown, unavailable, denied, locked or insecure (`basic_text`) storage means **session-only login** with a visible storage status and a retry; never a hardcoded-key fallback. Decrypt errors do not erase other profiles. Measuring the pinned Electron's actual provider on this desktop is report row L-C1.

The token reaches the helper as `CONCH_TOKEN` in a child environment that main **builds from empty by an allowlist** (the Windows design's rule, §3.7 there, adopted for both platforms so that the two supervisors do not differ in what they let through: on Linux a filter would do, on Windows it would not), with `credential: env` in the start; `CONCH_CONFIG_DIR` per profile so that device configuration does not cross accounts. The environment is readable by same-user processes, which is outside the vault's at-rest guarantee and is said so. Crash uploads carry no environment; logs are redacted. A one-shot credential channel is a future control version if ever wanted, not something added to v1/v2 here.

## 5. The managed transport from an Electron owner

**The shared contract, stated once for every platform** (identical to [windows-desktop.md §3](windows-desktop.md) and adopted by #770): a helper has **one command channel** from the owner and **one events channel** to the owner; **authority arrives only on the command channel, from the spawning process**; the events channel carries no authority, so a hijacked or observed events channel cannot cause a join, a publish or an unmute; **owner loss is EOF on either channel**, whichever comes first; the owner's stop is `leave`, close both channels, a graceful signal where the platform has one, a forced kill at `VoiceControlStopKillAfterMS`, then reap; nothing is ever killed by name; no new helper starts until the old one is reaped. Every message, bound, phase and fixture of tui-voice.md applies unchanged. Shared interfaces name **"the command channel" and "the events channel"**, never fd 3, process groups or PipeWire.

### 5.1 A Linux finding: an Electron owner cannot use fd 3 as it stands

The Go supervisor gives the helper four `os.Pipe()` FIFOs, the events on fd 3 (`internal/cli/voicehelper/run.go`). Node's `child_process.spawn` with `stdio: ['pipe', 'pipe', 'pipe', 'pipe']` also puts a fourth channel at fd 3 in the child (libuv `dup2`s stdio index N to fd N; [libuv `src/unix/process.c`](https://raw.githubusercontent.com/libuv/libuv/v1.x/src/unix/process.c), `uv__process_child_init`), **but it is not a FIFO: libuv creates every `'pipe'` stdio entry with `uv_socketpair(SOCK_STREAM, …)`** (same file, `uv__process_init_stdio`), which is what Node means by "these are not actual Unix pipes and therefore the child process can not use them by their descriptor files" ([Node child_process](https://nodejs.org/api/child_process.html)). The helper's `open_event_pipe` (`voice/conch-voice/src/managed.rs`) refuses fd 3 unless `metadata("/proc/self/fd/3").file_type().is_fifo()`, then reopens that path for writing and checks `is_fifo()` again; a socket fails the first check, and `/proc/self/fd/N` of a socket cannot be reopened anyway. So **today's helper, started by Electron with a fourth stdio pipe, exits with `EventPipe("is not a pipe")`**. Standard input as a socketpair is fine: the helper reads it with ordinary reads, and a `SOCK_STREAM` peer closing is EOF. This is a desk finding from the two sources named; it has not been run (report row L-X1 is the experiment).

### 5.2 Options for the events channel on Linux

| Option | For | Against |
| --- | --- | --- |
| **A. Relax the helper to accept a socket on fd 3** | smallest change | the reopen-through-`/proc` trick is how the helper gets an owned `File` without `unsafe`; a socket cannot be reopened by path, and wrapping fd 3 directly is `FromRawFd::from_raw_fd`, `unsafe`. Not available under `forbid(unsafe_code)` |
| **B. An owner-created Unix socket in `$XDG_RUNTIME_DIR`, the helper connecting by path** (proposed) | the same shape as the Windows named pipe: `net.createServer().listen(path)` in main ("On Unix, the local domain is also known as the Unix domain. The path is a file system pathname", [Node net](https://nodejs.org/api/net.html)); `std::os::unix::net::UnixStream::connect(path)` in the helper, a safe function returning a `Read + Write` stream that `try_clone`s into independent handles ([UnixStream](https://doc.rust-lang.org/std/os/unix/net/struct.UnixStream.html)); the cross-user boundary is the directory: `$XDG_RUNTIME_DIR` "MUST be owned by the user, and they MUST be the only one having read and write access to it. Its Unix access mode MUST be 0700" ([XDG Base Directory Specification](http://specifications.freedesktop.org/basedir/latest/)); the attach line of #893 binds the connection to the spawn exactly as on Windows | one more way to start the helper (`--events-pipe <path>`); a socket file to unlink (Node unlinks it on `server.close()`; after a crash "a Unix domain socket will be visible in the file system and will persist until unlinked", same page, so the owner unlinks a stale path before listening); a Linux process can read the socket's inode in `/proc` as it can read any of the owner's state: same-user is out of scope, as everywhere |
| C. Events on standard output | no new channel | the Linux design moved events off fd 1 because libwebrtc writes there: the probe receipts show `Nvidia Decoder is supported.` printed on native stdout eight times (`duplicate-first.log`, `duplicate-second.log`). Rejected on evidence |
| D. The helper creates a FIFO and the owner opens it | keeps the FIFO path | Node has no `mkfifo`; a FIFO opened by path has blocking-open and reader-count semantics the FIFO-on-fd-3 design avoided by inheriting; no gain over B |

**Proposed: B.** The helper gains `managed --events-pipe <path>` as one option meaning "an endpoint to connect to" on both platforms (a `\\.\pipe\` name on Windows, a socket path on Linux), writes the attach line first and then the hello, and reads the connection on its own thread solely to see its end; fd 3 remains the Go TUI's way and is unchanged. The owner's rules are the Windows design's six (random name and nonce from `crypto.randomBytes`, first-line deadline, 4096-byte cap, `timingSafeEqual`, never write, close the listener on attach or timeout, redact the nonce), with the socket path under `$XDG_RUNTIME_DIR/conch/<profile-id>/` (fallback: refuse to start voice if the variable is unset, saying so; never `/tmp`), and `fs.chmodSync(path, 0o600)` after `listen` as belt and braces (Node sets no mode itself; memory). A Go owner on Linux could additionally check `SO_PEERCRED` against the spawned pid; `UnixStream::peer_cred` is nightly-only in Rust ([UnixStream](https://doc.rust-lang.org/std/os/unix/net/struct.UnixStream.html)), so the helper does not. The attach schema is #893's; its design-doc text says, and this one repeats, that the nonce is a spawn-to-pipe binding and defence in depth, **not authentication**.

### 5.3 Owner loss, stop and signals on Linux

| Rule | Go TUI owner (today) | Electron owner (proposed) |
| --- | --- | --- |
| owner loss | EOF on stdin; parent pid changes (`std::os::unix::process::parent_id`, every `VoiceControlOwnerPollMS`) | the same two, plus EOF on the events socket; the parent watch works unchanged because Electron main is the parent |
| stop | `leave`, close stdin, `SIGTERM` at `StopTermAfterMS`, `SIGKILL` at `StopKillAfterMS`, `Wait` | `leave`, close stdin and the events server, `child.kill('SIGTERM')` at `StopTermAfterMS`, `child.kill('SIGKILL')` at `StopKillAfterMS`, wait for `exit`; Node's `kill` sends the named signal on Unix ([Node child_process](https://nodejs.org/api/child_process.html)) |
| process group | `Setpgid`, so terminal job control does not reach the helper | no terminal, so no need; spawn **without** `detached` (which would `setsid` and make the helper survive main's exit as a session leader until the EOFs land) |
| signals only to the owned process | by `*os.Process` before `Wait` | by the `ChildProcess` object; never `pkill`, never by name |
| descriptors | only the four pipes reach the child (`os/exec` closes the rest) | `stdio` of exactly `['pipe', 'pipe', 'pipe']` plus the socket path; libuv closes other descriptors in the child (same source: `uv__process_child_init` remaps and `cloexec`s) |
| environment | filter of `CONCH_*` and Rust debug variables | allowlist from empty (§4.1) |

The bounds (`VoiceControlGateShutMS` 100, `VoiceControlCaptureStopMS` 500, `VoiceControlOwnerLossExitMS` 3000) are the contract's and are measured again from an Electron owner (report row L-X3); a write failure on the events channel becoming owner loss is #894 and applies here too.

## 6. Media ownership: model B on both platforms

**Decision proposed, for Nick's sign-off:** **Model B.** The Rust helper is the person's one LiveKit participant for **voice**: it publishes the microphone behind its gate and plays remote voice through its mixer. Electron is a **companion participant** that conchd issues under a **derived identity** (#897): it publishes screen video (and scoped audio where §8 finds a route) through Chromium's portal capture and the LiveKit JS SDK, is folded into the person by conchd in presence, occupancy, share claims and revocation, and ends with the voice session. **One playback owner per audio source:** voice audio is the helper's, share audio is the companion's, and the helper never subscribes to its own companion's tracks (#906). Within B, **B2** (companion `canSubscribe: true` with `autoSubscribe: false`, subscribing only to screen tracks, so the desktop app can watch) is preferred over **B1** (publish-only companion; watching needs a hand-over). The Windows design's §7 holds the full option table and the LiveKit-grant analysis; this section states what is Linux-specific and why A is not chosen.

### 6.1 The evidence both documents rest on

- **conchd issues a second grant for the same person; LiveKit displaces the first connection.** The draft's synthetic probe, real LiveKit 1.13.7 in Docker, Rust SDK 0.9.4: the first connection reported `connection_state: Disconnected` with reason `DUPLICATE_IDENTITY` **507 ms** after the second `Room::connect` (`duplicate-first.log` `t_ms` 2121.916 against the second log's `connect_call` at 0.619 ms, with `unix_us` anchors 1793194754860185 and 1793194756474003; the report reproduces 507.487 ms), and the second stayed connected for its 5 s window (n = 1). That is the whole reason a desktop with native voice cannot add Chromium media as a second connection **as the person**, and the whole reason model B uses a **distinct identity that conchd sanctions** rather than a second token for `p<id>`.
- **The browser's watch-only mode is still the person's identity** (`web/src/voice/connection.ts`), so "a browser tab watches beside native voice" is not a fallback either; the hand-over of model D in the Windows §7.2 is.
- **LiveKit's grant cannot say "subscribe to video only"**: `canSubscribe` is one `*bool`, `canPublishSources` restricts publishing only ([`protocol/auth/grants.go`](https://raw.githubusercontent.com/livekit/protocol/main/auth/grants.go)); the JS SDK's `autoSubscribe` ("autosubscribe to room tracks after joining, defaults to true", [RoomConnectOptions](https://docs.livekit.io/reference/client-sdk-js/interfaces/RoomConnectOptions.html)) is the client-side lever B2 uses, and it is client honesty, not enforcement, which §7.2 of the Windows design states with its limit.

### 6.2 Model A, the considered alternative

The draft's decision 3 made the helper the **sole** participant: Electron captures the screen with consent and hands raw frames to the helper, which publishes them; the helper receives remote screen video and hands decoded frames back to Electron for a canvas. It is the right instinct (one identity, no server change) and it is **not chosen**, for these reasons:

1. **The bridge does not exist and is large.** Raw I420 at 1080p30 is about 93 MB/s in each direction before copies (the draft's arithmetic, kept as a cost warning, not a measurement); it needs a versioned framed bulk channel with generation, timestamps, stride and length validation, bounded queues and stall handling; Rust-side encoding through `NativeVideoSource::capture_frame` ([libwebrtc 0.3.51](https://docs.rs/libwebrtc/0.3.51/libwebrtc/video_source/native/struct.NativeVideoSource.html)) and Rust-side decoding to frames the shell draws outside Chromium's hardware path. The draft itself listed all of this as a blocking, undemonstrated experiment.
2. **B uses what exists.** Chromium's screen capture, encoding, simulcast and decoding are the mature path the browser client already ships on (`web/src/voice/share.ts`, `sharing.ts`, the alpha's measured share tests). The companion reuses that code; its new costs are the conchd grant (#897) and the helper's subscription rule (#906), both bounded, both testable with the existing e2e harness.
3. **B keeps the helper's process free of screen capture.** The linked libwebrtc had `pipewire_stubs.o` removed so that `dmn-audio` could own PipeWire in the helper's process (`scripts/voice-pins.sh`). Under A, any screen capture in the helper's process reopens that conflict; under B, capture happens in Chromium's process, which has its own PipeWire client, and the two never share a process.
4. **The draft's own receipt argues for B.** The 507 ms displacement is a fact about identities, not about processes; a distinct identity answers it, and A's single connection answers it at the price of the bridge.

**What would revive A:** evidence that a companion cannot be kept off voice (it can: B1 is server-enforced, B2 is client-filtered, and the helper's subscription rule is independent of both); a measurement that Chromium's capture from the packaged local origin on Linux fails on the supported desktops in a way the helper's own capture would not (report row L-S1); or a server-side reason the derived identity cannot be folded into the person (#897's design review). None of these exists today. Model C (Chromium owns all media, the helper becomes an input source over a new `conch.voice_input.v1`) stays the documented alternative to both, as in the Windows §7.2.

### 6.3 What is Linux-specific under B

- **Capture.** On Linux with PipeWire, "`desktopCapturer.getSources(options)` only returns a single source" and "PipeWire supports a single capture for both screens and windows"; requesting both types returns the selection as a window capture; the portal shows the picker and the handler must deny "when the user cancels the PipeWire picker" ([Electron 45.1.0 `desktopCapturer`](https://raw.githubusercontent.com/electron/electron/v45.1.0/docs/api/desktop-capturer.md)). The inventory observed ScreenCast v5 with monitor and window source types on this COSMIC session (report F2), which is the interface Chromium uses. Whether capture works from `conch-app://ui/` in the pinned Electron is pending (L-S1).
- **Share audio.** Electron's `loopback` audio "is currently only supported on Windows" ([Electron 45.1.0 `session`](https://raw.githubusercontent.com/electron/electron/v45.1.0/docs/api/session.md)). On Linux the three-scope vocabulary of the Windows §8 (none, this application, everything but the call) applies with no demonstrated route for the last two yet: §8 says what is and is not offered.
- **No in-process capture stack.** The helper does not grow screen capture, so `pipewire_stubs.o` stays removed and nothing in §1's last row changes.
- **The contradiction check asked for by this revision.** No Linux fact was found that forces A or forbids B: the portal capture is Chromium's own, independent of the helper; a companion stays off voice by grant (B1) or by `autoSubscribe: false` plus #906 (B2); the Secret Service, PipeWire and logind interfaces B relies on are the same ones A would rely on. The one real Linux-only obstacle found in this revision is the transport one of §5.1, and it is independent of the media model.

### 6.4 What model B asks of others (shared with the Windows design)

- **conchd (#897):** the companion grant and the viewer identity, one route with the publish set as a parameter; derived identity; folded presence and occupancy; share claims and stops resolve to the person; torn down with the parent session; the poller removes a companion without a parent.
- **helper (#906):** `auto_subscribe = false`; never its own companion's tracks when it has one; never other participants' `ScreenshareAudio` when the companion owns share playback (B2); unchanged with no companion (the TUI, the standalone helper); the same filter at join (`RoomEvent::Connected { participants_with_tracks }`) and later.
- **web (`web/src/voice/`):** the viewer resolves a share to a participant through conchd's presence rather than assuming the sharer's own identity publishes it (#772 and the Windows W4 share it).
- **The experiments:** the Windows E10 list, run once on Linux with the draft's probe harness extended (report L-M1 to L-M4).

## 7. Global push-to-talk input

**Decision proposed.** Input lives in the helper on every platform; no press or release crosses the control channel; bindings are configuration. The invariant as both control versions have it: no control command opens the gate; in push-to-talk mode a fresh press opens it, in open mode a fresh onset ([conch-voice.md §3](conch-voice.md), [tui-voice.md §20.6](tui-voice.md)). On Linux there are three sources of presses, **one active per session, never switched mid-hold**:

| Backend | Where | Evidence so far | Pending |
| --- | --- | --- | --- |
| **Per-device evdev reader** (exists: `keydev.rs`, `conch-voice.md §4`) | helper | the shipped Linux path; reads one configured event device with a udev grant; refuses anything but an event device; a lost device shuts the gate | ordinary-user access on this machine: the inventory saw 21 event nodes and **zero readable by this process**, and changed no group, ACL or rule (report F2). Mouse 4 (`BTN_SIDE`) on a mouse event device is a numeric code the decoder could accept; it is not a hardware result. Rows L-P1 to L-P6 |
| **GlobalShortcuts portal** (proposed) | helper, over D-Bus | the specification has `Activated` and `Deactivated` signals ([GlobalShortcuts](https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.GlobalShortcuts.html)). **Absent on this COSMIC session**: not advertised, and `Properties.Get` on its `version` answered `No such interface` (report F2, host receipt). Electron's own `globalShortcut` has an activation callback and no release ([45.1.0 global-shortcut](https://github.com/electron/electron/blob/v45.1.0/docs/api/global-shortcut.md)); unusable, as ADR-007 already says | a desktop that has the interface (GNOME, KDE: pending machines); whether a compositor accepts mouse triggers; the D-Bus crate (new dependency, Nick); if the portal needs the parent window's identity, the helper's own session may be refused and main would have to own it, which would force the trusted-input extension of §7.2 |
| **Native X11** (proposed) | helper | none; XWayland's `DISPLAY` in a Wayland session is not an X11 session | a genuine X11 session (pending machine); an X11 crate (new dependency); grabs, lock, fullscreen |

### 7.1 Fail-closed rules (shared)

The Windows §4.4 rules are adopted as the desktop's on every platform, each with its Linux form: repeats never hold or reopen; **an unknown initial state is not a press** (a binding is armed only by an observed release; the evdev path already sees releases, the portal path must see `Deactivated` once before `Activated` counts); device or portal-session loss shuts the gate with no tail and needs a fresh press; a stalled input thread shuts the gate (`ptt: lost`, `ptt_problem: read_failed`); focus changes do nothing; nothing but bound transitions leaves the input module. A press that outlives its release (a release the backend never saw) is bounded only by `max_transmit_secs`.

**The missed-release bound is a desktop decision for Nick.** Today's default is 120 s, at most 600 (`conch-voice.md §3`). The draft proposes **30 s** for desktop sessions (shorter configured values still win), which this revision carries as a **shared desktop default on both platforms** rather than a Linux rule, so that the two clients do not differ in how long a stuck key can transmit. The Windows design assumed the 120 s default; whichever value Nick picks, both documents follow (follow-up L-F9). Repeat events cannot renew the deadline on any backend.

### 7.2 Where input lives, and when it would not

Input lives in the helper (above). The Windows §6 lists the two cases that would need a versioned extension: model C, and live rebinding. Linux adds one: **if a GlobalShortcuts session must be created by the process that owns the window** (so that the portal can attribute it), the helper, which has no window, could not hold it, and main would have to deliver presses over a new trusted-input channel (`conch.voice_input.v1`, with source, generation, disconnect and fail-closed semantics). That is a hypothesis to test on a portal-equipped desktop (L-P7), not a design; until then the helper owns the portal session.

Configuration: the evdev path keeps `[keys] device` and the `KEY_*` names; the portal and X11 paths get their own binding forms under `[keys]`, written by `conch-voice config set` (the Windows F7, #896, defines the grammar for scan codes and mouse buttons; the Linux forms join it). Binding capture for the UI happens in the focused Electron window, as the Windows §4.5 says.

## 8. Screen, window and audio capability policy

Chromium's picker, from the companion, with the Linux facts of §6.3. Record whole-screen versus window behaviour, cancellation, denial, source closure, portal restart, cursor mode and GPU per configuration (L-S1 to L-S4). A rich thumbnail chooser cannot be assumed: the portal shows its own picker and returns one source.

Share audio uses the three scopes of the Windows §8, **off by default**:

| Scope | Linux route | Status |
| --- | --- | --- |
| none | — | default |
| this application | a PipeWire stream of the picked application alone; no Chromium route (loopback is Windows-only, above); a native route would be the helper capturing one application's output node and publishing `ScreenshareAudio` on the companion's behalf, which crosses the participant boundary and is #772's design to make or refuse | **pending**; not offered until demonstrated (L-S5) |
| everything but the call | a monitor of the output with the helper's own playback excluded; PipeWire can capture a sink's monitor, which includes the helper's playback of the room, so a bare monitor **re-broadcasts the call** and is never opened | **pending**; not offered until an exclusion route is demonstrated (L-S6) |
| whole output through a bare monitor | — | **unsupported**, by policy |

Share audio has its own consent and its own mute; microphone mute never enables a captured monitor; deafen stops local audible playback; lock, leave and quit stop both outgoing streams. Feedback is tested by playing remote speech alone against a measurement listener before any audio route is offered (L-S6).

## 9. Lifecycle, compatibility and distribution

| Event | Required behaviour proposed |
| --- | --- |
| Launch, login | Text first; no microphone, key monitoring or capture until explicit Join or configuration |
| Close | Quit by default; stop capture and media, `leave`, close the channels, reap the owned helper |
| Opt-in tray | Close hides only while a working tray indicator and an explicit setting exist; it shows joined, transmitting and sharing state and a Quit; no tray means quit |
| Lock, suspend | Fail closed: stop the share, mute or leave voice; resume and unlock need an explicit Join, a fresh source selection and a fresh press. **Observation on Linux:** Electron's `lock-screen`/`unlock-screen` events are macOS and Windows only; `suspend`/`resume` carry no platform annotation ([powerMonitor](https://www.electronjs.org/docs/latest/api/power-monitor)). Main therefore watches logind: the session's `Lock` and `Unlock` signals and `LockedHint` property, and the manager's `PrepareForSleep(true/false)` ([org.freedesktop.login1](https://www.freedesktop.org/software/systemd/man/latest/org.freedesktop.login1.html)), found through `GetSessionByPID`. On `Lock` or `PrepareForSleep(true)` main sends `set_mute true` (lock) or `leave` (sleep) and stops the companion's capture. Whether COSMIC sets `LockedHint` is pending (L-L1); inability to observe the lock is a support blocker, not permission to keep transmitting |
| Renderer crash | Main tears down media and capture; the renderer restarts signed in only if the vault allows; never rejoins |
| Main or helper crash, EOF | The helper fails closed on owner loss (§5.3); main drops confirmed media state on helper loss and keeps chat working; no automatic restart that captures |
| Network loss, room rotation | The existing fresh-session, backoff and revocation policy; the gate stays shut until a fresh press; the share is **not** resumed after rotation on the desktop without a new consent (stricter than the browser's resume, by design) |
| Sign-out, profile switch | Complete media teardown before the next account; cancel pending pickers and claims; reject stale completions |

Share stops name their claim, so a late cleanup cannot stop a newer share. Secure-desktop and lock observation are backend-specific on every platform.

**Compatibility.** Read `/healthz` before login; after login validate `whoami` and every required response family. During alpha the desktop accepts only the server release bundled with its own release plus explicitly tested patch releases: an **allowlist with a fixture matrix**, not a `>=` range; unknown or development versions get a clear unsupported state and no automatic login or media. Helper negotiation is independent: read the v1 hello, choose the highest mutually implemented version, reject before capture. Bundled releases match; a protocol mismatch is reported, never inferred from a version string.

**Distribution.** Per-user data; a system-installed x86_64 `.deb` with a desktop file whose application id matches the portal identity; sandbox on; the bundled helper beside the app. Measure ELF runtime libraries, the glibc floor, GPU dependencies, keyring, PipeWire and clean-machine install, upgrade and removal (L-D1 to L-D3). No `--no-sandbox`, no root, no blanket input access. Manual verified package replacement first; signing, an updater and other formats are #774 after evidence.

## 10. Threats and security review targets

| Threat | Boundary and required verification |
| --- | --- |
| Malicious server messages, attachments or links compromise the renderer | Sandboxed local code, CSP, no remote navigation, inert rendering, strict blob handling; a compromised renderer cannot retrieve a bearer or invoke host operations |
| A subframe or foreign window calls preload methods | Exact `webContents`, main-frame, local-origin and generation validation on every operation; negative packaged tests |
| Server A's response or feed lands in account B | Abort, generation fences, isolated vault, cache, socket and media handles; a two-server delayed-response test |
| Credential leaks via redirect, logs, diagnostics, dumps, argv or environment | Fixed destinations, no redirects on auth calls, safe errors, vault ownership, the allowlisted child environment; canary scans of artifacts |
| The UI lies about mute, or the helper is stalled | The helper is the source of confirmed gate and device state; bounded control; fail closed on loss; no renderer gate-open API |
| The events channel is observed or hijacked | Authority only on the command channel; the events channel carries none; `$XDG_RUNTIME_DIR` mode 0700 is the cross-user boundary; same-user is out of scope and named; the attach line binds the connection to the spawn (#893's review) |
| A second connection as the person | Never made: the companion is a distinct, conchd-issued identity (#897); the browser client is never run beside native voice as the person; hand-over is explicit |
| Share audio returns the room's own voices | No bare monitor; audio scopes off by default; exclusion demonstrated before offered; feedback test with a measurement listener |
| Screen or audio consent outlives intent | Current-generation consent, immediate track stop, claim-matched cleanup, explicit resume; no resume after rotation without consent |
| Another local user reads the socket, the vault or the helper | 0700 roots and `$XDG_RUNTIME_DIR`, 0600 files, trusted bundled executable checks, process-scoped handles; same-user and root are outside the vault guarantee |
| A portal, D-Bus or X11 dependency weakens isolation | Dependency, licence and `unsafe` review; no sandbox disabling; no broad input access |

Security review precedes accepting the auth, IPC, transport and media contracts and again precedes merging their implementations; #893 and #897 each carry their own. Nick signs any ADR, protocol version or dependency this requires.

## 11. What stays platform-neutral

The shell, profiles, IPC families, CSP, auth boundary, compatibility policy and lifecycle table (#770, #773); the transport semantics of §5 and the attach line (#893); media model B, the companion grant and the subscription rule (#897, #906); the fail-closed input rules and the missed-release default (§7.1); the three audio scopes (§8). Per platform: the audio device layer (`dmn-audio` here, `wasapi` there), the key sources (evdev, portal and X11 here; Raw Input there), the events-channel endpoint (a Unix socket here, a named pipe there), lock observation (logind here, `powerMonitor` there), packaging.

## 12. Gates and sign-offs

| Item | Kind | Section |
| --- | --- | --- |
| Media model B on both platforms | architecture decision; Nick | §6 |
| The shared missed-release default (30 s proposed) | product decision; Nick | §7.1 |
| Electron pin and desktop build tooling | new dependencies; Nick (#770, #774) | §2, §9 |
| A D-Bus crate for the portal backend; an X11 crate | new Rust dependencies; Nick (#771) | §7 |
| `conch.voice_control_pipe.v1` extended to a Linux endpoint; helper `--events-pipe` on Linux | schema already proposed in #893; the Linux endpoint joins it | §5 |
| Companion grant and viewer identity | server design and route; #897 | §6 |
| Helper subscription rule | helper work; #906 | §6 |
| Events-write failure is owner loss | helper work; #894 | §5.3 |

## 13. Traceability: #769 acceptance criteria to sections and report rows

| #769 acceptance criterion | Section | Report rows |
| --- | --- | --- |
| Proposed `desktop/` layout, platform-adapter boundary, local bundled UI, schema generation, server-version negotiation, threat model; browser builds independent | §2, §3, §9, §10 | L-D1, L-C2 |
| Trusted IPC methods and caller validation, credential ownership, per-server profiles, local origin, API/WS authentication and CSP; no disabled webSecurity, CORS or certificate validation | §3, §4 | L-C1, L-C2, L-C3 |
| A concrete media ownership model with one microphone publisher and one audible playback path; the second-connection conflict tested; screen and audio not assumed compatible with a second participant | §6 | L-M1 to L-M4, F1 |
| Keyboard AND Mouse 4 hold and release with another app or game focused; portal, per-device and X11 compared; unavailable, denied, grabbed and lost-release behaviour and bounds; no release inferred from repeats | §7 | L-P1 to L-P8, F2, F3 |
| Actual portal and compositor capabilities detected; versions recorded | report §Environment, F2 | inventory receipts |
| Portal input in the helper or a versioned trusted-input extension; v1/v2 not repurposed; approvals | §7.2 | L-P7 |
| Screen and window selection, system and application audio, denial, closure, remote-participant feedback; supported, unsupported and unverified kept apart; no 1080p60 promise | §8 | L-S1 to L-S6 |
| Close, tray, quit, lock, suspend, crash and reconnect rules; keyring with session-only fallback; first packaging format | §9 | L-L1 to L-L3, L-C1, L-D1 to L-D3 |
| A reproducible spike on available hardware and an explicit pending matrix for unavailable machines; no claim of the owner's machine tested remotely; architecture-blocking findings resolved before children are Ready | report §Pending matrix and §Procedure | all |

## 14. Follow-ups, mapped onto existing issues

| Id | Title | Scope (one line) | Issue |
| --- | --- | --- | --- |
| L-F1 | Accept model B for the Linux desktop | the decision of §6 recorded on #769 with Nick's sign-off; #770's shared media interface follows it | #769, #770 |
| L-F2 | Linux events endpoint for the helper | `managed --events-pipe <path>` accepting a Unix socket path, attach line first, read for EOF; owner rules in main; the socketpair finding as a test | #893 (extend to Linux) |
| L-F3 | Electron owner supervisor in main | the §5.3 table: stop escalation, no `detached`, allowlisted environment, generation fences; measured bounds | #770 |
| L-F4 | Companion grant and viewer identity on conchd | as #897; Linux adds no requirement | #897 |
| L-F5 | Helper subscription rule | as #906 | #906 |
| L-F6 | GlobalShortcuts portal backend in the helper | D-Bus crate decision (Nick), session ownership test on a portal desktop, `Activated`/`Deactivated` with the arming rule; the main-owned fallback only if L-P7 fails | #771 |
| L-F7 | Native X11 input backend | crate decision, press and release, grabs, lock, fullscreen on a real X11 session | #771 |
| L-F8 | Lock and suspend through logind in main | `Lock`/`Unlock`/`LockedHint`, `PrepareForSleep`; `set_mute` and `leave`; COSMIC's `LockedHint` behaviour recorded | #773 |
| L-F9 | Shared desktop missed-release default | Nick picks the value (30 s proposed); both designs and the helper's desktop default follow | new (decision record) |
| L-F10 | Share-audio routes on Linux | application audio and everything-but-the-call through PipeWire with demonstrated exclusion, or marked unsupported; feedback test | #772 |
| L-F11 | Packaged Electron spike on this desktop | pinned Electron from `conch-app://ui/`: picker, capture, `safeStorage` provider, sandbox; receipts | #770 |
| L-F12 | `.deb` packaging and clean-machine lifecycle | runtime libraries, glibc floor, portal identity, install, upgrade, removal | #774 |
| L-F13 | Server compatibility manifest | the allowlist and fixture matrix of §9 | #770 |
| L-F14 | Linux hardware run | the report's procedure with Nick at the keyboard and a second listener; GNOME, KDE and X11 machines when available | new (the other half of #769) |

## 15. Sources

Checked 2026-10-28. Repository: `internal/server/voice.go`, `voice_test.go`, `voice_presence.go`, `livekit/token.go`, `voice_share.go`; `internal/cli/voicehelper/run.go`; `voice/conch-voice/src/managed.rs` (`open_event_pipe`), `livekit.rs`, `keydev.rs`; `web/src/voice/connection.ts`, `share.ts`; `scripts/voice-pins.sh`; the receipts under `docs/reviews/linux-desktop/`.

External: [libuv `src/unix/process.c`](https://raw.githubusercontent.com/libuv/libuv/v1.x/src/unix/process.c) · [Node child_process](https://nodejs.org/api/child_process.html) · [Node net](https://nodejs.org/api/net.html) · [Rust `UnixStream`](https://doc.rust-lang.org/std/os/unix/net/struct.UnixStream.html) · [XDG Base Directory Specification](http://specifications.freedesktop.org/basedir/latest/) · [org.freedesktop.login1](https://www.freedesktop.org/software/systemd/man/latest/org.freedesktop.login1.html) · [Electron 45.1.0 `desktopCapturer`](https://raw.githubusercontent.com/electron/electron/v45.1.0/docs/api/desktop-capturer.md), [`session`](https://raw.githubusercontent.com/electron/electron/v45.1.0/docs/api/session.md), [`global-shortcut`](https://github.com/electron/electron/blob/v45.1.0/docs/api/global-shortcut.md), [release](https://github.com/electron/electron/releases/tag/v45.1.0) · [Electron `safeStorage`](https://www.electronjs.org/docs/latest/api/safe-storage), [`powerMonitor`](https://www.electronjs.org/docs/latest/api/power-monitor), [security](https://www.electronjs.org/docs/latest/tutorial/security), [releases.json](https://releases.electronjs.org/releases.json) · [GlobalShortcuts portal](https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.GlobalShortcuts.html), [ScreenCast portal](https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.ScreenCast.html) · [LiveKit `protocol/auth/grants.go`](https://raw.githubusercontent.com/livekit/protocol/main/auth/grants.go), [JS `RoomConnectOptions`](https://docs.livekit.io/reference/client-sdk-js/interfaces/RoomConnectOptions.html), [Rust SDK 0.9.4](https://docs.rs/livekit/0.9.4/livekit/), [libwebrtc 0.3.51 `NativeVideoSource`](https://docs.rs/libwebrtc/0.3.51/libwebrtc/video_source/native/struct.NativeVideoSource.html).

From memory, marked so in the text: that Node sets no mode on a Unix socket it creates; `BTN_SIDE` as Mouse 4's evdev code; that a `detached` spawn calls `setsid`.
