# macOS desktop capability matrix and security review (issue #755)

- **Reviewed:** the proposed design [docs/design/macos-desktop.md](../design/macos-desktop.md), against the sources it cites, on 2026-11-11.
- **Reviewer:** protocol-designer agent (Claude Fable 5.2), desk research only. **No Mac was used.** Nothing in this document was built or run on macOS. Nick has a Mac whose model, chip and macOS version are not known to this review (design §1.1 lists what is needed).
- **How to read the matrix.** Each row keeps two things apart, in two columns:
  - **Documented:** what a cited official page or a source file states. It is a statement about the API or the tool, not about a Conch build on a real machine. "—" means no page read says it; "memory, not checked" means the claim was written from memory and no source was read for it today.
  - **Needs a hardware run:** what only a Mac can show, and the experiment (ME1 to ME15, §3) that would show it. "—" means the row is a design rule that needs no measurement.
- **Status** is one of: `proposed` (the design says how; nothing to measure, or not yet); `documented` (the mechanism is stated by the cited page; no hardware question remains for the row as written); `pending-hardware`; `unsupported` (the design says it will not be offered); `decision` (waits on a person, named).
- **Versions to record with every hardware result:** the Mac's model and chip, macOS version and build (`sw_vers`), Xcode version (`xcodebuild -version`), Electron and Chromium (`process.versions`), `conch-voice` version and hello line, whether the build is Developer ID-signed, ad-hoc signed or unsigned (`codesign -dv`), the audio devices (UIDs and names), keyboard and mouse models and any vendor driver, the app in front, and the commit of this repository.

## 0. Browser or native, per capability

#755 asks that the design "document whether media uses browser or native components for each capability". Linux support is not evidence for any row.

