# Linux desktop D1 capability report

Issue [#769](https://github.com/njdaniel/conch/issues/769) · 2026-10-28 · Baseline
`cace5b0331ad65a4d2f2e11ef782b527346748a1`.
Design: [linux-desktop.md](../design/linux-desktop.md).

**Disposition: investigation started; architecture and hardware gates remain
open. No supported desktop configuration is declared.** No owner gaming,
physical-key, audible-listener or package-install acceptance was performed.
Agent commands accessed the exposed local session for read-only inventory;
that is not equivalent to Nick testing the client on his machine.

## Evidence definitions

- **Observed:** direct read-only runtime result for the recorded session.
- **Synthetic:** real processes/network/media with generated input, no physical
  device or human listener. This can refute a topology; it cannot certify PTT.
- **Source-inspected:** code or tagged upstream API; no runtime capability claim.
- **Unavailable:** the actual probed interface/backend is absent or inaccessible;
  scope the reason to this environment. Permission denial is not proof of absence.
- **Unverified / pending:** experiment not run. It is not an unsupported result.
- **Supported / unsupported:** reserve for an explicit product support decision
  after the full acceptance scenario; neither label is assigned by this report.

## Reproduction and retained receipts

The [inventory script](../design/spikes/linux-desktop/inventory.py) uses Python 3
stdlib and existing system commands only. It opens no input device or capture
stream and reads no stored secret. It records counts rather than input paths,
and connection success rather than the audio graph/device names. It does not
install dependencies or alter desktop settings. Run from the issue worktree:

```sh
python3 docs/design/spikes/linux-desktop/inventory.py > /tmp/conch-desktop-inventory.json
```

To record a helper build version, optionally pass `--helper` with the absolute
path of a trusted, locally built `conch-voice`; the script invokes `--version`
only. A missing binary is reported, not built or downloaded. The first inventory
was taken before this worktree's helper was built, so its helper result is
`missing_tool`. A separate `inventory-built-helper.json` records the rebuilt
worktree helper; the original receipt is preserved. Synthetic probe and
lockfile versions are recorded separately.

The sandbox run denied session-bus/PipeWire access and hid input nodes. A second
run with host read access successfully introspected the session. **Only that
successful introspection supports the GlobalShortcuts absence finding.** Retained
receipts under [linux-desktop evidence](linux-desktop/) record both contexts;
their timestamps distinguish inventory from later tests. No hostname, Bluetooth
address, collection contents, token or full audio graph is retained.

Existing SDK experiment (no new production or prototype dependencies):

```sh
go run ./e2e/sdkprobe -only reasons -reps 1 -raw /tmp/conch-desktop-sdk
```

It builds its Rust example with the repository's pinned toolchain, starts an
isolated LiveKit 1.13.7 container using the existing digest pin, generates audio,
and tears down its own processes/container. It does not open the microphone or
contact an existing Conch server. Docker/socket access is required; without it,
record the experiment as blocked rather than fabricate a pass. The reasons
probe includes a 90-second server pause; allow several minutes. Raw probe lines
are not the same as a full command transcript: the retained notes below include
the harness's timing/cleanup results. The duplicate case is Rust+Rust, not an
Electron+Rust combined-media test.

## Environment observed

| Component | Version / observation |
| --- | --- |
| OS / architecture | Pop!_OS 24.04 LTS, x86_64 |
| Kernel | `7.0.11-76070011-generic` |
| Session | `XDG_CURRENT_DESKTOP=COSMIC`, `XDG_SESSION_TYPE=wayland`; both DISPLAY and WAYLAND_DISPLAY present |
| Compositor | `cosmic-comp 0.1~1782837583~24.04~bb584aa` |
| Session package | `cosmic-session 1.0.0~1782340586~24.04~308c9d3` |
| Portal frontend | `xdg-desktop-portal 1.18.4-1ubuntu2.24.04.2` |
| COSMIC backend | `xdg-desktop-portal-cosmic 0.1.0pop1~1782500323~24.04~2948dd4` |
| GTK backend | `xdg-desktop-portal-gtk 1.15.1-1build2` |
| PipeWire | `1.6.8-1pop1~1787688714~24.04~fdff050`; runtime connection observed |
| WirePlumber | `0.5.12-2pop1~1766191027~24.04~d4bf441`; runtime connection observed |
| Keyring | GNOME Keyring `46.1-2ubuntu0.2`, libsecret `0.21.4-1build3`; Secret Service interface observed, no secret operation attempted |
| Native SDK / probe | `livekit 0.9.4`, `libwebrtc 0.3.51`, linked `webrtc-89d790b`, from lockfile/toolchain; Rust example rebuilt in this worktree |
| Rebuilt helper | `conch-voice 0.1.0 (48000 Hz, 10 ms frames)` from this worktree's debug binary; not an installed desktop package |
| Electron / Chromium | Not run. Investigation candidate Electron `45.1.0`; actual Chromium and packaged runtime requirements pending |
| Display/input permissions | Host-visible 21 event nodes; zero readable by this process. No node opened and no permission changed. DISPLAY is not proof of an X11 session. |

## Findings

### F1 — Two participant connections cannot be the desktop media model

**Source-inspected:** `voiceIdentity` gives one principal exactly one identity in
a room; `parseVoiceIdentity` accepts no suffix. The join token initially grants
only microphone publication. Screen sources are added to the existing
participant by the public share-claim path. Rust and browser code both recognize
duplicate-identity disconnects; the Rust policy stops on them.

**Synthetic, real LiveKit:** one second connection with the same identity
disconnected the first with `DUPLICATE_IDENTITY` **507 ms after the second
Room::connect call** (`n=1`). The second remained connected over the probe's
5-second observation window. This is a single measurement, not a latency bound
or Electron-specific measurement. The following additional cases ran:

| Cause | Observation, one sample each |
| --- | --- |
| Room deletion | `ROOM_DELETED` disconnect after 8.60 ms |
| Participant removal | `PARTICIPANT_REMOVED` disconnect after 9.34 ms |
| Server frozen 90 s then resumed | Reconnecting after 13.0 s; reconnected after 90.1 s |
| Forced server shutdown | `SERVER_SHUTDOWN` disconnect after 0.26 s |
| Docker stop, 10 s grace | Reconnecting after 10.0 s; unknown-reason disconnect after 33.4 s |
| Server killed | Reconnecting after 0.02 s; unknown-reason disconnect after 19.2 s |

Housekeeping: eight probe processes, 140 lines, largest clock disagreement
0.002 ms, no token/API-secret/room-name leaks detected, no labeled container
remaining. Native stdout printed `Nvidia Decoder is supported.` eight times,
reinforcing the existing private-event-channel requirement. The reason phase
took 3m15s, full command 4m9s. These observations do not measure actual speaker
silence, source teardown, Conch authorization or helper owner death.

**Action:** proposed sole Rust participant with Electron capture/render frame
bridge. It preserves identity/source grants and native microphone/playback
ownership. Bridge transport, source permission events and combined media are
not demonstrated; that blocker must be resolved before interface freeze.

### F2 — Portal PTT is unavailable in the observed session

**Observed:** successful frontend introspection advertises ScreenCast v5,
source flags `3` (monitor + window), cursor flags `3` (hidden + embedded), and
Secret v1. It does **not** advertise GlobalShortcuts. An explicit Properties.Get
for GlobalShortcuts `version` returned `No such interface`. No shortcut session
was created because the interface is absent. No claim is made about newer
COSMIC, GNOME/KDE sessions or mouse support.

**Source-inspected:** Electron 45.1.0 shortcuts expose an activation callback,
not the release events required for a held PTT control. The portal specification
defines Activated and Deactivated. That makes a direct helper portal backend a
candidate on other desktops; it does not establish Mouse 4 support.

**Action:** retain explicit per-device input, but the current process has no
read access to its event nodes. Do not change groups, ACLs or install a root
helper for the spike. Test selected-device ordinary-user access with Nick,
and schedule a portal-equipped desktop. X11 needs a genuine X11 session, not
XWayland in this session.

### F3 — Lost-release time bounds need a desktop decision

**Source-inspected:** the helper owner-loss contract allows 100 ms to the final
microphone frame, 500 ms to capture/playback closure, and 3 s to process exit,
counted from detection (parent polling is 250 ms). These bounds do not describe
a silent missing key release. Config defaults are 100 ms release tail and
120 s max continuous transmit, configurable up to 600 s.

**Action:** propose a desktop maximum held PTT duration of 30 s, always requiring
release/new press after expiry. A lost input session/device closes without tail.
The new backend's bounds, lock/suspend notification and actual second-listener
audio must be measured; repetitions must not infer release or renew a deadline.
Existing process tests are synthetic evidence of current helper behavior, not
physical PTT/game-focus acceptance.

### F4 — Capture and protected storage remain unverified

**Observed inventory:** ScreenCast, Secret Service and audio services exist.
**Not run:** Electron picker, display/audio capture, vault encryption, lock/
unlock, keyring denied/locked/insecure fallback, package installation. No screen
or microphone was captured by the inventory. A portal's presence cannot prove
working application audio or absence of remote-participant feedback.

**Action:** approve a pinned Electron-only spike and run the consent/failure
matrix before claiming capture or credential support. Do not download a new
dependency or define a new media/input protocol as an unreviewed experiment.

## Pending capability matrix

`I` = inventory only, `U` = unavailable in this session, `P` = pending real trial.
There are no supported rows yet. GNOME/KDE/X11 machine/version details must be
filled on those machines, not inferred from this Pop!_OS session.

| Capability | Pop!_OS / COSMIC Wayland above | GNOME Wayland | KDE Wayland | Native X11 |
| --- | --- | --- | --- | --- |
| Packaged launch / protected keyring | I / P | P | P | P |
| Keyboard PTT, other app/game focused | U portal; P selected-device | P | P | P |
| Mouse 4 PTT, other app/game focused | U portal; P selected-device | P | P | P |
| Conflicting/grabbed input / silent lost release | P | P | P | P |
| Locked / suspended session | P | P | P | P |
| Microphone/output switch, USB/Bluetooth changes | I / P | P | P | P |
| Screen / window picker and capture | I / P | P | P | P |
| Whole-output/system audio | P | P | P | P |
| Selected-application audio / remote-speech exclusion | P | P | P | P |
| Tray / notifications / missing tray | P | P | P | P |
| x86_64 deb install / upgrade / uninstall | P | P | P | P |

## Hardware / packaged-spike procedure

Record date, commit, package/helper build, Electron/Chromium, compositor/session,
portal backend/interface versions, PipeWire, GPU/driver and device classes per
trial. Keep tokens, desktop contents, conversations and input device identifiers
out of public receipts. Use a private test server and a distinct second listener.

1. Install the pinned packaged probe as an ordinary user. Verify sandbox,
   local UI origin, current keyring backend and session-only fallback for
   denied/locked/missing/insecure storage. Try a malicious server/link/frame
   and profile switch with delayed API/WS events. Browser build still works.
2. Configure only the intended keyboard and mouse actions. For each backend,
   log monotonic press/release/cut reason and compare final sound at the second
   listener. Hold in editor, another ordinary app, full-screen app and game.
   Test separate keyboard and Mouse 4 bindings. Do not silently consume a game
   action; record shortcut conflict and user choice.
3. Test repeat storms, conflicting grabs, denied binding, key/mouse unplug,
   portal restart/session loss and a release intentionally dropped by the
   controlled test harness. Measure release tail separately from forced cut
   and silent-loss expiry. Record actual worst observed delay and policy ceiling;
   a test harness timestamp is not the listener's last audible frame.
4. Lock/suspend while held/sharing, wake still held, then release. Expect no
   auto-rejoin/share/transmit and require fresh user intent. Kill renderer,
   helper and owner separately; test EOF with owner alive and parent death with
   the pipe held elsewhere. Confirm owned process cleanup and independent clients.
5. With one native mic and a second listener, share screen then window. Cancel/
   deny picker, close source, revoke share, rotate room, disconnect/reconnect and
   switch server. Confirm exactly one participant connection/publication/playback
   path and no stale source/frame from a previous profile.
6. Test video-only first, then whole-output and selected-application audio as
   distinct scenarios. Play remote speech while local sources are silent; any
   returned remote speech fails feedback exclusion. Stop audio on consent/source
   loss and verify deafen/share-audio mute behavior. Record unsupported separately
   from permission denial and trials not run.
7. During simultaneous voice/share/watch, record capture and delivered size/fps,
   drops, capture-to-listener latency, CPU/RSS and GPU behavior. Begin 720p30; do
   not claim 1080p60 until delivered media is measured. Exercise slow frame
   consumers/malformed frames without blocking release handling.
8. Install/upgrade/remove the deb on a clean supported baseline; confirm portal
   app identity, runtime libraries, sandbox and profile retention/removal. Repeat
   on each declared target. A successful development launch is insufficient.

## Actionable blockers / handoff

| Blocker | Concrete next result | Scope owner |
| --- | --- | --- |
| B1: Electron not approved/installed for spike | Approve exact candidate/licence/runtime route, then packaged origin/picker/keyring receipts with actual Chromium version. | Nick dependency decision; #769 |
| B2: no native media bridge | Protocol-owner/security review of proposed dedicated byte channels and frame lifecycle, then native+screen+listener experiment. No implementation child Ready beforehand. | #769 with #775; future #771/#772 |
| B3: no GlobalShortcuts here, no event-node access | Available portal desktop plus selected-device ordinary-user trial; keyboard and Mouse 4 hold/release, lock, grabs and bounds. Decide approved backend and 30 s hold policy. | #769; reuse #185/#189 evidence when actually applicable |
| B4: shared auth/IPC/version contract unreviewed | Security review of sanitized route mappings and helper token handoff; concrete tested server-version allowlist; Windows transport/lifecycle feedback. | #769/#775, common shell owner #770 |
| B5: other desktop/package/audio combinations untested | Run matrix on actual GNOME Wayland, KDE Wayland and X11, including feedback exclusion and clean installation. | #769 and #774 release acceptance |

This report does not close #769 or change roadmap/issue readiness. New schemas,
dependencies and ADR amendments still require the established owner decisions.

## Review and repository verification

The security-reviewer agent reviewed this draft, the inventory and retained
identity receipts on 2026-10-28: **PASS for the draft planning/spike change**,
with no blocking security findings. This is not approval of dependency choices,
new wire contracts, implementation readiness or the shared Windows interfaces.

`go test ./...` passed. The first unmodified `make check` passed formatting,
invisible-character checks, vet, lint, Go tests, schema compatibility, TS schema
generation and dependency gates, then failed deployment-unit verification:

```text
livekit.service: Failed to create livekit.service/start: Unit home.mount not found.
```

Classification: **environment**. This machine's Docker systemd drop-in requires
`/home` mounts and a host-specific verification service; the deployment fixtures
do not supply them. No production or test source was changed to hide the failure.
The entire deployment suite passed against vendor systemd units with:

```sh
SYSTEMD_UNIT_PATH=/usr/lib/systemd/system:/run/systemd/generator:/run/systemd/generator.late make deploy-test
```

Rust and web gates were also run separately to cover the targets the original
`make check` did not reach. The standard Rust suite retains **11 pre-existing
ignored tests**: one real-server/two-login listener test, seven opt-in PipeWire
or by-hand tests (including the ten-minute pacing trial), and three real-server/
stored-login API tests. None was added, deleted or newly skipped here. These
are pending evidence, not desktop capability passes.

The full `make check` **passed** with the same `SYSTEMD_UNIT_PATH` adjustment
(all targets, no newly skipped tests). The separate Rust and web checks passed;
web reported 58 test files and 1952 tests passing. Inventory syntax, local
document links and retained JSON receipts were checked, and the retained probe
timestamps independently reproduce a 507.487 ms duplicate disconnect delay.

The first `go test -race ./...` failed only in `cmd/conch`: its nested `go build`
could not obtain VCS status (Git exit 128), and the shared build-once failure
cascaded through CLI process tests. No race-detector warning was emitted. An
isolated rerun, **five CLI race repetitions**, and the next complete race suite
passed. Classified as an intermittent test-build/tooling failure with cause
unconfirmed; tracked separately as [#886](https://github.com/njdaniel/conch/issues/886).
No CLI code, VCS stamping, test or skip policy was changed in #769.
