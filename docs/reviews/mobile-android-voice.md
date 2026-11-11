# Android voice receipt (#1109, M4-C skeleton)

Skeleton written by #1391. **Every device cell is "not run"**: only Nick's by-hand runs on a physical Android 14 phone fill them. The Emulator column is a dry run on this machine and is never device evidence.

## Environment

| Field | Value |
| --- | --- |
| Date | 2026-11-05 (emulator dry run); repeated 2026-11-10 on the refreshed branches (M4-C steps on #1391's head, M4-D steps on #1392's), same AVD |
| Spike commit | (filled when merged) |
| Device model / Android build / Play services | not run |
| Emulator | Android 15 (API 35) x86_64 Google APIs AVD `conch35`, KVM, headless, no audio device |
| `@livekit/react-native` / `@livekit/react-native-webrtc` / `livekit-client` | 3.1.0 / 150.1.1 / 2.22.3 |
| conchd and LiveKit | the dev stack at the spike commit, via `adb reverse` (TCP only) |
| Second party | not run (device runs: separate principal or second session, per row) |

Schema copy: `src/logic/schema/{generated,validate}.ts`, verbatim from `web/src/schema/` at `db5c16c5`. Logic copies: from `web/src/voice/` at `ed4a68f9`.

## Rows

| Id | Capability | Device | Emulator |
| --- | --- | --- | --- |
| M-F1 | Join through `POST …/voice/session` and connect as `p<id>`; two-way audio with a second listener | not run | Pass (API level 35 emulator): sign-in link, Join, presence listed the principal; two-way audio and the 2 s bound not measured (no second listener, no real audio). |
| M-F2 | Hold to talk: press opens after the `started` answer (bounded wait), release mutes before `stopped`; audit sho | not run | Partial: a held press made the server show `transmitting` true, release made it false; audit has `voice_transmit_started` and `_stopped`, no `_unreported` (1 session, 2 presses). 20 presses and onset timing not run. |
| M-F3 | Mute, deafen, listen-only (`can_publish: false` publishes no track and opens no microphone) | not run | Not exercised on the emulator (no second listener); gate logic covered by unit tests. |
| M-F4 | Open microphone in the foreground: onset opens, hang shuts, speaker echo | not run | not run |
| M-F5 | Touch cancel (system gesture, notification shade, incoming-call banner) shuts the gate with no tail | not run | not run |
| M-F6 | Press past the bound shuts the gate | not run | not run |
| M-F7 | Joining without a press sends no microphone audio: the track is muted before it is published and the publicati | not run | Partial: Join sent no report and did not set `transmitting` (3 joins). No republish, no 20 joins. |
| M-L1 | The browser (PWA) on a locked and backgrounded phone: does the room stay connected, does playback continue, do | not run | not run |
| M-L2 | The app, in voice, screen locked, 5 min silence: room stays connected, remote speech heard on unlock within 1  | not run | Partial: Home key, 8 s, service still foreground (types 0x82), presence kept the principal. 5 min not run. |
| M-L3 | App switched away (another app in front), 5 min: as M-L2 | not run | not run |
| M-L4 | Open microphone kept in the background (the opt-in): capture continues, indicator persistent, interruption shu | not run | not run |
| M-L5 | Lock or switch while a press is held: gate shuts at once, no tail; unlock finds idle; nothing transmits until  | not run | Partial: lock via power key delivered the `lock` event and held the gate shut (UI says so); timing to the mute not measured. |
| M-L6 | Resume from background or unlock never unmutes or joins | not run | not run |
| M-L7 | Backgrounding where the OS does not keep the connection: the app leaves voice and says so on return; no silent | not run | not run |
| M-L8 | Lifecycle block in open mode (new work, design §4): with the open microphone on and a sustained loud source at | not run | not run |
| M-L9 | Resume latch in open mode (new work, design §4): the same sustained loud source, then the app returns to the f | not run | not run |
| M-B1 | Bluetooth headset attached before Join: capture and playback on it | not run | not run |
| M-B2 | Headset attached mid-call and detached mid-call | not run | not run |
| M-B3 | Earpiece, speaker, wired headset switching | not run | not run |
| M-B4 | Headset button as a talk key | not run | not run |
| M-I1 | Incoming phone call while in voice (idle, and while a press is held) | not run | not run |
| M-I2 | Siri / Assistant, alarm, another app taking the audio focus or session | not run | not run |
| M-I3 | Permission revoked mid-call (microphone off in system settings) | not run | not run |
| M-N1 | Wi-Fi to cellular and back while in voice | not run | not run |
| M-N2 | Airplane mode 60 s, then back | not run | not run |
| M-N3 | Room rotation during the call | not run | not run |
| M-K1 | Force-stop or swipe-away during a held press | not run | not run |
| M-K2 | Leave: `disconnect()` called, foreground service stopped, audio session deactivated, OS indicator gone | not run | Partial: Leave left the room (presence empty within 5 s) and the service was gone (`dumpsys`); OS indicator and the 2 s bound not observable. |
| M-K3 | Sign out while in voice: media torn down before the credential is ended; keystore entry gone | not run | not run |
| M-E1 | 30 min in voice, screen locked, mostly silence, Wi-Fi: battery and data against a 30 min idle baseline on the  | not run | not run |
| M-E2 | The same on cellular | not run | not run |
| M-E3 | 30 min with the app open and no voice (chat feed only) | not run | not run |
| M-R1 | A server on a LAN address over Wi-Fi, plain HTTP development profile | not run | Pass for the development profile over `adb reverse` (plain HTTP to a loopback address on the host). |
| M-R2 | A server behind a reverse proxy with a public certificate, over cellular | not run | not run |
| M-R3 | A server with a private CA | not run | not run |
| M-R4 | LiveKit down, conchd up | not run | not run |
| M-S1 | Phone joins while the browser or desktop holds `p<id>`: presence check, the warning, the displacement | not run | not run |
| M-S2 | The reverse: desktop or browser joins while the phone holds `p<id>` | not run | not run |
| M-S3 | Credential ended from another device while the phone is in voice | not run | not run |
| M-S4 | Removed from the channel while in voice | not run | not run |
| M-S5 | The phone never calls `…/voice/companion`; a watch uses the person's own watch-only session | not run | Asserted by unit tests of the request list; no network log taken. |
| M-S6 | Different channels on two devices: the desktop or browser in channel A's voice, the phone joins channel B's vo | not run | not run |
| M-P1 | Microphone permission denied at Join | not run | Prompt appears at Join, not launch; the denied path is covered by a unit test only. |
| M-P2 | Notification permission denied | not run | Prompt appears at Join; denied path not run. |
| M-P3 | Android 14+: the foreground service is started from the Join press (a visible activity) and never from the bac | not run | Partial: service started from the Join press (3 joins, no exception); 20 joins not run. |
| M-A1 | TalkBack / VoiceOver through sign-in, channel list, send, Join, Hold to talk, Leave | not run | not run |
| M-A2 | Largest system text size | not run | not run |
| M-H1 | Lock-screen or notification Hold to talk control | not run | not run |
| M-H2 | Volume or other hardware key as a talk key | not run | not run |
| M-H3 | iOS Push to Talk framework (system PTT UI, 16.0+) as a later path | not run | not run |
| M-C1 | Claim and release | not run | Pass (emulator, M4-D, virtual front camera, hw.camera.front=emulated): the button claimed (`claimCamera` 200); `conch voice status` showed `quiet+camera`; LiveKit's room API listed a VIDEO/CAMERA track 640x360 with LOW and MEDIUM layers (a second view of the track, not a second subscriber); the button off ended it (`stopCamera` 204, track gone from the room, `quiet`). Front camera (device 1) was opened. Release on Leave not run on the emulator (unit-tested). |
| M-C2 | Off on joining | not run | Pass (emulator): after Join the camera was off, no camera claim in presence, and the CAMERA permission still `granted=false`. |
| M-C3 | Background with the camera on | not run | Pass (emulator): Home with the camera on closed camera device 1 in the same second (`dumpsys media.camera`), and the claim and the track were gone at the first poll, 0.5 s after the key. Timing is by polling, at 0.5 s steps. Found and fixed on this run: waiting for the SDK's unpublish held the release back until the app returned (React Native pauses JS timers in the background), so the unpublish is no longer waited for. |
| M-C4 | Screen lock with the camera on | not run | Pass (emulator): power key with the camera on closed the camera device and ended the claim between 0.5 s and 1.0 s after the key (polling at 0.5 s); on wake the camera stayed off. |
| M-C5 | Interruption with the camera on: an incoming call, the voice assistant, and another app taking the camera | not run | Partial (emulator): an incoming call (`gsm call`) ended the camera and the claim within 0.5 s; the stock camera app opened over the app also ended it. In both the app also left the foreground, so the interruption reason is not separated from background. The voice assistant not run. |
| M-C6 | No auto-resume | not run | Pass (emulator): after Home, lock and the call, returning to the app left the camera off with the lifecycle note shown; no capture, claim or permission request followed (camera clients empty, presence without `+camera`). Waited seconds, not minutes. |
| M-C7 | Permission prompt | not run | Pass (emulator): the CAMERA prompt came at the first tap only, not at launch or Join; Don't allow left the camera off with a note and no claim; the next tap prompted again, and While using the app started the camera with no second tap. Intended spike behaviour, not a defect: the camera keeps sending when the person leaves the Voice view for another screen while still in voice (presence polling stops there, the camera does not; the lifecycle and Leave stop it). |
| M-C8 | Receiving others' cameras | not run | not run on the emulator: no second camera publisher was available. Receive is unit-tested (subscription only from claim holders, only in the open and shown Voice view). |
| M-C9 | Hand-off with the camera on | not run | not run on the emulator (needs a second device of the same principal). |
| M-C10 | Battery and data | not run | not run |

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

## Manifest (to quote)

See `spikes/mobile/android/android/app/src/main/AndroidManifest.xml`: service `.VoiceService` `foregroundServiceType="microphone|mediaPlayback"`, no `camera` type; permissions RECORD_AUDIO, CAMERA (for M4-D), POST_NOTIFICATIONS, FOREGROUND_SERVICE(_MICROPHONE, _MEDIA_PLAYBACK), MODIFY_AUDIO_SETTINGS, ACCESS_NETWORK_STATE, CHANGE_NETWORK_STATE, BLUETOOTH_CONNECT (route rows M-B1 to M-B3); `allowBackup="false"`.

## Answers, recommendation

Not written: they need the device rows.
