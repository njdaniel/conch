# Linux desktop integration and shared shell contracts

Status: **draft; D1 remains open** · Issue [#769](https://github.com/njdaniel/conch/issues/769)
· Baseline `cace5b0331ad65a4d2f2e11ef782b527346748a1` · 2026-10-28.

This is a proposed implementation boundary under accepted ADR-006/007, not an
accepted ADR amendment, dependency approval, or desktop support claim. The
[capability report](../reviews/linux-desktop-capabilities.md) separates inventory,
synthetic experiments and pending real-device tests. Architecture-blocking rows
keep #770–#774 out of Ready. Coordinate the shared boundary with Windows #775
before accepting it; this document does not establish Windows feasibility.

## Decisions proposed for review

1. Package the existing React application locally in Electron. A platform
   adapter supplies public-API transport, authentication, media, notifications
   and desktop lifecycle. Browser builds retain their present browser adapters.
2. Main owns profiles and credentials, authenticates public REST/WS requests,
   validates every privileged action and supervises one session-owned helper.
   The renderer never receives a stored bearer credential or LiveKit token.
3. **The Rust helper is the sole LiveKit participant connection for desktop
   media in each joined room.** It owns microphone capture/gating and all
   audible playback. Electron supplies consented screen capture and displays
   received screen frames through a proposed bounded local media bridge.
4. Put portal input inside the helper, alongside its per-device input backend.
   Do not turn managed mute/unmute or talk-mode commands into PTT. A fallback
   design in which main delivers input would require a separately reviewed,
   versioned trusted-input protocol, not an addition to published v1/v2.
5. Start with x86_64 `.deb`, one active server/account/voice destination per
   instance, no auto-join, default close quits, explicit manual package updates.
   These are proposed defaults pending package and runtime measurements.

Decision 3 removes the known identity conflict, but **the local media bridge
has not been demonstrated**. Its performance, safety and SDK compatibility
are a blocking experiment, not an implementation commitment. If it fails,
return to owner review with evidence; neither a second participant nor a
framework switch is an automatic fallback.

## Existing behavior to preserve

| Source at baseline | Consequence |
| --- | --- |
| `internal/server/voice.go`, `voiceIdentity` | The participant is exactly `p<principal-id>`; one connection per principal per room. |
| `internal/server/voice_presence.go`, `parseVoiceIdentity` / `voiceEntitlement` | A suffixed or arbitrary media identity is not a second authorized device. |
| `internal/server/livekit/token.go`, `JoinToken` | A join token initially permits only the microphone source; subscribe is enabled, data publication is disabled. |
| `internal/server/voice_share.go`, `startShare`; `livekit/client.go`, `Permission` | Screen video/audio require the existing share claim and permission update on the already connected participant. |
| `voice/conch-voice/src/managed.rs`; `pkg/schema/voice_control*.go` | v1/v2 offer lifecycle, mute/deafen and state; v2 also offers device/talk-mode changes. Neither offers external PTT press/release or frame transport. |
| `internal/cli/voicehelper/` | Reuse framing, version selection, bounded shutdown, environment scrubbing and ownership semantics; the supervisor itself is Go, not an Electron module. |
| `web/src/voice/sdk.ts`, `connection.ts` | Browser mode currently captures/publishes microphone and screen on one JS room; this implementation must not run concurrently with native desktop voice. |
| `scripts/voice-pins.sh` | Linked libwebrtc excludes PipeWire stand-ins to coexist with `dmn-audio`; do not assume libwebrtc's built-in screen capturer is usable. |

Managed control v1/v2 have golden fixtures in `pkg/schema/testdata/voice-control*`.
The helper independently verifies the selected principal before opening media.
Preserve that check even when main already authenticated the account.

## Proposed layout and build

```text
desktop/
  main/       app, profiles, credential vault, public API/WS transport,
              IPC validation, helper supervisor, lifecycle
  preload/    frozen narrow contextBridge surface; no generic ipcRenderer
  platform/   Linux/Windows/macOS host implementations behind shared semantics
  build/      Electron build, licences, local asset manifest, deb metadata
  tests/      packaged app, malicious renderer, two-server fixtures
web/src/platform/
  browser/    current same-origin API/storage/LiveKit implementations
  desktop/    typed preload client and native media UI adapter
  index.ts    build-selected adapter; no Electron dependency in browser graph
voice/        existing helper and shared Rust crates, reviewed extensions later
pkg/schema/   authoritative public and future local control/IPC definitions
```

Electron **45.1.0** is the concrete investigation candidate, identified from its
official release on 2026-10-27. It is not installed, approved or runtime-tested
by this change. Record `process.versions.electron/chrome/node` from the actual
packaged probe before accepting it. Recheck security updates before pin approval.
Use tagged 45.1.0 API sources, not unversioned latest examples (references below).
React/Vite/LiveKit JS keep the existing `web/package-lock.json` pins; native SDK
is `livekit=0.9.4`, locked `libwebrtc=0.3.51`. No dependency changes land here.

Build web independently with `make web-build`. A separate desktop build selects
the desktop adapter and bundles its assets plus a compatible helper. Production
starts neither Vite nor a localhost HTTP server. Keep Node off the renderer
dependency graph. `go generate ./pkg/schema` and `make schema-ts` remain the
public type-generation/conformance path. Any new bridge/control wire shape
starts in `pkg/schema` using `schema-change`, protocol-owner review, fixtures
and generated TS/Rust conformance; the method list below is a semantic inventory,
not a second JSON protocol. Bulk media needs its own versioned framing/limits.

## Local UI, network and authentication boundary

Serve an immutable packaged asset allowlist at **`conch-app://ui/`**, registering
a standard, secure scheme before Electron ready. Validate exact scheme, host
and normalized paths; reject traversal, encoded separators, userinfo, alternate
ports and unknown assets. Never load a server's `/app/` or arbitrary remote HTML
in this window. Do not grant scheme privileges to bypass CSP, CORS or Fetch API
checks. Whether display capture works from this origin is an Electron probe gate.

BrowserWindow uses `nodeIntegration: false`, `contextIsolation: true`,
`sandbox: true`, `webSecurity: true`, a fixed preload and no webviews. Reject
navigation, redirects, new windows and unsolicited downloads. Only explicit
user link actions can open canonical `https:`/`http:` URLs in the OS browser;
reject executable/custom/file schemes and credentials in URLs. Links never
inherit Conch authorization. Attachments remain untrusted bytes, not app assets.

Proposed desktop CSP: default/script/style/font sources restricted to packaged
self; `object-src 'none'; base-uri 'none'; frame-src 'none'; frame-ancestors
'none'; form-action 'none'; connect-src 'none'`. Image previews may use `blob:`;
frame display may use local canvas/streams. Existing React inline styles need
an explicit audit (nonce/stylesheet or a narrowly documented style-attribute
allowance), not a broad script exception. No `unsafe-eval`, remote scripts,
service workers or credential-bearing remote image loads. Main's API transport
can provide validated attachment blobs with strict MIME/size limits. Register
media/display permission handlers to deny by default and grant only to this
main frame, for the current explicit user action; desktop renderer microphone
and camera permission remain denied under the native ownership model.

**Main is a native public-API client.** It makes authenticated REST calls and
WS upgrades itself. This avoids cross-origin renderer requests without changing
server CORS or certificate policy. WS uses the existing public bearer-header
path with no browser Origin, as the CLI does; it does not manufacture an Origin
or require `--allowed-origins`. Existing browser WS tickets stay in the browser
adapter. No new cookie authentication, local proxy, exposed localhost socket,
or server exception for Electron is needed.

Main maps reviewed operation IDs to fixed public routes/methods and schema
validators; it does not accept an arbitrary URL, header map or fetch command.
Only the explicitly selected profile controls the base address. Initially
support root-mounted HTTPS origins; reject userinfo, fragments, unexpected
base paths and non-HTTP schemes. Plain HTTP is an explicit loopback development
option; reject remote HTTP. Private-CA deployments install the CA in the
relevant trust stores and test both main and Rust trust paths. Do not override
certificate failures, `NODE_TLS_REJECT_UNAUTHORIZED`, Chromium certificate
errors or CORS. Redirects on authenticated operations fail closed; credentials
never follow a redirect to another origin. This restriction also applies to
the LiveKit endpoint and native helper transport.

## Trusted IPC and profile isolation

| Method family proposed | Input and authority | Result / limits |
| --- | --- | --- |
| Profile list/add/rename/remove/select | User-selected canonical origin; opaque existing profile ID for changes | Safe display metadata only; main binds server/account and increments session generation. |
| Login / challenge / sign-out | Existing schema-derived password/invite/device-link/TOTP operations; secrets accepted only for that operation | Main consumes token fields, returns sanitized account/challenge state, never a bearer. Passwords/reauth proofs are not persisted or logged. |
| API domain operations | Allowlisted operation plus its generated request type | Generated response validation and bounded bytes; credential fields consumed in main, diagnostic errors sanitized. Operator reauth proofs stay main-owned with expiry. |
| Feed subscribe/unsubscribe | Approved feed kind, channel/session/cursor | Main owns socket handles, rate limits, queue bounds and teardown; sends validated safe events. |
| Voice join/leave/mute/deafen/devices/mode | Current profile/channel; explicit Join required | Confirmed helper state, not optimistic transmit state. No renderer PTT-down/up method. |
| Share start/stop/watch | Current room, explicit picker action and user-selected source | Validated native share state; captures scoped to current generation. No arbitrary source ID bypass. |
| Notifications / external link / clipboard attachment | Fixed notification fields or validated URL; explicit user action for clipboard/file reads | No shell, arbitrary path access, credential getter or general IPC forwarding. |

On **every invocation and subscription**, main checks `event.sender` against the
owned webContents, `senderFrame` against its live mainFrame, exact local URL
origin and expected document generation, plus schema/size/rate limits. Reject
subframes, destroyed senders, devtools/other windows and stale handles. The
preload exposes named functions and safe unsubscribe callbacks, never an Electron
event object or caller-chosen channel. Renderer compromise can exercise the
account's allowed UI actions; it must not escape into arbitrary host operations.
Server authorization remains authoritative, including guests and operator TOTP.

Key profiles by a random local ID mapped to canonical origin + verified principal.
Never key credentials by display name or hostname alone. Nonsecret preferences
and drafts are scoped by profile/account; no renderer token storage. Use an
in-memory Electron session partition per active instance/profile generation;
clear it on switch/sign-out. Two instances must use independent data roots and
profile locks, or focus the existing instance for the same root; no competing
credential writers. One helper belongs to one instance/session, never a global
voice singleton. Browser storage policies remain in the browser adapter.

On switch: stop media, close sockets, abort requests, clear renderer state and
release helper before activating the next profile. Every async completion and
feed event carries a main-owned generation check; stale work cannot write into
another profile. Removing a profile deletes its vault entry and scoped cache;
sign-out revokes the credential through the existing API where supported, then
forgets locally even if offline. Explain that offline local deletion cannot
prove server revocation; late 401s affect only their originating generation.

### Credential storage and helper handoff

Main uses Electron safeStorage with encrypted credential files under a private
user data root, mode 0700 directory / 0600 files and atomic replacement. Actual
45.1.0 provider behavior must be measured: a visible Secret Service is not proof
Electron uses protected storage. The tagged docs describe both deprecated sync
and newer async APIs; do not apply the sync backend getter to the async provider
as proof of protection. For the initial spike, explicitly test the documented
sync provider identity (`basic_text` is refused) and round-trip failure behavior;
evaluate async provider assurance separately before choosing it. Unknown,
unavailable, denied, locked or insecure provider means **session-only login**
with a visible storage status and a user retry. Never silently persist the token
using a hardcoded-key fallback. Decrypt errors do not erase other profiles.

Existing v1/v2 can hand an in-memory token to a helper through its private
`CONCH_TOKEN` child environment with `credential=env`; no token appears in start,
argv, files or renderer storage. Isolate `CONCH_CONFIG_DIR` per profile so device
configuration does not cross accounts, and clear inherited `CONCH_*`/Rust debug
variables as the Go supervisor does. Environment handoff is available now but
exposes secrets to same-user process inspection and crash dumps; the same-user
boundary is explicitly outside a keyring's disk-at-rest guarantee. Disable
secret-bearing crash uploads and redact logs. A private one-shot credential
pipe is preferable for a future control version, subject to protocol/security
review; do not invent it in v1/v2 or copy keyring tokens into CLI login files.

## Media topology and required bridge experiment

```text
consented Electron display capture  -- bounded frames --> Rust helper
React watch canvas                 <-- bounded frames -- Rust helper
                                                       |
                                       microphone + PTT gate + audio mixer
                                                       |
                                      one LiveKit connection per room
                                                       |
                                                    LiveKit
```

The helper alone publishes the microphone, screen-video and optional
screen-share-audio tracks on its principal's connection. All remote audio,
including watched share audio, goes through its single playback mixer/output.
Electron plays no remote audio and opens no JS LiveKit room. Use JS SDK only in
the browser adapter. Native UI state must not import the browser `VoiceClient`
and accidentally create another room.

Keep `POST voice/session`, transmit reporting, share claim/stop and presence
semantics intact. Claim after source consent and immediately before publication
so a slow picker does not exhaust the server's 15-second first-track window.
Wait for the helper's actual source permission update; failed/late claims stop
capture and release only the matching claim. On revocation/source closure,
stop tracks and capture locally before reporting stopped. Never publish screen
as microphone to bypass source checks. Watching remote video does not open a
second listen-only participant. Preserve net/audience rules: screen share is
not silently moved to a net room.

The locked SDK exposes `NativeVideoSource::capture_frame`, `I420Buffer`,
`NativeVideoStream` and native audio sources. Those are **source-inspected
candidates**, not evidence of a working Electron bridge. The existing helper
has no screen publication/subscription/rendering interface. Its auto-subscribe
behavior and audio receiver must be audited for separate share audio and
selective watch subscriptions. Do not activate libwebrtc's built-in PipeWire
screen capture given the linked archive's existing PipeWire modification.

Proposed first bridge uses dedicated inherited byte channels, bounded copies,
raw I420 video and 48 kHz audio; never put frames or secrets on the 4096-byte
JSON control channel. Shared semantic handles mean capabilities scoped to the
spawned process, not Linux fd numbers in shared APIs. Linux pipes and Windows
inherited handles/job ownership are separate platform implementations. No
unauthenticated named pipe or localhost listener. Do not introduce unsafe Rust
or a native dependency to avoid the review gates.

Before freezing framing, specify source/track IDs, generation, monotonic
timestamp, format, dimensions, stride and length validation with checked
arithmetic. Proposed ceilings for the **experiment**: 1920x1080 video, two queued
frames per stream, latest-frame replacement, 100 ms queued audio then drop/stop,
and no blocking of the PTT/control thread. Limit number of watched tracks,
reject malformed/oversized frames before allocation, and close on EOF/generation
change. A 1080p I420 stream at 30 fps is about 93 MB/s before extra copies; that
arithmetic is a cost warning, not a performance measurement. Start at 720p30.
No 1080p60 support promise. Measure actual capture/encode/render drops, latency,
CPU/RSS and copies under combined voice, share and second-listener playback.

**Blocking proof:** one Rust microphone + one shared screen + optional shared
audio + one watched screen, with a second listener confirming exactly one
audible voice and no returned remote speech. Include deny/cancel, source close,
slow renderer/helper, owner EOF/crash, device switch, room rotation and revocation.
Probe source permission negotiation using real conchd; direct LiveKit-only tests
cannot establish Conch authorization. Protocol approval precedes building the
new bridge; current source inspection alone cannot unblock #772.

## Global PTT input

| Backend | Selection and limits | Evidence needed |
| --- | --- | --- |
| GlobalShortcuts portal inside helper | Preferred when the actual interface exists and grants a binding; consume Activated **and** Deactivated. Bind only configured actions, verify session/sender/shortcut ID. | Keyboard and Mouse 4 independently, full-screen/game focus, denied binding, conflict, session closure and compositor restart. |
| Explicit per-device native event input | Retain existing reviewed device selection and exact access rules. Mouse event devices can carry button codes; accepting a numeric code is not a hardware test. | Ordinary-user access to selected keyboard/mouse, press/release, grab, unplug/replug; no broad input-group/root requirement. |
| Native X11 input | Separate proposed backend for a genuine X11 session. XWayland DISPLAY in Wayland is insufficient. | Key and button press/release, competing grabs, lock and full-screen on real X11. Any new crate gets owner review. |

Electron 45.1.0 `globalShortcut` documents a pressed callback without a release
callback; registering successfully is insufficient PTT evidence. The portal
has activation/deactivation signals, but does not prove every compositor accepts
mouse triggers. Detect the D-Bus interface/version and actual binding response
each session, not just OS branding or an installed portal package. In the
observed COSMIC session GlobalShortcuts is absent while ScreenCast exists.
This does not predict later COSMIC versions or another portal backend.

Choose one active talk input backend per session; do not OR unrelated presses
or switch to another backend mid-hold. Mute/deafen actions still obey helper
policy. Portal support inside the helper needs an approved D-Bus dependency and
a versioned configuration/capability/status design; **no current wire shape is
changed here**. If main must own a portal session to get a valid parent identity,
that failed helper experiment triggers a new trusted-input version design with
source ownership, sequence/generation, disconnect and fail-closed semantics.

An ordinary release uses the configured audio tail (currently 100 ms default).
Input loss, lock/suspend, owner loss, invalid event ordering or portal session
closure forces the gate shut with no tail and requires a fresh press after
recovery. Existing owner-loss bounds are measured from **detection**: last mic
frame within 100 ms, capture/playback closed within 500 ms, exit within 3 s;
parent checking polls every 250 ms. These are existing protocol requirements,
not measured desktop input latency.

Report the missed-release limit honestly. When an input backend goes silent
without announcing loss, current max transmit defaults to **120 seconds**
(configurable to 600); repeat events cannot prove release or renew a deadline.
Do not advertise this as a 100 ms lost-release guarantee. The desktop proposal
caps a PTT hold at 30 s (shorter existing settings still win), closes and
requires release/fresh press. Selecting and exposing this policy, measuring it
through the new backend and establishing compositor liveness remain a D1 gate.
If no backend can establish safe state, PTT is unavailable; no implicit open-mic
fallback. Tests must inject both detectable device/session loss and a silent
lost release while a second listener measures the last received sound.

## Screen/window and audio capability policy

ScreenCast source flags establish offered source types, not working capture.
Test the actual pinned Electron picker from the local asset origin. Record
whole-screen versus individual-window behavior, cancellation, denial, source
closure, portal/session restart, cursor mode and GPU/driver. Linux PipeWire
`desktopCapturer.getSources` may return one selected source; a rich thumbnail
chooser cannot be assumed. Availability of `getDisplayMedia({audio:true})` is
not evidence of system/application audio.

System/output audio and per-application audio are separate opt-ins and separate
matrix rows. Do not open a generic output monitor automatically: it can include
the native helper's remote participants and return their speech to the room.
Require a demonstrated application/source exclusion route, or mark audio
sharing unsupported while still permitting silent video. Audio loopback must
stop with its source, mute/deafen/share stop and lifecycle policy as designed;
ordinary microphone mute must not accidentally enable a captured monitor.
In particular, decide and display whether microphone mute affects share audio;
proposed behavior: separate share-audio consent/mute, deafen stops local audible
playback, and lock/leave/quit stops both outgoing streams. Test remote speech
alone against a recording/measurement listener before offering loopback.

## Lifecycle and compatibility

| Event | Required behavior proposed |
| --- | --- |
| Launch / login | Text first; no microphone, key monitoring or capture until explicit Join/configuration. |
| Close | Quit by default; stop capture/media, send leave and close control, reap owned helper. |
| Opt-in tray/background | Close hides only while a functioning tray/background indicator and explicit setting exist; show joined/transmitting/sharing state and Quit. Missing tray falls back to quit. |
| Lock / suspend | Fail closed, stop share and leave media; retain text/profile. Resume/unlock requires explicit Join, source selection and fresh press. |
| Renderer crash | Main tears down its media/capture; restart renderer signed in only if vault/session policy allows, never rejoin automatically. |
| Main/helper crash or EOF | Helper fails closed on owner loss; main drops confirmed media state on helper loss and keeps chat working. No auto-restart that captures. |
| Network loss / room rotation | Reuse fresh-session/backoff/revocation policy; gate remains shut until fresh press. Proposal stops screen capture and requires new user consent rather than silently resuming desktop screen share. |
| Sign-out / profile switch | Complete owned-media teardown before new account activation; cancel pending picker/claims and reject stale completions. |

Share stops must use claim identity so a late cleanup cannot stop a newer
share. On ordinary quit use existing supervisor timings: close stdin with
leave, SIGTERM at 4 s, kill/reap owned process at 5 s. Do not kill by executable
name; main crash cleanup and any descendants need their own measured ownership
test. Secure-desktop/lock detection must be backend-specific; inability to
observe lock is a support blocker, not permission to keep transmitting.

Read `/healthz` using its existing schema before login; after login validate
`whoami` and every required response family. Proposed initial compatibility
policy accepts only the server release bundled with the first desktop release
plus explicitly tested patch releases; maintain an explicit allowlist and
fixture matrix, not an assumed `>=` semver range during alpha. Unknown/dev
versions get a clear unsupported-version state and no automatic media/login;
text use requires a separate explicit tested policy. The baseline version here
is a source commit, **not** a supported-server declaration. A compatibility
manifest with real release versions must be filled before #770 acceptance.
Missing optional voice means text remains usable on compatible servers.

Helper negotiation is independent: read its v1 hello, choose the highest
mutually implemented control version and reject incompatible required features
before capture. Current v1/v2 suffice for native voice/per-device PTT, not the
new frame bridge or portal status. Bundle matching releases and report protocol
mismatch explicitly; never infer bridge support from a binary version string.

First package proposal: per-user data, system-installed x86_64 `.deb`, desktop
file/app identity matching the portal identity, sandbox support and bundled
helper. Measure ELF runtime libraries, glibc floor, GPU dependencies, keyring,
PipeWire, installed sandbox behavior and clean-machine upgrade/removal. Current
OS has glibc/package inventory, not an install trial. Do not run with
`--no-sandbox`, root or blanket input access. Manual verified package replacement
first; signing, updater and alternate formats are #774 work after evidence.

## Threats and security review targets

| Threat | Boundary / required verification |
| --- | --- |
| Malicious server messages, attachments or links compromise renderer | Sandboxed local code, CSP, no remote navigation, inert rendering and strict blob handling; compromised renderer still cannot retrieve bearer or invoke general host operations. |
| Subframe/foreign window calls preload methods | Exact webContents/mainFrame/local-origin/generation validation on every operation and subscription. Negative packaged tests required. |
| Server A response or feed lands in account B | Abort + generation fences + isolated vault/cache/socket/media handles; two-server delayed-response test. |
| Credential leaks via redirect, logs, diagnostics, dumps or argv | Fixed destination, no redirects for auth calls, safe errors, vault ownership and reviewed child environment; scan artifacts for canary credentials. |
| UI lies about mute or native helper is stalled | Helper is source of confirmed gate/device state; bounded control; fail closed on loss, no renderer gate-open API. |
| Forged/malformed/slow local media | Private inherited capabilities, validation before allocation, bounded queues, no PTT-thread blocking; corrupt-frame/EOF/stall test. |
| Screen/audio consent outlives user intent | Current-generation consent, immediate track stop, matching-claim cleanup and explicit resume. No remote participant feedback. |
| Another local user steals credentials or helper | Private roots/files, trusted bundled executable/ownership checks and process-scoped handles. Same-user/root compromise is outside the vault guarantee. |
| New portal or packaging dependency weakens isolation | Dependency/licence/unsafe review; no sandbox disabling or broad input access. |

Security review is required before accepting auth/IPC/media contracts and before
merging their implementations. Nick signs any ADR, protocol version or new
dependency decision required by ADR-000/006. This draft requests no implicit
approval through prose.

## Remaining gates and issue ownership

| Gate | Owner / next action | Children held |
| --- | --- | --- |
| Electron candidate and build/runtime/licence review | Nick approves dependency choice; D1 runs pinned packaged local-origin/picker/keyring probe. | #770–#774 |
| Single-native-participant frame bridge | D1 + protocol owner define approved framing, run one-room combined media/listener proof; revise design if it fails. | #770 shared media interface, #771/#772 |
| Portal/helper input and fail-closed policy | D1 tests identity/parenting + key/button release on an available portal desktop; approves backend dependency/status/input design. | #771, lifecycle parts of #773 |
| Credential/transport/IPC threat review | Security reviewer verifies public-route mappings, auth response sanitization, trust paths and cross-profile isolation. | #770 |
| Windows coordination | #775 reviews byte-channel/handle, media ownership, native output and lock semantics. No Linux fd/process-group assumptions in shared interfaces. | Shared interface freeze in #770 |
| Real-device acceptance | Run report's listener and hardware scenarios; preserve pending machines explicitly. | Support claims / #774 |

No child becomes Ready based solely on this document. Record accepted decisions
and their evidence in #769; this draft can be reviewed while the hardware and
dependency gates remain pending.

## Primary references

Accessed 2026-10-28. Specifications document mechanisms; capability claims require
the measured versions and experiments in the report.

- [Electron 45.1.0 release](https://github.com/electron/electron/releases/tag/v45.1.0)
- [45.1.0 globalShortcut](https://github.com/electron/electron/blob/v45.1.0/docs/api/global-shortcut.md)
- [45.1.0 safeStorage](https://github.com/electron/electron/blob/v45.1.0/docs/api/safe-storage.md)
- [45.1.0 desktopCapturer](https://github.com/electron/electron/blob/v45.1.0/docs/api/desktop-capturer.md)
- [45.1.0 Session / capture permissions](https://github.com/electron/electron/blob/v45.1.0/docs/api/session.md)
- [Electron security guidance](https://www.electronjs.org/docs/latest/tutorial/security)
- [GlobalShortcuts protocol](https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.GlobalShortcuts.html)
- [ScreenCast protocol](https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.ScreenCast.html)
- [LiveKit Rust SDK 0.9.4](https://docs.rs/livekit/0.9.4/livekit/)
- [Locked libwebrtc 0.3.51 video source](https://docs.rs/libwebrtc/0.3.51/libwebrtc/video_source/native/struct.NativeVideoSource.html)