| Capability | Component on macOS | Why |
| --- | --- | --- |
| Microphone capture, the transmit gate | **native**: `conch-voice`, Core Audio through `cpal` | model B: the helper is the one microphone publisher (design §7) |
| Voice playback and mixing | **native**: `conch-voice` | one playback owner for voice (#906) |
| Echo cancellation | **native**, later: the voice-processing I/O unit (MF12); none in the first release | Chromium's canceller is not in the path under model B |
| Global push-to-talk | **native**: `conch-voice`, a listen-only event tap | Electron has no press-and-release API |
| Binding capture for the settings screen | **browser**: the focused renderer's key and mouse events | no global permission is needed to read a key in one's own window |
| Screen and window capture, the picker | **browser**: Chromium, with the system's picker | the companion publishes screen video (design §8.1) |
| Watching a share | **browser**: the companion (B2) | as on the other desktops |
| Share audio | **none** in the first release; a native tap is the candidate (MF11) | the only Chromium route captures the call itself (design §8.2) |
| Camera | **browser**, in a later phase ([webcam-video.md](../design/webcam-video.md)) | `conch-voice` stays an audio client (ADR-015) |
| Device list and switching | **native**: the helper lists and switches; the app shows | `set_devices`, `conch.voice_devices` |
| Notifications, lock and sleep, invite links, credentials | **Electron main** | `Notification`, `powerMonitor`, `open-url`, `safeStorage` |
| Text chat, everything else | **browser**: the shared React application, unchanged | ADR-007 |

## 1. Capability matrix

### Target and toolchain

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-T1 | Supported OS: macOS 27 Golden Gate on Apple silicon | Apple's release table | 27.2.1 released 2026-11-09 and named as the latest ([Apple security releases](https://support.apple.com/en-us/100100)) | ME1 to ME15 on it first | pending-hardware |
| M-T2 | macOS 26 Tahoe | the same table | 26.7.3 released 2026-11-09 | the experiments rerun on 26 | pending-hardware (pending validation) |
| M-T3 | macOS 15 Sequoia, the floor | the same table | 15.8.3 released 2026-11-09. That Apple still supports it is an **inference** from the table; the page states no policy | the experiments rerun on 15, lower priority | pending-hardware (pending validation) |
| M-T4 | macOS 14 and older, and Intel Macs | — | 14.8.9 on 2026-08-06 and nothing since. `x86_64-apple-darwin` is Rust Tier 2 with host tools ([rustc platform support](https://doc.rust-lang.org/rustc/platform-support.html)). That macOS 27 excludes Intel Macs is from press reports; no Apple page was found | — | unsupported in the first release; **decision** returns to Nick if his Mac is Intel |
| M-T5 | Rust builds for `aarch64-apple-darwin` | Tier 1 with host tools, macOS 11.0 and later | [rustc platform support](https://doc.rust-lang.org/rustc/platform-support.html) | — | documented |
| M-T6 | `livekit` 0.9.4 and `webrtc-sys` build on macOS with the pinned libwebrtc | `webrtc-mac-arm64-release.zip` exists for tag `webrtc-89d790b` (277,148,750 bytes; the GitHub API reports `sha256:9f25fea4...2c348`); the `-ObjC` link argument is required | [release assets](https://api.github.com/repos/livekit/rust-sdks/releases/tags/webrtc-89d790b); [rust-sdks README](https://github.com/livekit/rust-sdks); [`.cargo/config.toml`](https://raw.githubusercontent.com/livekit/rust-sdks/main/.cargo/config.toml). The README states no Xcode or compiler requirement for macOS | ME1: the hash of the download, the Xcode and clang that work, the archive's deployment target, what the binary links | pending-hardware |
| M-T7 | Electron runs on the supported macOS versions | 44.10.0 (Chromium 152.0.7977.169, Node 24.22.1), pinned by #770; "macOS (Ventura and up)", Intel and Apple silicon binaries | [Electron 44.10.0 README](https://github.com/electron/electron/blob/v44.10.0/README.md); [releases.json](https://releases.electronjs.org/releases.json) | ME9, ME12 on the pinned version | documented for the claim; pending-hardware for behaviour |

### Audio

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-A1 | Microphone capture into the capture ring, 48 kHz mono f32, 10 ms frames | a `cpal` 0.18.2 input stream on the Core Audio host; the helper converts | `cpal` has a Core Audio host and input streams with data and error callbacks ([docs](https://docs.rs/cpal/0.18.2/cpal/)). 0.18.0: "physical stream format is now set directly on the hardware device" ([CHANGELOG](https://raw.githubusercontent.com/RustAudio/cpal/v0.18.2/CHANGELOG.md)) | ME3: the formats offered, whether asking changes the device's rate for other apps, glitches over ten minutes, whether a resampler is needed | pending-hardware |
| M-A2 | Playback of the mix from the playback ring | a `cpal` output stream | same | ME3 | pending-hardware |
| M-A3 | Device list with stable identities | `DeviceId` built from `kAudioDevicePropertyDeviceUID` | `DeviceId`: "a stable identifier for an audio device across all supported platforms" ([docs](https://docs.rs/cpal/0.18.2/cpal/)); the UID property in [`macos/device.rs`](https://raw.githubusercontent.com/RustAudio/cpal/v0.18.2/src/host/coreaudio/macos/device.rs). Apple's page for the property has no text; persistence across restarts is memory, not checked | ME4: real UIDs against the `set_devices` name rule; stability across replug and restart; two identical headsets | pending-hardware |
| M-A4 | A device removed while in use ends transmission, shows `capture: unavailable`, and is not replaced | `DeviceNotAvailable` on the error callback | 0.18.0 changelog: default output streams "now reroute or report `ErrorKind::DeviceNotAvailable`"; "Disconnection detection on Mac OS" in an earlier release | ME4: a named input device unplugged mid-press; what the callback reports and how fast the gate shuts | pending-hardware |
| M-A5 | Following `default` when it changes | `DeviceChanged` on the error callback; a transmission under way ends first | 0.18.0: `DeviceChanged` "when the system default **output** device changes". The default **input** changing is not mentioned | ME4: change the default input in System Settings and by connecting Bluetooth earphones, with the key held | pending-hardware |
| M-A6 | The lid closed with an external display: built-in microphone and speakers go | a device loss, not a lifecycle event | memory, not checked | ME4, ME12 | pending-hardware |
| M-A7 | Bluetooth earphones: lower rates in two-way mode; playback quality drops when their microphone opens | conversion in the helper; a sentence in the device picker | memory, not checked | ME3, ME4 | pending-hardware |
| M-A8 | Microphone permission denied or revoked is reported truthfully | main reads the status before starting the helper; the helper reports `no_microphone` or `audio_unavailable` | [`systemPreferences.getMediaAccessStatus`](https://github.com/electron/electron/blob/v44.10.0/docs/api/system-preferences.md) | ME5: what the stream does when denied (an error or silence), and when revoked mid-call | pending-hardware |
| M-A9 | Echo cancellation | the voice-processing I/O unit through `coreaudio-rs` (`IOType::VoiceProcessingIO`) | the unit: [Apple](https://developer.apple.com/documentation/audiotoolbox/kaudiounitsubtype_voiceprocessingio); the variant: [`types.rs`](https://raw.githubusercontent.com/RustAudio/coreaudio-rs/master/src/audio_unit/types.rs). Whether it can be reached without the crate's unsafe functions was not checked | ME15: measured echo with built-in speakers; side effects | proposed, not promised (MF12). Until then: headphones, as on Linux and Windows |

### Transport, process ownership and owner death

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-X1 | Commands reach the helper on a private channel | standard input, a libuv socketpair, as on Linux | [linux-desktop.md §5.1](../design/linux-desktop.md) (libuv's Unix process code is shared; not re-read today) | ME2 | documented by the Linux design's source; pending-hardware |
| M-X2 | Events on a channel nothing else writes to | an owner-created Unix socket; the helper connects by absolute path with `UnixStream::connect`, writes the attach line, then reads only to see the end | `voice/conch-voice/src/channel.rs` (#937, merged); `std::os::unix` exists on macOS | ME2: attach, a minute of events, the end seen | pending-hardware |
| M-X3 | The socket's directory keeps other users out | the per-user temporary directory, **verified** (a directory, not a link, owned by main, no group or other access), then `conch/<profile-id>` at 0o700 | that macOS has such a directory and that `$TMPDIR` names it: memory, not checked | ME2: owner, mode and length of the real directory; from a second account, the socket cannot be reached | pending-hardware |
| M-X4 | The path fits | `sun_path` is 104 bytes on macOS, so the budget is 103; the owner fails closed with `socket-path-too-long` | [xnu `bsd/sys/un.h`](https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/sys/un.h): `char sun_path[104]` | ME2: the real length with a profile id and a random name | documented for the limit; pending-hardware for the fit |
| M-X5 | The helper knows its own uid without `/proc` | `rustix::process::getuid()` as a direct dependency, or a probe file | `devices::current_uid` reads `/proc/self` today (repository). `rustix` 1.1.5 is in `Cargo.lock`. That `getuid` is a safe function there: memory, not checked | ME2, with whichever route MF2 takes | **decision** (the dependency); pending-hardware |
| M-X6 | Owner death stops capture and ends the helper within the contract's bounds | EOF on standard input; EOF or any byte on the events socket; the parent watch (`parent_id`) | `managed.rs`, `channel.rs` (repository). That an orphan is given to `launchd`, and that Force Quit leaves children running: memory, not checked. kqueue's `NOTE_EXIT` exists ([kqueue(2)](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/kqueue.2.html)) and is not used | ME6: kill and Force Quit main while transmitting; measure gate shut, capture stop and exit against `VoiceControlGateShutMS`, `VoiceControlCaptureStopMS`, `VoiceControlOwnerLossExitMS` | pending-hardware |
| M-X7 | Bounded stop; only the owned process is ever signalled | `leave`, close both channels, `SIGTERM`, `SIGKILL`, wait; no `detached`; never by name | [linux-desktop.md §5.3](../design/linux-desktop.md) | ME6: a hung helper | pending-hardware |
| M-X8 | No listener, no secret in argv, an environment built from empty | a Unix socket, not TCP; the token and nonce in the environment; the macOS allowlist (`HOME`, `TMPDIR`, `LANG`, `LC_*`) | design §3.2; `desktop/main/voice/spawn.ts` | ME2: the helper runs with exactly that list | pending-hardware |
| M-X9 | The helper is not slowed while the window is hidden | none needed, or `powerSaveBlocker` for the length of a session | App Nap's reach: memory, not checked | ME6: a ten-minute call with the window hidden | pending-hardware |
| M-X10 | fd 3 (the Go TUI's route) on macOS | not offered: no `/proc/self/fd`; a start without `--events-pipe` gets the existing error | `managed::open_event_pipe` (repository) | — | unsupported (TUI voice on macOS is outside #755) |
| M-X11 | No press or release on the control channel; v1 and v2 byte-identical | input stays in the helper | [pkg/schema/voice_control.go](../../pkg/schema/voice_control.go) | — | documented (repository) |

### Push-to-talk (each row is press AND release, heard by a second listener)

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-P1 | Which permission a listen-only tap needs | Input Monitoring, and not Accessibility | Input Monitoring exists and covers apps that "monitor your keyboard, mouse, or trackpad even when you're using other apps" ([Mac User Guide](https://support.apple.com/guide/mac-help/control-access-to-input-monitoring-on-mac-mchl4cedafb6/mac)); the listen-access functions exist since 10.15 with no text. **No page read says a listen-only tap needs this and only this** | ME7, first step | pending-hardware; **if Accessibility is needed, the design returns to Nick** |
| M-P2 | Keyboard, Conch's window in front | `KeyDown`/`KeyUp` by virtual key code | the event types and `KEYBOARD_EVENT_KEYCODE` in [`core-graphics`](https://raw.githubusercontent.com/servo/core-foundation-rs/main/core-graphics/src/event.rs) | ME7 | pending-hardware |
| M-P3 | Keyboard, another app in front | a session-level tap is not tied to focus | [`tapCreate`](https://developer.apple.com/documentation/coregraphics/cgevent/tapcreate(tap:place:options:eventsofinterest:callback:userinfo:)) describes taps; it does not discuss focus | ME7 | pending-hardware |
| M-P4 | Keyboard, a full-screen app or game in front | same | — | ME7, per app, title recorded | pending-hardware |
| M-P5 | Mouse side buttons, in front and behind | `OtherMouseDown`/`OtherMouseUp` with `MOUSE_EVENT_BUTTON_NUMBER` | "Additional buttons are specified in USB order using the integers 3 to 31" ([CGMouseButton](https://developer.apple.com/documentation/coregraphics/cgmousebutton)) | ME7: the numbers real mice send, with and without a vendor driver | pending-hardware |
| M-P6 | Auto-repeat never holds or reopens the gate | the auto-repeat field, and the machine ignoring a down for a held key | `KEYBOARD_EVENT_AUTOREPEAT` in the crate | ME7: hold ten seconds, one transmission | pending-hardware |
| M-P7 | A key held when the helper starts cannot open the gate | a binding is armed only by an observed release | design §4.4; no key-state query is reachable from safe code (`event_source.rs` wraps only the constructor) | ME7 | pending-hardware |
| M-P8 | Holding past `max_transmit_secs` ends the transmission; repeats do not reopen it | the shared 30 s desktop default | [linux-desktop.md §7.1](../design/linux-desktop.md) | ME7 | pending-hardware |
| M-P9 | A disabled tap shuts the gate and is re-enabled | `TapDisabledByTimeout`/`TapDisabledByUserInput` arrive as events; `enable()` | [Apple](https://developer.apple.com/documentation/coregraphics/cgeventtype/tapdisabledbytimeout); the crate's `enable` | ME7 if it can be provoked; otherwise a unit test with a fake tap | proposed; pending-hardware |
| M-P10 | A refused tap is `ptt_problem: refused`; voice still joins | the tap's creation fails | `tapCreate` returns nothing when the tap cannot be created; the vocabulary has `refused` | ME5 | pending-hardware |
| M-P11 | Secure Input: presses not seen, a release possibly missed | the tap goes deaf to the keyboard; the 30 s bound; the visible `transmitting` | the system will "stop passing keyboard events to any intercept process whenever any process has enabled secure event input" ([TN2150](https://developer.apple.com/library/archive/technotes/tn2150/_index.html), 2007) | ME8: a password field, Terminal's secure keyboard entry, with the key held and not held; a mouse binding under the same | pending-hardware; presses under Secure Input are **unsupported**, labelled |
| M-P12 | A keyboard unplugged with the key held | **no removal notice exists for a tap**; a key up may or may not arrive | — | ME7 | pending-hardware; if no key up: bounded by `max_transmit_secs` only, and said so |
| M-P13 | The bound key still reaches the app in front | the tap is listen-only: "a passive listener" | [Apple](https://developer.apple.com/documentation/coregraphics/cgeventtapoptions/listenonly) | ME7 | documented; pending-hardware |
| M-P14 | A modifier alone as the talk key | `FlagsChanged` | the event type exists; it carries state, not a transition for one key | ME7: left and right keys told apart, press and release | pending-hardware; refused by `config set` unless shown |

### Screen share and audio

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-S1 | Choosing a screen or window | the system's picker: `setDisplayMediaRequestHandler(handler, { useSystemPicker: true })` | "experimental, and currently available for MacOS 15+ only" ([Electron 44.10.0 `session`](https://github.com/electron/electron/blob/v44.10.0/docs/api/session.md)); `SCContentSharingPicker`, macOS 14.0 and later ([Apple](https://developer.apple.com/documentation/screencapturekit/sccontentsharingpicker)) | ME9: it works from the app's own scheme in 44.10.0 on 15, 26 and 27; cancel; a second share | pending-hardware; if it does not work, a **decision** (a chooser built by main) |
| M-S2 | Screen Recording permission | read with `getMediaAccessStatus('screen')`; no ask call exists for it | [`systemPreferences`](https://github.com/electron/electron/blob/v44.10.0/docs/api/system-preferences.md): `askForMediaAccess` takes `microphone`, `camera`. Whether the picker needs the permission, and periodic re-asking: memory, not checked | ME9 | pending-hardware |
| M-S3 | Source closed, minimised, a full-screen Space, a display unplugged, protected content | capture-side facts | — | ME9; "unsupported" is an allowed answer | pending-hardware |
| M-S4 | Share quality | 720p30 by default, 1080p30; nothing unmeasured | [linux-desktop.md §8](../design/linux-desktop.md) (Q-D4-3) | ME9: resolution, frame rate, CPU on named hardware | pending-hardware |
| M-S5 | Sound with a share | **none**: the share is silent and says so | design §8.2; as Linux (#1552) | — | proposed |
| M-S6 | Whole output through Chromium | `audio: 'loopback'` | **Electron 44.10.0 contradicts itself:** `session.md`: "currently only supported on Windows"; `desktop-capturer.md`: Core Audio taps are "the default for desktop audio capture" since v39 on macOS 14.2 and later, needing `NSAudioCaptureUsageDescription` | ME10: what it does, and that it carries the helper's playback | **unsupported as "system audio"**: it would rebroadcast the call |
| M-S7 | Everything but the call; this application | a Core Audio process tap excluding, or naming, processes; or a ScreenCaptureKit filter | `CATapDescription` and its initialisers exist ([Apple](https://developer.apple.com/documentation/coreaudio/catapdescription); no text on the initialisers); `excludesCurrentProcessAudio` excludes "audio from your app" ([Apple](https://developer.apple.com/documentation/screencapturekit/scstreamconfiguration/excludescurrentprocessaudio)), and the helper is another process | ME10: a tap that excludes the helper captures none of the call; whether the system counts the helper as the app | pending-hardware; not offered until demonstrated (MF11) |

### Permissions

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-R1 | Microphone asked once, at the first Join, with the reason first | `getMediaAccessStatus`, then `askForMediaAccess('microphone')`; `NSMicrophoneUsageDescription` | [`systemPreferences`](https://github.com/electron/electron/blob/v44.10.0/docs/api/system-preferences.md); without the key and entitlement "the system terminates your app" ([Apple](https://developer.apple.com/documentation/avfoundation/requesting-authorization-to-capture-and-save-media)) | ME5: the dialog's app name; that the helper is covered by the app's grant | pending-hardware |
| M-R2 | Microphone denied | Join fails before a helper starts; a sentence and a Settings button; no retry | "if access has already been requested and denied, it must be changed through the preference pane; an alert will not pop up" (same Electron page) | ME5 | pending-hardware |
| M-R3 | Microphone granted later, or revoked mid-call | the app says if it must be reopened | "a restart of the app will be required for the new permissions to take effect" (same page) | ME5: whether a fresh helper sees the grant without that | pending-hardware |
| M-R4 | Input Monitoring asked when a global key is saved | the tap's creation | memory, not checked (that creating the tap raises the prompt) | ME5, ME7 | pending-hardware |
| M-R5 | Input Monitoring denied or reset | `ptt_problem: refused`; open mode still works | the vocabulary (repository) | ME5 | pending-hardware |
| M-R6 | Accessibility | never requested | — | ME7 confirms it is not needed | proposed |
| M-R7 | Notifications | Electron `Notification`; in-window marks if denied | "requires an application to be code-signed in order for notifications to appear. Unsigned binaries will emit a `failed` event" ([Electron 44.10.0](https://github.com/electron/electron/blob/v44.10.0/docs/api/notification.md)) | ME5: signed and unsigned | pending-hardware |
| M-R8 | Local network, for a server on the same network | a usage string; the connection error names the setting | memory, not checked | ME5: a server at a private address, allowed and denied | pending-hardware |
| M-R9 | Camera; system audio | not requested in the first release | design §9 | — | proposed |
| M-R10 | A reset, or a differently signed build | `tccutil reset`; an unsigned build is a different app to the system | memory, not checked | ME5 | pending-hardware |

### Lock, sleep and lifecycle

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-L1 | Lock closes transmission; unlock does not reopen it | `set_mute true` on `lock-screen` | `lock-screen`/`unlock-screen`: "_macOS_ _Windows_" ([Electron 44.10.0 `powerMonitor`](https://github.com/electron/electron/blob/v44.10.0/docs/api/power-monitor.md)) | ME12, ME8 | documented; pending-hardware |
| M-L2 | Sleep ends the session; wake does not rejoin | `leave` on `suspend`; nothing on `resume` | `suspend`/`resume` on the same page | ME12: Apple menu Sleep, and the lid on battery | pending-hardware |
| M-L3 | Switching users is treated as a lock | `user-did-resign-active` | macOS-only events on the same page | ME12 | pending-hardware |
| M-L4 | The lid with an external display | a device loss (M-A6) | memory, not checked | ME12 | pending-hardware |
| M-L5 | Reconnect after wake or a network change | the existing policy; the gate stays shut until a fresh press | [linux-desktop.md §9](../design/linux-desktop.md) | ME12 | pending-hardware |
| M-L6 | Close quits and stops media; the Dock and menu-bar item are the opt-in; Quit always stops the helper | #773's rule kept on macOS | with no `window-all-closed` handler "the default behavior is to quit the app" ([`app`](https://github.com/electron/electron/blob/v44.10.0/docs/api/app.md)); that Mac custom is to stay in the Dock is memory, not checked | ME12 | **decision** (it departs from Mac custom) |
| M-L7 | Invite links cannot join voice or retarget audio | `CFBundleURLTypes` at build; `open-url` registered before `ready`; the shared strict parse and choice | "register a listener for the `open-url` event early ... If you register the listener in response to a `ready` event, you'll miss URLs that trigger the launch" (same page) | ME12: `open 'conch://invite?...'` cold and warm | documented; pending-hardware |
| M-L8 | Single instance | the system for Finder and URL launches; `requestSingleInstanceLock()` for command-line launches | same page | ME12 | documented |

### Privilege

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-G1 | Runs as an ordinary user: no root, no kernel or system extension, no protection disabled | user-space APIs only; the hardened runtime on | design §5 | ME14: a standard (non-administrator) account installs and runs it | pending-hardware |

### Media ownership

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-M1 | One microphone publisher, one playback owner per source | model B, #906 | [windows-desktop.md §7](../design/windows-desktop.md); decided 2026-10-28 | ME11 | pending-hardware |
| M-M2 | Helper and companion stay connected together | the companion grant under a derived identity (#897) | [linux-desktop.md §6.4](../design/linux-desktop.md) | ME11 | pending-hardware |
| M-M3 | Watching a share from the app | the companion (B2) | same | ME11 | pending-hardware |

### Credentials

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-C1 | Tokens protected at rest, per profile | `safeStorage`, asynchronous API: the Keychain | "protecting content from other users and other apps running in the same userspace"; "in a way that prevents other applications from loading them without user override" ([Electron 44.10.0](https://github.com/electron/electron/blob/v44.10.0/docs/api/safe-storage.md)) | ME13 | documented; pending-hardware |
| M-C2 | Session-only fallback, including a denied Keychain prompt | unavailable, or denied, is no persistence, with the reason | "these calls can block the current thread to collect user input" (same page). How a denial is reported is not stated | ME13 | pending-hardware |
| M-C3 | A rebuilt or re-signed app and the Keychain | a consistent signature | "macOS may not recognize different builds of your app as the same application, which can cause the Keychain to re-prompt the user for permission on every update" (same page) | ME13, ME14 | documented; pending-hardware |
| M-C4 | Token to the helper without argv, logs or the renderer | `credential: env` | [windows-desktop.md §3.7](../design/windows-desktop.md) | ME2 | proposed |

### Distribution

| Id | Capability | Mechanism | Documented | Needs a hardware run | Status |
| --- | --- | --- | --- | --- | --- |
| M-D1 | One `arm64` DMG and zip, with `conch-voice` inside the app | electron-builder 26.17.0, `mac` target | default target "`dmg` and `zip` for Squirrel.Mac"; `binaries`; `minimumSystemVersion` ([`macOptions.ts`](https://raw.githubusercontent.com/electron-userland/electron-builder/electron-builder@26.17.0/packages/app-builder-lib/src/options/macOptions.ts)) | ME14 | pending-hardware |
| M-D2 | Signed with a Developer ID | the Apple Developer Program | 99 USD per membership year; members "receive an Apple-verified Developer ID" ([Apple](https://developer.apple.com/programs/whats-included/)) | — | **decision** (Nick; design §12.2) |
| M-D3 | Notarized and stapled | `notarytool` through electron-builder, with one of three credential sets | "all software built after June 1, 2019, and distributed with Developer ID must be notarized"; the service "generates a ticket for you to staple" ([Apple](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution)) | ME14: `stapler validate`, `spctl -a -vvv`, first launch offline | pending-hardware |
| M-D4 | The hardened runtime with two entitlements in all | `allow-jit` on the app; `audio-input` on the helper | "to upload a macOS app to be notarized, you must enable the Hardened Runtime"; "use only the entitlements that are absolutely necessary" ([Apple](https://developer.apple.com/documentation/security/hardened-runtime)) | ME14: the app runs and the helper records with exactly these; ME5 for whether the app needs `audio-input` too | pending-hardware |
| M-D5 | Manual verified updates; no restart during a call | the release notice; the notarized DMG; `checksums.txt` | [linux-desktop.md §9](../design/linux-desktop.md) (Q-D6-8) | ME14: replace the app; grants and the Keychain item survive | pending-hardware |
| M-D6 | An unsigned preview is labelled, and comes with no instruction to lower a protection | design §12.2 | how Gatekeeper treats it: memory, not checked | ME5, ME14 | proposed |
| M-D7 | Clean install, replace, remove; what is left behind | drag in, drag over, drag to the Trash | — | ME14 | pending-hardware |

## 2. Security review

This is a design review; no macOS code exists. Each item is a threat, who can mount it, the design's answer, what remains, and the status.

### 2.1 Managed transport

| Threat | Who | Design's answer | Residual | Status |
| --- | --- | --- | --- | --- |
| **The invariant.** Authority arrives only on standard input, from the spawning process | — | unchanged from Linux and Windows; the events socket carries none | — | by design (shared) |
| Another **user** reaches the events socket | any local user | the directory: verified as owned by main with no group or other access, then a 0o700 subdirectory; the helper checks the same before connecting | if the per-user temporary directory is not what memory says it is, the owner **refuses** (it never falls back to `/tmp`); voice does not start | pending ME2 |
| **Same-user** code unlinks the socket and binds its own | same-user processes | none, and none claimed: named in `channel.rs` and in both earlier designs; the nonce is a binding, not authentication | confidentiality of state lines and availability; never authority | out of scope by design |
| The path is **too long** and something truncates it | — | the budget is computed (103 bytes) and the owner fails closed | none | by design |
| The **temporary directory is cleaned** under a running session | the system | the socket is already connected; a new session re-verifies and recreates | none expected | pending ME2 (a long session) |
| The helper trusts a **wrong uid** | — | one implementation per platform; route 1 asks the system, route 2 reads a file it just created | route 2 needs write access where the helper otherwise reads | decision (MF2) |
| **Owner death** leaves a helper holding the microphone | a crash, Force Quit | three independent signs of owner loss; the helper's own watchdog | bounded by `VoiceControlOwnerLossExitMS` | pending ME6 |
| **Injected libraries** through the environment | a stray variable in main's environment | the child's environment is built from empty; no `DYLD_*` is passed; the hardened runtime is on and the `allow-dyld-environment-variables` entitlement is not granted | none | by design |
| **The wrong app is named** in a permission prompt, or the grant does not reach the helper | — | measured before anything is built on it | if the helper is its own identity to the system, the usage strings and the user-facing text change, and design §6's hypothesis is tested | pending ME5 |

### 2.2 Input

| Risk | Design's answer | Residual | Status |
| --- | --- | --- | --- |
| **Keylogging surface.** A listen-only tap is handed every key and mouse-button event in the session | only bound transitions leave the input module; no key code, count or time is kept, logged or sent; the no-key-code output test runs on macOS | any app the person grants Input Monitoring can do the same; Conch adds the grant, and says what it means before asking | by design; the README states it |
| **Asking for too much.** Accessibility would let the app control the computer | never requested; the tap is listen-only; if it turns out to be required, the design stops | — | pending ME7 |
| **An active tap** could delay or drop the person's input | not used: the options are exactly `ListenOnly`, audited at the pinned version | none | by design (MF5's audit) |
| **Secure Input** hides a release | the 30 s bound; `transmitting` visible; the owner's mute on lock; a mouse binding as the recommendation if ME8 confirms it is unaffected | up to 30 s of transmission after a key is let go while a password field holds the keyboard | pending ME8; stated to the person |
| **No device-removal notice** | the same bound | up to 30 s after a keyboard is unplugged with the key held, if no key up arrives | pending ME7; the one place macOS is weaker than Linux and Windows |
| **A key held at start, a stalled thread, a disabled tap** | armed only by a release; the watchdog tick; disabled is lost | one unused press | pending ME7 |
| **Injected input** opens the gate | a same-user process that can post events can also read the token | not a new boundary | accepted, stated |
| **The dependency's own surface** | `core-graphics` pinned, licence-checked, features minimal; `new_unchecked` (unsafe) not used; no call that posts an event | a new version re-runs the audit | pending MF5 |

### 2.3 Other notes

- **Permissions are spent once.** macOS does not ask twice, so an ask made at the wrong moment (at launch, with no explanation) is a permanent denial for most people. The design's rule that each is asked at the moment of use, after a sentence, is a security property as much as a courtesy: a person who understands the ask is the only real check on it.
- **Signing is identity.** The privacy grants, the Keychain item, notifications and the first-launch check all key on the signature. A preview signed differently from the release teaches nothing about the release and leaves grants behind under another identity. This is why §12.2 of the design is a decision for the owner and not a packaging detail.
- **Entitlements.** Two in all, each on the one executable that needs it. The three that weaken the hardened runtime most (unsigned executable memory, disabled library validation, dyld environment variables) are absent, and ME14 runs the app without them rather than assuming it can.
- **The share's sound.** The only route Chromium offers on macOS captures the whole output, the call included. Shipping silent is the safe side; the Electron documentation's disagreement with itself (M-S6) is one more reason not to build on it unmeasured.
- **Local network.** A denied local-network permission is indistinguishable from a server that is down unless the app says otherwise. It is a usability trap with a security flavour: people will be told to "fix the server" when the answer is a setting.
- **Companion participant, installer and updates:** as the Windows review, §2.3.

## 3. Experiments (MF17): steps and pass criteria

**Every experiment here needs real hardware**: a Mac, and a person at it. None has been run. Run on macOS 27 first; repeat on 26 and 15 where a machine exists. Record the versions listed in the preamble with each. "A second listener" is another person in the room, or a second device, joined to the same voice room.

### ME1. Build (the Mac, Xcode)

1. Record `sw_vers`, `uname -m`, `xcodebuild -version`, `clang --version`.
2. Download `webrtc-mac-arm64-release.zip` for tag `webrtc-89d790b`; record `shasum -a 256` and compare with the digest the GitHub API reports.
3. Build `conch-voice` for `aarch64-apple-darwin` with the archive, the `-ObjC` link argument, and a stub audio backend (#1309's split in place); record whether the Command Line Tools alone are enough, the build time, the binary's size, `otool -L` and `vtool -show-build` (the deployment target).
4. `cargo deny check` with `aarch64-apple-darwin` among the targets; record every crate and licence that appears only on macOS.
5. Add `cpal` and `core-graphics` in a scratch branch; record what `Cargo.lock` gains.

**Pass:** the helper builds and starts (`conch-voice --version`) on the Mac; the hash is recorded; the deployment target is 15.0 or lower; `cargo deny` is clean or its findings are listed. **Settles:** M-T5, M-T6.

### ME2. The events socket from an Electron owner (the Mac, a second user account)

1. In the app's main process, record `app.getPath('temp')`, `$TMPDIR`, and `stat` of that directory: owner, mode, length in bytes.
2. Create `conch/<profile-id>` there at 0o700, bind a random socket name, record the full path's length against 103.
3. Start the helper with `--events-pipe`, the nonce in its environment, and exactly the allowlisted environment of design §3.2. Record the attach line's arrival, then one event line every 100 ms for a minute.
4. From a **second standard user account**, try to list the directory and to connect to the socket.
5. Leave a session running for an hour; record whether the directory or socket was disturbed.
6. Record what `ls -l /dev/fd/` shows in a child started with a fourth stdio pipe (for the record only; fd 3 is not used).

**Pass:** the directory is owned by the user with no group or other access; the path fits; the attach arrives within the hello timeout and every event arrives; the second user gets "permission denied" at the directory; the helper runs with the allowlist and nothing else. **Settles:** M-X1 to M-X5, M-X8, M-C4.

### ME3. Capture and playback (built-in audio, a USB headset, Bluetooth earphones)

1. With `cpal`, list the default input and output configurations and the supported ranges for each device.
2. Note each device's rate in Audio MIDI Setup; open streams as the backend would; note the rates again, with another app playing audio.
3. Run capture and playback through the rings with the LiveKit runtime active for ten minutes per device; count underruns and overruns; record callback sizes.
4. Repeat with Bluetooth earphones as input and output.

**Pass:** 48 kHz mono f32 in 10 ms frames reaches the rings on every device, by conversion where needed; **no other app's device rate changes**; no audible glitch and no ring underrun in ten minutes. If conversion needs a resampler, that is the finding. **Settles:** M-A1, M-A2, M-A7, the resampler row of design §15.

### ME4. Device identity and loss (a USB headset, Bluetooth earphones, an external display)

1. Record every device's UID and name; check each UID against the `set_devices` name rule; restart the Mac and compare.
2. Plug in two identical USB headsets if available; record their UIDs.
3. With a **named** microphone selected and the talk key held, unplug it. Record the error the callback reports, the time to the gate shutting, and the state line.
4. With `default` selected and the key held, change the default input in System Settings; then connect Bluetooth earphones.
5. Close the lid with an external display attached while in a call on the built-in microphone.

**Pass:** UIDs are stable across replug and restart and distinct for identical devices; a removed named device gives `capture: unavailable` with `no_microphone` within `VoiceControlGateShutMS` of the callback and **nothing is substituted**; a default change ends the transmission under way and the next press uses the new default; the lid case is a device loss, reported as one. **Settles:** M-A3 to M-A6.

### ME5. Permissions from a clean state (the Mac; a signed build for the second half)

For each of microphone, Input Monitoring, notifications, local network and (if ME9 shows it is used) screen recording:

1. Reset it for the app (`tccutil reset <service> chat.conch.desktop`; record the service names that work).
2. Trigger the ask as the design says (first Join; saving a global key; the first notification; connecting to a server at a private address; the first Share). Record the dialog's text and **the app name it shows**, and the name listed in System Settings afterwards (Conch, or `conch-voice`).
3. Deny. Record what the app shows, what the helper reports (for the microphone: an error or a silent stream; for the tap: `ptt_problem`), and that nothing asks again.
4. Grant in System Settings while the app runs. Record whether the next Join works without reopening the app.
5. Revoke during a call. Record what happens to the stream and the tap.
6. Repeat steps 1 to 4 with an **ad-hoc signed** build and with a rebuilt one; record whether the grants carry over.
7. Check whether the app, and not only the helper, needs the audio-input entitlement for the microphone prompt to appear.

**Pass:** each ask appears once, at the moment of use, naming Conch; each denial produces the named state and sentence of design §9 and no retry; a denied microphone never produces a session that transmits silence; the entitlement set of design §12.3 is sufficient, or the one addition is recorded. **Settles:** M-A8, M-P10, M-R1 to M-R10, M-D4 (in part), design §6's hypothesis.

### ME6. Owner death and App Nap (a second listener)

1. In a call, holding the talk key and being heard: `kill -9` the main process. Measure, from the listener's side and the helper's log, when transmission stopped and when the helper exited. Record the helper's parent pid before and after.
2. Repeat with Force Quit from the Apple menu, and with a renderer crash.
3. Make the helper hang (a debugger stop) and use the app's Leave; time the `SIGTERM` and `SIGKILL` steps.
4. In a call, hide the window (and separately minimise it, and cover it) for ten minutes with the listener talking; record late or missing frames and the gaps between `VoiceControlOwnerPollMS` ticks.

**Pass:** the gate shuts within `VoiceControlGateShutMS` of the helper noticing, capture stops within `VoiceControlCaptureStopMS`, and the process is gone within `VoiceControlOwnerLossExitMS`, in every case; no helper survives its owner; no audible degradation with the window hidden, or `powerSaveBlocker` is shown to cure it. **Settles:** M-X6, M-X7, M-X9.

### ME7. The listen-only event tap (a mouse with side buttons, a second listener)

1. With no permission granted, create a listen-only tap for key and other-mouse events through `core-graphics`. Record whether creation fails, which prompt appears (**Input Monitoring or Accessibility**), and what works after granting only Input Monitoring.
2. Press and release the bound key and each side button with: Conch in front; a browser in front; an app in a full-screen Space; a game in full screen. The listener confirms each start and stop.
3. Record the button numbers each mouse sends, with and without its vendor's driver installed.
4. Hold the key for ten seconds (one transmission; repeats ignored). Hold across switching apps.
5. Start the helper with the key already held (nothing transmits until a release and a new press).
6. Hold past `max_transmit_secs` (it ends at the bound; repeats do not reopen; a release and press do).
7. Unplug the keyboard, and separately the mouse, with the binding held. Record whether an up event arrives.
8. Bind Right Option alone; record what `FlagsChanged` delivers for left and right, down and up.
9. Confirm the app in front still receives the key.
10. Run the helper's output through the no-key-code check.

**Pass:** only Input Monitoring is needed; press and release are heard correctly in every focus case for key and mouse; the arming and bound rules hold; the key still reaches the front app; no key code appears in any output. **If Accessibility is needed, stop and report.** **Settles:** M-P1 to M-P9, M-P12 to M-P14, M-R4, M-R6.

### ME8. Secure Input, lock and user switching (a second listener)

1. Hold the talk key (being heard), click into a password field in a browser, release the key. Record when the listener stops hearing and what the state shows.
2. With the cursor in a password field, press the talk key. Record that nothing transmits.
3. Turn on Terminal's Secure Keyboard Entry, leave Terminal in the background, repeat 1 and 2 with another app in front.
4. Repeat 1 and 2 with a mouse side button as the binding.
5. Hold the key and lock the screen; release on the lock screen; unlock. Hold the key and switch users.

**Pass:** nothing transmits past `max_transmit_secs` in any case, and the state shows `transmitting` for as long as it does; lock and user switching end transmission at once through the owner's mute; after unlock nothing transmits without a fresh press and a deliberate unmute. The mouse result is recorded either way. **Settles:** M-P11, M-L1, M-L3.

### ME9. The system picker in the pinned Electron (a second viewer)

1. With `useSystemPicker: true`, start a share from the app. Record whether the system's picker appears, whether the handler is called, and whether any Screen Recording prompt appears, on macOS 15, 26 and 27.
2. Pick a window, a display, an app; cancel; start a second share.
3. Close, minimise and move the shared window to a full-screen Space; unplug a shared display; share a window showing protected video.
4. Measure at 720p30 and 1080p30: delivered resolution and frame rate at the viewer, latency, CPU.
5. With `useSystemPicker: false`, record what `desktopCapturer.getSources` needs and whether the system re-asks later.

**Pass:** the picker works from the app's scheme on every supported macOS; cancel shares nothing; a closed source ends the share with its claim; the measurements are recorded. **If the picker does not work, stop and report** (design §8.1). **Settles:** M-S1 to M-S4.

### ME10. Share audio (a second listener)

1. In the pinned Electron, request a share with `audio: 'loopback'`, with and without `NSAudioCaptureUsageDescription`. Record whether an audio track exists and whether it carries sound.
2. With the helper playing a remote participant's speech and **no local sound**, record whether the loopback track carries that speech.
3. Natively (a scratch program is enough): create a process tap that excludes the helper's process; with remote speech playing and music playing in another app, record what the tap carries.
4. Create a tap of one app's processes; and a ScreenCaptureKit stream on a filter for one app with audio on.
5. Pick Conch's own window with application audio requested.

**Pass for any route to be offered:** with remote speech alone playing, the captured track is **silent** to a measurement listener. Step 2 is expected to fail that test, which is why the route is not offered; record it. Step 5 must be refused by pid (#905). **Settles:** M-S6, M-S7.

### ME11. Model B on macOS (a second participant)

The Windows E10 list ([windows-desktop-capabilities.md §3](windows-desktop-capabilities.md)), once, with a macOS helper and the Electron companion: both connections stay up; the helper is not displaced; voice plays once; a remote share's sound plays once; the helper never plays its own companion.

**Pass:** as E10. **Settles:** M-M1 to M-M3.

### ME12. Lifecycle (the Mac)

1. In a call: lock (Control-Command-Q); Apple menu Sleep; close the lid on battery; close the lid with an external display and power; wake each time. Record the `powerMonitor` events seen and what the app did.
2. Switch users and back.
3. Close the window; Command-Q; Quit from the Dock menu; log out with a call running.
4. Turn Wi-Fi off and on in a call.
5. With the app closed, run `open 'conch://invite?server=...&code=...'`; then again with the app running; then with a malformed link.
6. Two profiles: sign in to two servers, join voice on one, switch.

**Pass:** nothing transmits while locked or after a wake without a fresh Join and press; no helper survives a quit; a link shows the choice and never joins or contacts a server by itself; profile isolation as on Linux. **Settles:** M-L1 to M-L8, M-A6.

### ME13. The Keychain (signed and unsigned builds)

1. Launch a fresh install and record any Keychain prompt before a login exists (the cookie-encryption fuse). Save a login. Record the Keychain item's name and access list, and any prompt.
2. Deny the prompt. Record what `safeStorage` reports and what the app offers.
3. Rebuild the app (ad-hoc signed) and launch; then a Developer ID-signed build and its successor. Record prompts.
4. Try to read the item from another app (Keychain Access as the same user; a script).

**Pass:** a denied or unavailable Keychain gives session-only login with its reason and stores nothing readable; two builds with the same Developer ID do not re-ask. **Settles:** M-C1 to M-C3.

### ME14. A signed, notarized, stapled build on a clean account (a Developer ID; a second account or Mac)

1. Build the DMG with the entitlements of design §12.3 and no others; sign; notarize; staple.
2. Verify: `codesign --verify --deep --strict --verbose=2`, `codesign -d --entitlements -` for the app and for `conch-voice`, `spctl -a -vvv`, `xcrun stapler validate`.
3. Download it through a browser on a clean **standard** account, with the network off at first launch. Record the dialog.
4. Run the whole product: chat, voice, talk key, share.
5. Replace the app with a newer build of the same identity. Record whether the privacy grants and the Keychain item survive.
6. Move the app to the Trash. Record what is left: the `userData` directory, the Keychain item, the privacy entries, logs, caches.
7. For comparison, the same download unsigned: record what the system says, **and do not record any way round it as an instruction.**

**Pass:** every verification command succeeds; first launch works offline with the ordinary "downloaded from the Internet" dialog; the app runs fully with two entitlements; an update keeps grants and the saved login; what uninstall leaves is listed. **Settles:** M-D1, M-D3 to M-D7, M-G1.

### ME15. Voice processing (a second listener)

1. Behind the same traits, open the voice-processing I/O unit through `coreaudio-rs`; record whether any unsafe function is needed.
2. On built-in speakers and microphone, with the remote participant talking, record what they hear of themselves, against the plain path of ME3.
3. Record what happens to other apps' volume and to the microphone's character while it is active, and with headphones.

**Pass (to offer it):** the remote participant hears no echo of themselves at ordinary speaker volume, speech stays intelligible in both directions, and the side effects are acceptable and described. **Settles:** M-A9.

## 4. Verdict

The design is **ready for review as a proposal**, not for acceptance. It adds no wire shape and changes none; it shares every platform-neutral decision with the Windows and Linux designs and says where macOS differs; Linux and Windows behaviour and `forbid(unsafe_code)` are unchanged. Four things stand between it and implementation rows becoming Ready, and none can be settled from documentation:

1. **Which Mac.** The target (Apple silicon, macOS 27 first) is a proposal until Nick's Mac is known (design §1.1). An Intel Mac changes it.
2. **Dependencies:** `cpal`, `core-graphics`, `core-foundation` as a direct dependency, the uid route (`rustix` or none), and probably a resampler. The owner's sign-off.
3. **The account:** the Apple Developer Program membership and where the signing identity lives (design §12.2). Without it there is no build that behaves like the product: notifications, the Keychain and the privacy grants all depend on the signature.
4. **Hardware evidence**, ME1 to ME15. Two of them can stop the design rather than fill a row: **ME7** (if a listen-only tap needs Accessibility) and **ME9** (if the system picker does not work in the pinned Electron).

The findings a reader should not miss:

- **macOS is weaker than Linux and Windows in one place:** an event tap has no device-removal notice and goes deaf to the keyboard under Secure Input, so two cases of a missed release are bounded only by the 30 s default (M-P11, M-P12). They fail closed for the microphone and are stated, not hidden.
- **`winit`, the Windows input route, does not transfer:** on macOS it reports device events only from events delivered to the app (design §4.2).
- **Electron's documentation disagrees with itself** about system audio on macOS at the pinned version (M-S6). The design builds nothing on either reading.
- **The helper is a separate process**, so the per-process "exclude my own audio" switches do not cover the call (M-S7), and which app a permission prompt names is a measurement (ME5).
- **`conch.voice_devices.v2` is not yet published:** adding `coreaudio` now costs a line; later it costs a version (design §2.6).
