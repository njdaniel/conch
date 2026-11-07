# iOS voice receipt (#1110, M5-C skeleton)

Skeleton written by #1395. **Every cell is "not run"**: nothing iOS-native has been compiled, signed or run (it was written on Linux, and no worker has a Mac). Only Nick's by-hand runs on his Mac and a physical iPhone fill the Device column. The Simulator column is a dry run on the Mac and is never device evidence. The rows are those of `docs/reviews/mobile-capabilities.md`; the camera rows M-C1 to M-C10 belong to M5-D and stay "not run" here.

## Environment

| Field | Value |
| --- | --- |
| Date | not run |
| Spike commit | not run |
| Mac / macOS / Xcode | not run |
| iPhone model / iOS build | not run |
| Simulator | not run |
| `@livekit/react-native` / `@livekit/react-native-webrtc` / `livekit-client` | 3.1.0 / 150.1.1 / 2.22.3 (the npm pins; the Pod versions come from `ios/Podfile.lock`, not run) |
| conchd and LiveKit | not run (the dev stack in LAN mode, #1366) |
| Second party | not run |

Schema copy: `src/logic/schema/{generated,validate}.ts`, verbatim from `web/src/schema/`. Logic copies: from `spikes/mobile/android` at `ea00f74f` (see each file's first line); `src/logic/ios.ts` is M5-B's.

## Rows

| Id | Capability | Device | Simulator |
| --- | --- | --- | --- |
| M-F1 | Join through `POST …/voice/session` and connect as `p<id>`; two-way audio with a second listener | not run | not run |
| M-F2 | Hold to talk: press opens after the `started` answer (bounded wait), release mutes before `stopped`; audit shows reported starts and stops, no `voice_transmit_unreported` | not run | not run |
| M-F3 | Mute, deafen, listen-only (`can_publish: false` publishes no track and opens no microphone) | not run | not run |
| M-F4 | Open microphone in the foreground: onset opens, hang shuts, speaker echo | not run | not run |
| M-F5 | Touch cancel (system gesture, notification shade, incoming-call banner) shuts the gate with no tail | not run | not run |
| M-F6 | Press past the bound shuts the gate | not run | not run |
| M-F7 | Joining without a press sends no microphone audio: the track is muted before it is published and the publication muted again after publishing and after a republish (the browser's order, #501) | not run | not run |
| M-L1 | The **browser** (PWA) on a locked and backgrounded phone: does the room stay connected, does playback continue, does capture continue | not run | not run |
| M-L2 | The app, in voice, screen locked, 5 min silence: room stays connected, remote speech heard on unlock within 1 s, gate shut throughout | not run | not run |
| M-L3 | App switched away (another app in front), 5 min: as M-L2 | not run | not run |
| M-L4 | Open microphone kept in the background (the opt-in): capture continues, indicator persistent, interruption shuts, resume needs the person | not run | not run |
| M-L5 | Lock or switch **while a press is held**: gate shuts at once, no tail; unlock finds idle; nothing transmits until a fresh press | not run | not run |
| M-L6 | Resume from background or unlock never unmutes or joins | not run | not run |
| M-L7 | Backgrounding where the OS does not keep the connection: the app leaves voice and says so on return; no silent zombie session | not run | not run |
| M-L8 | **Lifecycle block in open mode** (new work, design §4): with the open microphone on and a sustained loud source at the microphone, the app is backgrounded, then locked, then interrupted by a call | not run | not run |
| M-L9 | **Resume latch in open mode** (new work, design §4): the same sustained loud source, then the app returns to the foreground, the interruption ends, and separately a reconnect completes | not run | not run |
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
| M-K2 | Leave: `disconnect()` called, foreground service stopped, audio session deactivated, OS indicator gone | not run | not run |
| M-K3 | Sign out while in voice: media torn down before the credential is ended; keystore entry gone | not run | not run |
| M-E1 | 30 min in voice, screen locked, mostly silence, Wi-Fi: battery and data against a 30 min idle baseline on the same phone | not run | not run |
| M-E2 | The same on cellular | not run | not run |
| M-E3 | 30 min with the app open and no voice (chat feed only) | not run | not run |
| M-R1 | A server on a LAN address over Wi-Fi, plain HTTP development profile | not run | not run |
| M-R2 | A server behind a reverse proxy with a public certificate, over cellular | not run | not run |
| M-R3 | A server with a private CA | not run | not run |
| M-R4 | LiveKit down, conchd up | not run | not run |
| M-S1 | Phone joins while the browser or desktop holds `p<id>`: presence check, the warning, the displacement | not run | not run |
| M-S2 | The reverse: desktop or browser joins while the phone holds `p<id>` | not run | not run |
| M-S3 | Credential ended from another device while the phone is in voice | not run | not run |
| M-S4 | Removed from the channel while in voice | not run | not run |
| M-S5 | The phone never calls `…/voice/companion`; a watch uses the person's own watch-only session | not run | not run |
| M-S6 | **Different channels on two devices:** the desktop or browser in channel A's voice, the phone joins channel B's voice | not run | not run |
| M-P1 | Microphone permission denied at Join | not run | not run |
| M-P2 | Notification permission denied | not run | not run |
| M-P3 | Android 14+: the foreground service is started from the Join press (a visible activity) and never from the background | n/a on iOS (the matrix has no iOS cell; Android 14 only) | n/a |
| M-A1 | TalkBack / VoiceOver through sign-in, channel list, send, Join, Hold to talk, Leave | not run | not run |
| M-A2 | Largest system text size | not run | not run |
| M-H1 | Lock-screen or notification Hold to talk control | not run | not run |
| M-H2 | Volume or other hardware key as a talk key | not run | not run |
| M-H3 | iOS Push to Talk framework (system PTT UI, 16.0+) as a later path | not run | not run |
| M-C1 | **Claim and release** | not run | not run |
| M-C2 | **Off on joining** | not run | not run |
| M-C3 | **Background** with the camera on | not run | not run |
| M-C4 | **Screen lock** with the camera on | not run | not run |
| M-C5 | **Interruption** with the camera on: an incoming call, the voice assistant, and another app taking the camera | not run | not run |
| M-C6 | **No auto-resume** | not run | not run |
| M-C7 | **Permission prompt** | not run | not run |
| M-C8 | **Receiving others' cameras** | not run | not run |
| M-C9 | **Hand-off with the camera on** | not run | not run |
| M-C10 | **Battery and data** | not run | not run |

## What iOS delivered (to be filled)

`controller.audio.delivered` (logic/ios.ts `IosAudioEvents`) lists every interruption and route change iOS sent in a run, with the "should resume" option as received. The Diagnostics screen's export carries it. Paste it per run: not run.

## Simulator dry run, step by step (to be filled)

Not run. The steps are in `spikes/mobile/ios/README.md`, "Simulator dry run". The Simulator has no real microphone path, no Bluetooth and no interruption by phone call (Xcode can simulate a few; record which).

## Info.plist (to quote)

See `spikes/mobile/ios/ios/ConchSpike/Info.plist`: `UIBackgroundModes` = `audio` only (no `voip`, no `remote-notification`, no `fetch`); `NSMicrophoneUsageDescription`; `NSLocalNetworkUsageDescription`; `NSAppTransportSecurity` with `NSAllowsArbitraryLoads` false and `NSAllowsLocalNetworking` true (the dev stack over http and ws on the LAN). No camera key (M5-D).

## Answers, recommendation

Not written: they need the device rows.
