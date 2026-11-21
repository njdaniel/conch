# Android voice receipt (#1109, M4-C skeleton)

Skeleton written by #1391; device columns added by #2008. **Every device result is "not run"**: only Nick's own runs on a physical Android phone fill the Result, Evidence and Who ran it columns. The Emulator column is a dry run on a development machine and is never device evidence (#1109, "separate kinds of evidence"). The steps that fill each row are in the run sheet, [`spikes/mobile/android/DEVICE-RUN.md`](../../spikes/mobile/android/DEVICE-RUN.md).

How to fill a row:

- **Result** is one of **pass** (every part of the row's pass criterion in [the capability matrix](mobile-capabilities.md#pending-capability-matrix) was checked and held), **fail** (a part was seen to fail), **not run** (not tried, setup missing, or only part checked: say what was seen in Evidence) or **unsupported** (a documented policy or OS limit, such as M-L4's "not offered on this OS").
- **Evidence** names the run (1 or 2), the run-sheet step, the measured figure where the row has one, and the screenshot or Diagnostics export file. No addresses, names, tokens, room or device IDs, conversations or audio.
- **Who ran it** is the person holding the phone. A row run by another tester is recorded with their name and counts as device evidence only once Nick has reviewed it.

## Environment

| Field | Value |
| --- | --- |
| Date | 2026-11-05 (emulator dry run); repeated 2026-11-10 on the refreshed branches (M4-C steps on #1391's head, M4-D steps on #1392's), same AVD |
| Spike commit | (filled when merged) |
| Device model / Android build / Play services | not run (see Device runs) |
| Emulator | Android 15 (API 35) x86_64 Google APIs AVD `conch35`, KVM, headless, no audio device |
| `@livekit/react-native` / `@livekit/react-native-webrtc` / `livekit-client` | 3.1.0 / 150.1.1 / 2.22.3 |
| conchd and LiveKit | the dev stack at the spike commit, via `adb reverse` (TCP only) |
| Second party | not run (device runs: separate principal or second session, per row) |

Schema copy: `src/logic/schema/{generated,validate}.ts`, verbatim from `web/src/schema/` at `db5c16c5`. Logic copies: from `web/src/voice/` at `ed4a68f9`.

## Device runs

| Field | Run 1 | Run 2 |
| --- | --- | --- |
| Date and start time | not run | not run |
| Who ran it | | |
| APK: CI build (#2007) or local recipe, and its commit | | |
| Includes PR #1985 (yes or no) | | |
| Phone model / Android build / Play services | | |
| Network (Wi-Fi, cellular) and battery saver | | |
| conchd and LiveKit versions (from the operator) | | |
| Listener: browser and device | | |
| Your second session: browser, desktop app or TUI | | |

## Camera-on return bug (#1972, fix in PR #1985)

Run-sheet steps 35 to 38. The verdict is recorded here, apart from the rows it touches (M-C3, M-C6, M-K2, M-L7).

| Field | Before the fix | With #1985 |
| --- | --- | --- |
| APK commit | not run | not run |
| Seconds away | | |
| Old room dropped during the SDK's reconnect (yes, no, unknown) | | |
| Order of connections and disconnections (operator's room history) | | |
| Duplicate-identity warning or take-over message on the phone | | |
| Hidden connection left after **Leave voice** or **Not in voice** | | |
| Verdict: reproduced, fixed, not fixed or inconclusive | | |

## Rows

| Id | Capability | Result | Evidence | Who ran it | Emulator (not device evidence) |
| --- | --- | --- | --- | --- | --- |
| M-F1 | Join and two-way audio with a second listener | not run | | | Pass (API level 35 emulator): sign-in link, Join, presence listed the principal; two-way audio and the 2 s bound not measured (no second listener, no real audio). |
| M-F2 | Hold to talk: start and stop reports, 20 presses | not run | | | Partial: a held press made the server show `transmitting` true, release made it false; audit has `voice_transmit_started` and `_stopped`, no `_unreported` (1 session, 2 presses). 20 presses and onset timing not run. |
| M-F3 | Mute, deafen, listen-only | not run | | | Not exercised on the emulator (no second listener); gate logic covered by unit tests. |
| M-F4 | Open microphone in the foreground, speaker echo | not run | | | not run |
| M-F5 | Touch cancelled by the system shuts the microphone | not run | | | not run |
| M-F6 | Press past 30 s shuts the microphone | not run | | | not run |
| M-F7 | Joining sends no microphone audio (20 joins, a republish) | not run | | | Partial: Join sent no report and did not set `transmitting` (3 joins). No republish, no 20 joins. |
| M-L1 | Phone browser (PWA) locked and in the background | not run | | | not run |
| M-L2 | App in voice, screen locked, 5 min of silence | not run | | | Partial: Home key, 8 s, service still foreground (types 0x82), presence kept the principal. 5 min not run. |
| M-L3 | App switched away, 5 min | not run | | | not run |
| M-L4 | Open microphone kept in the background (opt-in) | not run | | | not run |
| M-L5 | Lock or switch while a press is held | not run | | | Partial: lock via power key delivered the `lock` event and held the gate shut (UI says so); timing to the mute not measured. |
| M-L6 | Return or unlock never unmutes or joins (10 cycles) | not run | | | not run |
| M-L7 | The OS drops voice in the background: no hidden session | not run | | | not run |
| M-L8 | Lifecycle block in open mode (switch, lock, call) | not run | | | not run |
| M-L9 | Resume latch in open mode (return, call end, reconnect, mode change) | not run | | | not run |
| M-B1 | Bluetooth headset attached before Join | not run | | | not run |
| M-B2 | Headset attached and detached during voice | not run | | | not run |
| M-B3 | Earpiece, speaker and wired headset | not run | | | not run |
| M-B4 | Headset button as a talk key (unsupported by policy; recorded only) | not run | | | not run |
| M-I1 | Incoming phone call (idle and while pressing) | not run | | | not run |
| M-I2 | Assistant, alarm, another app taking the audio | not run | | | not run |
| M-I3 | Microphone permission revoked during voice | not run | | | not run |
| M-N1 | Wi-Fi to cellular and back | not run | | | not run |
| M-N2 | Airplane mode 60 s, then back | not run | | | not run |
| M-N3 | Room rotation during the call | not run | | | not run |
| M-K1 | Force-stop or swipe away during a held press | not run | | | not run |
| M-K2 | Leave voice | not run | | | Partial: Leave left the room (presence empty within 5 s) and the service was gone (`dumpsys`); OS indicator and the 2 s bound not observable. |
| M-K3 | Sign out while in voice | not run | | | not run |
| M-E1 | Battery and data: 30 min locked in voice on Wi-Fi vs idle | not run | | | not run |
| M-E2 | Battery and data: the same on cellular | not run | | | not run |
| M-E3 | Battery and data: 30 min app open, no voice | not run | | | not run |
| M-R1 | LAN server, plain HTTP development profile | not run | | | Pass for the development profile over `adb reverse` (plain HTTP to a loopback address on the host). |
| M-R2 | Public HTTPS server over cellular | not run | | | not run |
| M-R3 | Server with a private certificate authority | not run | | | not run |
| M-R4 | Voice down, server up | not run | | | not run |
| M-S1 | Phone takes voice from your other session (same room) | not run | | | not run |
| M-S2 | Your other session takes voice from the phone (same room) | not run | | | not run |
| M-S3 | Sign-in ended from another device during voice | not run | | | not run |
| M-S4 | Removed from the channel during voice | not run | | | not run |
| M-S5 | No companion route; Watch uses your own watch-only session | not run | | | Asserted by unit tests of the request list; no network log taken. |
| M-S6 | Different channels on two devices (recorded, not judged) | not run | | | not run |
| M-P1 | Microphone permission denied at Join | not run | | | Prompt appears at Join, not launch; the denied path is covered by a unit test only. |
| M-P2 | Notification permission denied | not run | | | Prompt appears at Join; denied path not run. |
| M-P3 | Android 14+: voice service started only from the Join press | not run | | | Partial: service started from the Join press (3 joins, no exception); 20 joins not run. |
| M-A1 | TalkBack through the main flow | not run | | | not run |
| M-A2 | Largest system text size | not run | | | not run |
| M-H1 | Lock-screen or notification talk control (unsupported by policy; recorded only) | not run | | | not run |
| M-H2 | Volume or other hardware key as a talk key (unsupported by policy; recorded only) | not run | | | not run |
| M-H3 | iOS Push to Talk framework: **iOS only**, no Android test | not run (iOS only) | | | not run |
| M-C1 | Camera claim and release | not run | | | Pass (emulator, M4-D, virtual front camera, hw.camera.front=emulated): the button claimed (`claimCamera` 200); `conch voice status` showed `quiet+camera`; LiveKit's room API listed a VIDEO/CAMERA track 640x360 with LOW and MEDIUM layers (a second view of the track, not a second subscriber); the button off ended it (`stopCamera` 204, track gone from the room, `quiet`). Front camera (device 1) was opened. Release on Leave not run on the emulator (unit-tested). |
| M-C2 | Camera off on joining | not run | | | Pass (emulator): after Join the camera was off, no camera claim in presence, and the CAMERA permission still `granted=false`. |
| M-C3 | Camera on, app switched away | not run | | | Pass (emulator): Home with the camera on closed camera device 1 in the same second (`dumpsys media.camera`), and the claim and the track were gone at the first poll, 0.5 s after the key. Timing is by polling, at 0.5 s steps. Found and fixed on this run: waiting for the SDK's unpublish held the release back until the app returned (React Native pauses JS timers in the background), so the unpublish is no longer waited for. |
| M-C4 | Camera on, screen locked | not run | | | Pass (emulator): power key with the camera on closed the camera device and ended the claim between 0.5 s and 1.0 s after the key (polling at 0.5 s); on wake the camera stayed off. |
| M-C5 | Camera on, interrupted (call, assistant, another camera app) | not run | | | Partial (emulator): an incoming call (`gsm call`) ended the camera and the claim within 0.5 s; the stock camera app opened over the app also ended it. In both the app also left the foreground, so the interruption reason is not separated from background. The voice assistant not run. |
| M-C6 | Camera never turns itself back on | not run | | | Pass (emulator): after Home, lock and the call, returning to the app left the camera off with the lifecycle note shown; no capture, claim or permission request followed (camera clients empty, presence without `+camera`). Waited seconds, not minutes. |
| M-C7 | Camera permission prompt and server-side camera switch | not run | | | Pass (emulator); re-run on the #1758 build (emulator, Android 35 google_apis, 2026-11-11): Pass. The prompt came at the first tap; "While using the app" started the camera with no second tap (camera device 1 opened); Home then closed it, and after returning one tap started it again. Earlier run (emulator): the CAMERA prompt came at the first tap only, not at launch or Join; Don't allow left the camera off with a note and no claim; the next tap prompted again, and While using the app started the camera with no second tap. Intended spike behaviour, not a defect: the camera keeps sending when the person leaves the Voice view for another screen while still in voice (presence polling stops there, the camera does not; the lifecycle and Leave stop it). |
| M-C7b | Camera permission dialog, then Home or Recents (#1758; part of M-C7) | not run | | | Emulator (Android 35 google_apis, #1758 build, 2026-11-11): with the CAMERA dialog up, Home and Recents each dismissed the dialog: on return it was gone, the camera stayed off with no capture and no claim, and the "camera is blocked" note showed (the dismissal counted as a denial; repeated, Android stopped asking until the permission flags were reset). So on this image the dialog does not survive Home or Recents and the stale-Allow hazard cannot arise; whether it survives on a real device (other OEMs) is open, so the row stays for a device. Split-screen focus change not run (not driven through adb). The dialog alone, then Allow, started the camera (M-C7). |
| M-C8 | Receiving other people's cameras | not run | | | not run on the emulator: no second camera publisher was available. Receive is unit-tested (subscription only from claim holders, only in the open and shown Voice view). |
| M-C9 | Hand-off with the camera on | not run | | | not run on the emulator (needs a second device of the same principal). |
| M-C10 | Camera battery and data | not run | | | not run |

## Emulator dry run, step by step (M4-C)

1. Sign in by a one-use sign-in link, server typed in, bearer in memory: pass. Channel list showed the member channel only: pass.
2. Join: presence check (nobody) then microphone and notification prompts at Join, foreground service started, session, room: pass. First attempt failed with the SDK's "Audio device module is not initialized": `LiveKitReactNative.setup` is needed in `Application.onCreate` (native, not reachable from JS).
3. Hold to talk (touch held 3 to 8 s): server presence showed `transmitting` true during and false after: pass.
4. Home key and screen lock while in voice: room kept, service foreground, gate held shut ("lock"); after unlock the block lifted and nothing sent: pass.
5. Leave: presence empty, service stopped: pass.
6. Audit afterwards: `voice_joined`/`voice_left`/`voice_session_issued` x3, `voice_transmit_started`/`_stopped` x2, no `voice_transmit_unreported`.

Not shown by the emulator: real audio (no audio device), LAN reachability, Bluetooth and wired routes, OS microphone indicator, `adb emu gsm call` interruption (not run), battery and data, TalkBack, Android 14 behaviour. Mute, deafen, open microphone, listen-only, the hand-off warning and displacement were not driven on the emulator (unit tests only).

## Findings for M6 (recorded, not fixed in the spike)

- React Native's JavaScript timers pause while the app is in the background. The presence poll, the retry timer and the `ReportQueue` retries all run on them, so a failed `stopped` report sent on Home waits for the app's return (the room stays up, and conchd's own unreported-transmit handling is the backstop). A production app needs a native or service-held timer for the report, or a report path that does not depend on JS being awake. Device rows M-L2 and M-L5 should watch for it.
- `VoiceService` calls `stopSelf()` after a failed `startForeground`. On some Android builds, a service that was started with `startForegroundService` and then stops without having called `startForeground` successfully can raise the "did not then call Service.startForeground()" crash. Not seen on the emulator; under M-P3 the device run should look for it.
- `CameraClient.stopNow` (`src/logic/camera.ts`) bounds the unpublish and a claim in flight with `bounded(..., unpublishWait)` and `bounded(..., claimWait)`. Those are JS timers too, so in the background they pause like the poll above: the capture is already stopped (synchronously, in the adapter), but a claim release that waits on `claimWait` waits for the app's return. Same family as the `stopCamera` finding: the adapter does not wait for the unpublish for this reason (M-C3).
- Camera tiles and the participants list name people "Person N", not by name. Presence documents carry principal ids only, and the spike makes no directory call. The production app (M8, M9) resolves names from the directory, as the browser does. M-C8's "each tile names its person" is therefore answered by the browser's pattern, not by this spike.
- `livekit_url` from the session grant is used as given. The server is the trust root for voice, so that is fine for the spike; a production release build must require `wss:` and refuse anything else.

## Manifest (to quote)

Source: `spikes/mobile/android/android/app/src/main/AndroidManifest.xml`. Service `.VoiceService` has `foregroundServiceType="microphone|mediaPlayback"` and no `camera` type; `allowBackup="false"`. The app declares RECORD_AUDIO, CAMERA (for M4-D), POST_NOTIFICATIONS, FOREGROUND_SERVICE(_MICROPHONE, _MEDIA_PLAYBACK), MODIFY_AUDIO_SETTINGS, ACCESS_NETWORK_STATE, CHANGE_NETWORK_STATE, BLUETOOTH_CONNECT (route rows M-B1 to M-B3), INTERNET.

The **merged debug manifest** (`app/build/intermediates/merged_manifest/debug/processDebugMainManifest/AndroidManifest.xml`) adds the following from libraries. What the spike keeps, and why:

| Entry | Brought by | Verdict |
| --- | --- | --- |
| WAKE_LOCK | `@livekit/react-native` | Kept. The SDK and its audio switch hold wake locks while a call runs; the foreground service already keeps the process up, so a device run (M-L2, M-L3) should show whether it is needed at all. |
| BLUETOOTH (max SDK 30) | `@livekit/react-native` | Kept, inert: `minSdkVersion` is 31, so it never applies. |
| BLUETOOTH_ADMIN | `@livekit/react-native` | Kept. The audio switch uses it for the Bluetooth headset route on the way to M-B1 to M-B3; Android 12+ routes also need BLUETOOTH_CONNECT (declared). Revisit once the device rows say whether it matters. |
| ACCESS_LOCAL_NETWORK | React Native's debug manifest | Kept, debug only. On Android 17 an app declaring it must hold it to reach a dev server over the LAN; Metro over `adb reverse` is loopback and exempt. |
| `DYNAMIC_RECEIVER_NOT_EXPORTED_PERMISSION` (signature) | AndroidX core | Kept. Generated for the non-exported receiver the screen-state broadcast uses. |
| SYSTEM_ALERT_WINDOW | React Native's debug manifest (dev overlay) | **Removed** with `tools:node="remove"`: the spike's dev loop does not need draw-over-apps. |
| `com.oney.WebRTCModule.MediaProjectionService` (`foregroundServiceType="mediaProjection"`, not exported) | `@livekit/react-native-webrtc` | **Removed** with `tools:node="remove"`: phone screen broadcasting is out of scope (ADR-016), and a mediaProjection service type has no place in a voice and camera spike. |

After the change `assembleDebug` builds and neither entry is in the merged manifest.

## Answers, recommendation

Not written: they need the device rows.
