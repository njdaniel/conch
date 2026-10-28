# Linux desktop D1 capability report

Issue [#769](https://github.com/njdaniel/conch/issues/769) · 2026-10-28 · Baseline
`cace5b0331ad65a4d2f2e11ef782b527346748a1`. Design: [linux-desktop.md](../design/linux-desktop.md).
Draft by Codex (PR #887); revised by the principal to the reconciled media model and the
shared transport contract. The inventory, the receipts and the synthetic probe below are the
draft's and are unchanged.

**Disposition: investigation started; architecture and hardware gates remain open. No
supported desktop configuration is declared.** No owner gaming, physical-key, audible-listener,
keyring or package-install acceptance was performed. Agent commands accessed the exposed local
session for read-only inventory; that is not equivalent to Nick testing the client on his
machine, and no input or capture probe that needs a person at the keyboard was run.

## Evidence definitions

- **Observed:** direct read-only runtime result for the recorded session.
- **Synthetic:** real processes, network and media with generated input; no physical device or
  human listener. It can refute a topology; it cannot certify push-to-talk.
- **Source-inspected:** code or a tagged upstream API; no runtime capability claim.
- **Verified-by-docs:** a cited official page states the mechanism; a statement about the API,
  not about a pinned Conch build on a real machine.
- **Unavailable:** the probed interface or backend is absent or inaccessible in this environment.
  Permission denial is not proof of absence.
- **Pending:** the experiment has not run. It is not an unsupported result.
- **Supported / unsupported:** reserved for an explicit product decision after the full
  acceptance scenario; neither label is assigned by this report except where the design says
  "unsupported by policy".

## Reproduction and retained receipts

The [inventory script](../design/spikes/linux-desktop/inventory.py) uses Python 3 stdlib and
existing system commands only. It opens no input device or capture stream and reads no stored
secret. It records counts rather than input paths, and connection success rather than the audio
graph or device names. It does not install dependencies or alter desktop settings. Run from the
issue worktree:

```sh
python3 docs/design/spikes/linux-desktop/inventory.py > /tmp/conch-desktop-inventory.json
```

To record a helper build version, optionally pass `--helper` with the absolute path of a
trusted, locally built `conch-voice`; the script invokes `--version` only. The first inventory
was taken before this worktree's helper was built, so its helper result is `missing_tool`; a
separate `inventory-built-helper.json` records the rebuilt worktree helper, and the original
receipt is preserved.

The sandbox run denied session-bus and PipeWire access and hid input nodes. A second run with
host read access successfully introspected the session. **Only that successful introspection
supports the GlobalShortcuts absence finding.** Retained receipts under
[linux-desktop evidence](linux-desktop/) record both contexts. No hostname, Bluetooth address,
collection contents, token or full audio graph is retained.

Existing SDK experiment (no new production or prototype dependencies):

```sh
go run ./e2e/sdkprobe -only reasons -reps 1 -raw /tmp/conch-desktop-sdk
```

It builds its Rust example with the repository's pinned toolchain, starts an isolated LiveKit
1.13.7 container using the existing digest pin, generates audio, and tears down its own
processes and container. It does not open the microphone or contact an existing Conch server.
Docker access is required; without it, record the experiment as blocked. The reasons probe
includes a 90-second server pause; allow several minutes. The duplicate case is Rust+Rust, not
an Electron+Rust combined-media test.

## Environment observed

| Component | Version / observation |
| --- | --- |
| OS / architecture | Pop!_OS 24.04 LTS, x86_64 |
| Kernel | `7.0.11-76070011-generic` |
| Session | `XDG_CURRENT_DESKTOP=COSMIC`, `XDG_SESSION_TYPE=wayland`; both `DISPLAY` and `WAYLAND_DISPLAY` present |
| Compositor | `cosmic-comp 0.1~1782837583~24.04~bb584aa` |
| Session package | `cosmic-session 1.0.0~1782340586~24.04~308c9d3` |
| Portal frontend | `xdg-desktop-portal 1.18.4-1ubuntu2.24.04.2` |
| COSMIC backend | `xdg-desktop-portal-cosmic 0.1.0pop1~1782500323~24.04~2948dd4` |
| GTK backend | `xdg-desktop-portal-gtk 1.15.1-1build2` |
| PipeWire | `1.6.8-1pop1~1787688714~24.04~fdff050`; runtime connection observed |
| WirePlumber | `0.5.12-2pop1~1766191027~24.04~d4bf441`; runtime connection observed |
| Keyring | GNOME Keyring `46.1-2ubuntu0.2`, libsecret `0.21.4-1build3`; Secret Service interface observed, no secret operation attempted |
| Native SDK / probe | `livekit 0.9.4`, `libwebrtc 0.3.51`, linked `webrtc-89d790b`, from lockfile and toolchain; Rust example rebuilt in this worktree |
| Rebuilt helper | `conch-voice 0.1.0 (48000 Hz, 10 ms frames)` from this worktree's debug binary; not an installed package |
| Electron / Chromium | Not run. Investigation candidate Electron `45.1.0` (Chromium 156.0.8078.49, Node 24.22.1); actual packaged runtime pending |
| Input permissions | Host-visible 21 event nodes; zero readable by this process. No node opened, no permission changed. `DISPLAY` is not proof of an X11 session |

## Findings

### F1 — Two connections as the person cannot be the desktop media model; a distinct identity can

**Source-inspected:** `voiceIdentity` gives one principal exactly one identity in a room;
`parseVoiceIdentity` accepts no suffix. The join token initially grants only microphone
publication; screen sources are added to the existing participant by the share-claim path. Rust
and browser code both recognise duplicate-identity disconnects; the Rust policy stops on them.
**conchd itself does not refuse a second session**: `voiceSession` signs a token on every request
(`TestVoiceSessionRecordsTheHolder` expects 200 for "the same session again"). The limit is
LiveKit's.

**Synthetic, real LiveKit:** one second connection with the same identity disconnected the first
with `DUPLICATE_IDENTITY` **507 ms after the second `Room::connect` call** (`n=1`,
`duplicate-first.log` and `duplicate-second.log`; the retained timestamps reproduce 507.487 ms).
The second remained connected over the probe's 5-second window. The other cases of the same run:

| Cause | Observation, one sample each |
| --- | --- |
| Room deletion | `ROOM_DELETED` disconnect after 8.60 ms |
| Participant removal | `PARTICIPANT_REMOVED` disconnect after 9.34 ms |
| Server frozen 90 s then resumed | Reconnecting after 13.0 s; reconnected after 90.1 s |
| Forced server shutdown | `SERVER_SHUTDOWN` disconnect after 0.26 s |
| Docker stop, 10 s grace | Reconnecting after 10.0 s; unknown-reason disconnect after 33.4 s |
| Server killed | Reconnecting after 0.02 s; unknown-reason disconnect after 19.2 s |

Housekeeping: eight probe processes, no token, API-secret or room-name leak detected, no
labelled container remaining. Native stdout printed `Nvidia Decoder is supported.` eight times,
which is the evidence behind keeping events off standard output (design §5.2, option C).

**Action (revised):** the design adopts **model B** on both platforms (design §6): the helper
stays the one voice participant, and Electron publishes screen media as a **companion
participant under a distinct identity that conchd issues** (#897), so that no second connection
is ever made as the person. The draft's sole-participant frame bridge (model A) is kept as the
considered alternative with what would revive it (design §6.2). The 507 ms receipt is the
argument for a distinct identity, not for a bridge. Rows L-M1 to L-M4 are the experiments that
remain.

### F2 — Portal PTT is unavailable in the observed session

**Observed:** successful frontend introspection advertises ScreenCast v5, source flags `3`
(monitor + window), cursor flags `3` (hidden + embedded), and Secret v1. It does **not**
advertise GlobalShortcuts. An explicit `Properties.Get` for GlobalShortcuts `version` returned
`No such interface`. No shortcut session was created because the interface is absent. No claim
is made about newer COSMIC, GNOME or KDE sessions, or about mouse support.

**Source-inspected:** Electron 45.1.0's `globalShortcut` exposes an activation callback, not the
release events a held control needs. The portal specification defines `Activated` and
`Deactivated`, which makes a helper portal backend a candidate on other desktops; it does not
establish Mouse 4 support.

**Action:** retain the explicit per-device input; this process has no read access to the event
nodes, and the inventory changed no group, ACL or rule. Selected-device ordinary-user access is
tested with Nick at the keyboard (L-P1 to L-P6); a portal-equipped desktop is a pending machine
(L-P7); X11 needs a genuine X11 session, not XWayland here (L-P8).

### F3 — Lost-release bounds need a desktop decision

**Source-inspected:** the owner-loss contract allows 100 ms to the final microphone frame, 500 ms
to capture and playback closure, and 3 s to process exit, counted from detection (parent polling
every 250 ms). These bounds do not describe a silently missed release. Configuration defaults
are a 100 ms release tail and 120 s maximum continuous transmit, configurable up to 600 s.

**Action:** the design carries the draft's **30 s** desktop cap as a **shared default on both
platforms for Nick to decide** (design §7.1, L-F9); shorter configured values still win; a lost
input session or device closes the gate with no tail; repeats never renew a deadline. The new
backends' bounds and the second-listener measurement remain hardware work (L-P4, L-P6).

### F4 — Capture and protected storage remain unverified

**Observed inventory:** ScreenCast, Secret Service and audio services exist. **Not run:** the
Electron picker, display or audio capture, vault encryption, lock and unlock, the keyring's
denied, locked and insecure fallbacks, package installation. A portal's presence cannot prove
working application audio or the absence of remote-participant feedback.

**Action:** a pinned, packaged Electron spike from the local origin (L-F11; rows L-S1, L-C1) before
any capture or credential claim. No new dependency or protocol as an unreviewed experiment.

### F5 — An Electron owner cannot drive today's helper over fd 3 (desk finding, this revision)

**Source-inspected:** libuv creates every `'pipe'` stdio entry on Unix with
`uv_socketpair(SOCK_STREAM, …)` and `dup2`s it to the child's fd N
([libuv `src/unix/process.c`](https://raw.githubusercontent.com/libuv/libuv/v1.x/src/unix/process.c));
Node documents that these "are not actual Unix pipes" ([Node child_process](https://nodejs.org/api/child_process.html)).
The helper's `open_event_pipe` (`voice/conch-voice/src/managed.rs`) requires
`/proc/self/fd/3` to be a FIFO before and after reopening it. A socketpair fails the first check.
The Go supervisor is unaffected: it hands over real `os.Pipe()` FIFOs.

**Action:** the design proposes an owner-created Unix socket under `$XDG_RUNTIME_DIR` (mode
0700 by specification) that the helper connects to with safe `UnixStream::connect`, with the
attach line already proposed for Windows (#893) extended to Linux (design §5.2, option B); fd 3
stays for the Go TUI. Row L-X1 is the experiment that confirms both the refusal and the fix.

## Pending capability matrix

Rows carry ids used by the design's §13 and §14. Columns are the desktops #769 names; only tested
combinations ever become supported, and GNOME, KDE and X11 cells are filled on those machines,
never inferred from this COSMIC session. `I` = inventory only, `U` = unavailable in this session,
`P` = pending real trial, `D` = verified-by-docs or source-inspected (mechanism only).

| Id | Capability | Mechanism | Pop!_OS COSMIC Wayland (above) | GNOME Wayland | KDE Wayland | Native X11 | Experiment |
| --- | --- | --- | --- | --- | --- | --- | --- |
| L-X1 | Electron owner drives the helper: command channel on stdin, events on an absolute-path Unix socket under a verified `$XDG_RUNTIME_DIR` (lstat: directory, own uid, `mode & 0o077 == 0`; 0o700 profile directory; abstract names refused; `sun_path` budget) with the attach line | design §5.2 | D (socketpair refusal; `UnixStream::connect`; 0700 runtime dir by specification) / P | P | P | P | LE1 |
| L-X2 | Owner loss by EOF on stdin, EOF or **any received byte** on the events socket, or parent change; bounds met. **Stdin is a libuv socketpair whose write end lives in Electron main beside many Chromium children**: measured for a leak that would keep the helper's stdin open after main dies | design §5.3 | D / P | P | P | P | LE1 |
| L-X3 | Owner stop: `leave`, close, `SIGTERM`, `SIGKILL`, reap; no `detached`; nothing killed by name | design §5.3 | D / P | P | P | P | LE1 |
| L-X4 | Child environment from an allowlist; no token in argv, logs or renderer | design §4.1 | D / P | P | P | P | LE1 |
| L-M1 | Second connection as the person displaces the first | F1 | **Synthetic: 507 ms, n=1** | — | — | — | done (reproduce with LE2) |
| L-M2 | Companion under a derived identity stays up beside the helper; folded into the person by conchd | design §6, #897 | P (needs #897 prototype) | — | — | — | LE2 |
| L-M3 | One voice playback: the companion receives no voice (B1 enforced by grant; B2 by `autoSubscribe: false`) | design §6, #906 | P | — | — | — | LE2 |
| L-M5 | The companion grant is `["screen_share"]` only, 15 s, `canPublishData: false`; `screen_share_audio` arrives only by permission update on main's request and leaves on stop; a second companion request answers 409; an orphaned companion is removed after the poller's grace; the renderer holds nothing but this grant | design §6.4, #897 | P (needs #897 prototype) | — | — | — | LE2 |
| L-M4 | One share-audio playback: a remote share's sound heard once; the helper never plays its own companion | design §6, #906 | P | — | — | — | LE2 |
| L-P1 | Keyboard PTT press and release, another app focused, evdev backend | design §7 | U portal; P selected-device (0 readable nodes) | P | P | P | LE3 |
| L-P2 | Mouse 4 PTT press and release, another app focused, evdev backend | design §7 | U portal; P selected-device | P | P | P | LE3 |
| L-P3 | Keyboard and Mouse 4 with a windowed, borderless and fullscreen game focused | design §7 | P | P | P | P | LE3 |
| L-P4 | Conflicting or grabbed input; silently lost release bounded by the desktop default | design §7.1 | P | P | P | P | LE3 |
| L-P5 | Device unplug while held: gate shut, `ptt: lost`, fresh press needed | existing `keydev.rs` rule | D / P | P | P | P | LE3 |
| L-P6 | Repeats never hold or reopen; unknown initial state is not a press | design §7.1 | D / P | P | P | P | LE3 |
| L-P7 | GlobalShortcuts portal backend: `Activated`/`Deactivated`, session ownership by the helper, mouse triggers | design §7 | **U** (not advertised; `No such interface`) | P | P | — | LE4 |
| L-P8 | Native X11 backend | design §7 | — (Wayland session) | — | — | P | LE5 |
| L-L1 | Lock and suspend close transmission and capture; resume needs fresh intent; logind `Lock`/`LockedHint`/`PrepareForSleep` observed | design §9 | D / P (does COSMIC set `LockedHint`?) | P | P | P | LE6 |
| L-L2 | Close quits; tray opt-in; missing tray never hides a session | design §9 | P | P | P | P | LE6 |
| L-L3 | Renderer, helper and owner crashes each fail closed; no automatic rejoin or capture | design §9 | P | P | P | P | LE1, LE6 |
| L-S1 | Screen or window picker and capture from `conch-app://ui/` in the pinned Electron; single source via PipeWire | design §6.3, §8 | I (ScreenCast v5, sources 3) / P | P | P | P | LE7 |
| L-S2 | Cancel, deny, source closure, portal restart, cursor mode | design §8 | P | P | P | P | LE7 |
| L-S3 | Share published by the companion, watched by browser, TUI-less Linux and Windows peers; claims resolve to the person | design §6.4 | P (needs #897) | — | — | — | LE2, LE7 |
| L-S4 | Delivered size, fps, latency, CPU and GPU under voice plus share plus watch; start at 720p30 | design §8 | P | P | P | P | LE7 |
| L-S5 | Selected-application audio with remote-speech exclusion | design §8 | P; not offered until demonstrated | P | P | P | LE8 |
| L-S6 | Everything-but-the-call audio; a bare output monitor is **unsupported by policy** | design §8 | P; feedback test with a measurement listener | P | P | P | LE8 |
| L-C1 | Protected credential storage: the pinned Electron's actual provider; session-only fallback on `basic_text`, locked or denied | design §4.1 | I (Secret Service, Secret portal v1) / P | P | P | P | LE7 |
| L-C2 | Trusted IPC: subframe, foreign window, stale generation and malicious link rejected; two-server stale responses fenced; the share/watch methods return the companion grant and nothing else; `connect-src` is exactly the profile's LiveKit origin per load | design §3, §4 | P (packaged negative tests) | — | — | — | LE7 |
| L-C3 | No disabled `webSecurity`, CORS or certificate validation; private CA through trust stores | design §3 | D / P | P | P | P | LE7 |
| L-D1 | x86_64 `.deb` install, upgrade, removal on a clean machine; portal identity; sandbox on | design §9 | I (glibc and package inventory) / P | P | P | P | LE9 |
| L-D2 | Microphone and output switch, USB and Bluetooth changes, under the helper | existing v2 rules | I / P | P | P | P | LE3 |
| L-D3 | Server compatibility allowlist shown; unknown version refused clearly | design §9 | P | — | — | — | LE7 |

There are no supported rows yet.

## Experiments (the hardware and spike half of #769, L-F14 in the design)

Record date, commit, package and helper build, Electron and Chromium (`process.versions`),
compositor and session, portal backend and interface versions, PipeWire, GPU and driver, and
device classes per trial. Keep tokens, desktop contents, conversations and input device
identifiers out of public receipts. Use a private test server and a distinct second listener.
Only an inventory may be run without a person present; every other experiment needs Nick at the
keyboard or a second listener.

| Id | Experiment | Settles |
| --- | --- | --- |
| LE1 | A Node script spawns today's helper with a fourth `'pipe'` stdio entry and records the `EventPipe("is not a pipe")` exit (the finding of F5); then, with the `--events-pipe` helper, a Unix socket under a verified `$XDG_RUNTIME_DIR`: attach line, hello, one state line every 100 ms for a minute while the EOF read is pending, then owner exit; a byte written to the helper by a stand-in owner (expected: owner loss); an abstract-namespace name and a relative path (expected: refused); a symlink at the socket path (expected: refused by `symlink_metadata`); measure gate shut, capture stop and exit against the bounds; stop escalation with a hung helper; the allowlisted environment; from a second user, connect to the socket path (expected: refused by the directory mode); **kill main while Chromium children are alive and record whether the helper's stdin stays open** (the socketpair leak) | L-X1 to L-X4, L-L3 |
| LE2 | Against current conchd and LiveKit with the draft's probe harness: reproduce L-M1; then, with a prototype companion grant (#897) and the subscription rule (#906): both connections stay up; the companion receives no voice; a remote share's sound is heard once; the helper does not play its own companion; share claims and stops resolve to the person; the companion token carries `["screen_share"]` only and `canPublishData: false`; a publish of a synthesised audio track from the companion is **refused by LiveKit** until main's permission request, and refused again after stop; a second companion request answers 409; an orphaned companion disappears within the poller's grace; `hidden` participant tracks subscribable or not | L-M1 to L-M5, L-S3 |
| LE3 | With Nick at the keyboard and a second listener, evdev backend after a one-device udev grant: keyboard and Mouse 4 press and release in an editor, another app, a fullscreen app and a game; grabs; unplug while held; repeat storms; helper started with the key held; hold past the desktop default; USB and Bluetooth device changes | L-P1 to L-P6, L-D2 |
| LE4 | On a desktop that advertises GlobalShortcuts (GNOME or KDE machine): whether the helper, windowless, can create the session; `Activated` and `Deactivated` for a key and for Mouse 4; denied binding; conflict; session closure and compositor restart | L-P7 |
| LE5 | On a genuine X11 session: press and release, grabs, lock, fullscreen | L-P8 |
| LE6 | Lock and suspend while held and sharing, wake still held, release; `LockedHint` and `PrepareForSleep` as seen by main; close, tray present and absent, Quit; renderer, helper and main killed separately | L-L1 to L-L3 |
| LE7 | The packaged pinned Electron from `conch-app://ui/`: picker, screen then window capture, cancel, deny, source closure, portal restart; `safeStorage` provider and its fallbacks; packaged negative IPC tests; certificate and CORS behaviour against a private-CA server; compatibility states; delivered size, fps, latency, CPU and GPU with voice, share and watch | L-S1, L-S2, L-S4, L-C1 to L-C3, L-D3 |
| LE8 | Audio scopes: play remote speech alone and record what a would-be application or exclusion route returns; any returned remote speech fails; stop on consent and source loss; deafen and share-audio mute | L-S5, L-S6 |
| LE9 | Clean-machine `.deb` install, upgrade, removal on each declared target; runtime libraries, glibc floor, portal identity, sandbox, profile retention | L-D1 |

## Security review

The security-reviewer agent reviewed the draft, the inventory and the retained identity
receipts on 2026-10-28: **PASS for the draft planning/spike change**, with no blocking findings.
That pass predates this revision's changes to the media model (§6) and the transport (§5); both
reuse contracts whose own reviews are owed under #893 and #897. The security review of the
revised design (2026-10-28, request changes) found two false authority statements, both fixed:
the renderer does hold a LiveKit credential under model B, the companion grant, and nothing else
(design D1-2, §4, §6); and share audio must never be in that grant, only added by conchd's
permission update on main's request after consent (design §6.4, a requirement on #897). The
threat table (design §10) now names: a compromised renderer publishing synthetic media as the
person's screen; a compromised renderer subscribing to voice under B2; share audio published
without a press; the events channel observed or hijacked; a second connection as the person;
share audio returning the room. The transport gained verified runtime-directory checks, abstract
sockets forbidden, bind-then-retry instead of unlink-then-bind, and received bytes as owner loss.
A re-check of the amended text is requested before acceptance. None of this is approval of
dependency choices, new wire contracts or implementation readiness.

## Actionable blockers and handoff

| Blocker | Concrete next result | Scope owner |
| --- | --- | --- |
| B1: Electron not approved or installed for the spike | Approve the exact candidate, licence and build route; then packaged local-origin, picker and keyring receipts with the actual Chromium version (LE7) | Nick; #770 (L-F11) |
| B2: media model not yet signed off | Nick accepts model B (or directs otherwise); #897 and #906 become Ready; LE2 runs with the prototype grant | #769 with #775's merged design; #897, #906 |
| B3: no GlobalShortcuts here, no event-node access | LE3 on this machine with Nick (one-device udev grant); LE4 on a portal desktop; decide the portal and X11 crates and the shared missed-release default | #771 (L-F6, L-F7); L-F9 |
| B4: Electron owner transport | The helper's `--events-pipe` on Linux and the owner rules in main (L-F2, L-F3); LE1 | #893, #770 |
| B5: shared auth, IPC and version contract unreviewed | Security review of route mappings, auth response sanitisation, trust paths and cross-profile isolation; the compatibility allowlist (L-F13) | #770 |
| B6: other desktops, packaging and audio routes untested | LE4 to LE9 on GNOME Wayland, KDE Wayland and X11; `.deb` lifecycle; audio scopes | #771, #772, #774; L-F14 |

This report does not close #769 or change roadmap or issue readiness. New schemas, dependencies
and ADR amendments still require the established owner decisions.

## Review and repository verification (the draft's, retained)

`go test ./...` passed. The first unmodified `make check` passed formatting, invisible-character
checks, vet, lint, Go tests, schema compatibility, TS schema generation and dependency gates,
then failed deployment-unit verification:

```text
livekit.service: Failed to create livekit.service/start: Unit home.mount not found.
```

Classification: **environment**. This machine's Docker systemd drop-in requires `/home` mounts
and a host-specific verification service; the deployment fixtures do not supply them. No
production or test source was changed to hide the failure. The entire deployment suite passed
against vendor systemd units with:

```sh
SYSTEMD_UNIT_PATH=/usr/lib/systemd/system:/run/systemd/generator:/run/systemd/generator.late make deploy-test
```

Rust and web gates were also run separately to cover the targets the original `make check` did
not reach. The standard Rust suite retains **11 pre-existing ignored tests**: one
real-server/two-login listener test, seven opt-in PipeWire or by-hand tests (including the
ten-minute pacing trial), and three real-server/stored-login API tests. None was added, deleted
or newly skipped. These are pending evidence, not desktop capability passes.

The full `make check` **passed** with the same `SYSTEMD_UNIT_PATH` adjustment (all targets, no
newly skipped tests). The separate Rust and web checks passed; web reported 58 test files and
1952 tests passing. Inventory syntax, local document links and retained JSON receipts were
checked, and the retained probe timestamps independently reproduce a 507.487 ms duplicate
disconnect delay.

The first `go test -race ./...` failed only in `cmd/conch`: its nested `go build` could not
obtain VCS status (Git exit 128), and the shared build-once failure cascaded through CLI process
tests. No race-detector warning was emitted. An isolated rerun, **five CLI race repetitions**,
and the next complete race suite passed. Classified as an intermittent test-build/tooling
failure with cause unconfirmed; tracked separately as
[#886](https://github.com/njdaniel/conch/issues/886). No CLI code, VCS stamping, test or skip
policy was changed in #769.

This revision changed documents only; no build or test was rerun for it.
