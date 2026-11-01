# Mobile M1 capability record

Issue [#1106](https://github.com/njdaniel/conch/issues/1106) · 2026-11-01 · Baseline
`7472b65c2756530370e28a547817e46afbffdb92`. Design: [mobile-app.md](../design/mobile-app.md);
proposed ADR: [ADR-016](../adr/ADR-016-mobile-app.md).

**Disposition: planning only. No phone, Mac, Xcode, emulator or simulator was available or
used. No device row has run. No supported phone, OS or device model is declared.** The
repository facts below were read at the baseline commit; the external facts were read on
2026-11-01 from the pages the design's §12 names. M4 ([#1109](https://github.com/njdaniel/conch/issues/1109),
Android) and M5 ([#1110](https://github.com/njdaniel/conch/issues/1110), iPhone) fill the
matrix, each OS independently; an emulator or simulator result they obtain is recorded in a
separate column and never in a device cell.

## Evidence definitions

The Linux desktop report's definitions, reused unchanged:

- **Observed:** a direct runtime result on the recorded device and build.
- **Synthetic:** real processes, network and media with generated input; no physical device or
  human listener. It can refute a topology; it cannot certify push-to-talk.
- **Source-inspected:** code in this repository at the baseline, or a tagged upstream API; no
  runtime capability claim.
- **Verified-by-docs:** a cited official page states the mechanism; a statement about the API,
  not about a Conch build on a real phone.
- **Memory:** written without a page or a file behind it; said so in the text.
- **Unavailable:** the probed interface is absent in the environment. Permission denial is not
  proof of absence.
- **Pending:** the experiment has not run. It is not an unsupported result.
- **Supported / unsupported:** reserved for an explicit product decision after the acceptance
  scenario; this record assigns neither, except where the design says "unsupported by policy".

## Environment

| Component | Observation |
| --- | --- |
| Repository | main `7472b65`, read in a clean worktree; `make check` run for the documents (see Repository verification) |
| Phones | **none available.** Nick's Android phone and iPhone models are unknown to this record; M4 and M5 record them |
| Mac, Xcode | **none available** to this work; a prerequisite M5 lists |
| Android SDK, Android Studio, JDK | not installed in this environment; not needed for a documents-only change |
| LiveKit | not started; the Linux report's 507 ms displacement receipt (LiveKit 1.13.7, Rust SDK 0.9.4, synthetic, n = 1) is reused as source material and not re-run |
| Emulator, simulator | **not used** |
| External pages | read 2026-11-01; those that returned no body are listed under Fetches that failed |

## Findings

### F1 — Every route of the phone story exists except link-making, the directory, the voice switch and push

**Source-inspected** (`internal/server/routes.go`, `web/src/platform/operations.ts`): sign-in
(`/v1/login`, `/v1/login/totp`), invite preview and v2 redemption (`/v1/invites/preview`,
`/v2/invites/redeem`), address joining (`/v1/join`, `/v1/join/account`), one-use device sign-in
links (`/v1/sign-in-links/mine`, `/preview`, `/redeem`), `whoami`, own credentials (list, end
one, end current, revoke all), channels, members, v2 messages, read state, files, the four
ticketed sockets, voice session, presence, transmit reports, occupancy, share claim, companion,
problem reports. **Not on main:** `POST /v2/invites` and its list and joiners (I6a, #985 open),
`GET /v1/directory/*`, channel join, leave and settings (I3a, I3b, I4 open), `PUT
/v1/principals/{id}/voice-permission` (I5 open), any push registration (M3, M11). Guests are
refused everywhere (#1005 closed). Design §1.1 and §1.5 have the table.

**Action:** the phone's redeem path and device-onboarding path need no new route; M7 (#1112)
keeps #978, #986 and #987 as its prerequisites for link-making and open sign-up.

### F2 — The modules that carry the protocol discipline are DOM-free; the views are not

**Source-inspected:** `grep -lE 'document\.|window\.|navigator\.|localStorage|sessionStorage|HTMLElement|React'`
over `web/src/state/*.ts` (tests excluded) names only `drafts.ts` and `draftStore.ts`.
`web/src/platform/operations.ts`, `web/src/schema/{generated,validate}.ts`,
`web/src/state/{auth,channels,messages,attachments,connection,feed}.ts`,
`web/src/voice/{gate,disconnect}.ts` import no DOM, **but `gate.ts` is reused only with
additions**: its `lost()` shuts push-to-talk only (lines 111–116, "The open microphone does not
depend on focus") and it has no sounding state or fresh-onset latch, so a phone must add a
lifecycle block for both modes and a resume latch (design §4; rows M-L8, M-L9);
`web/src/voice/connection.ts` is already transport-injected: its `../api` import is `import type`
(line 64, erased at compile time) and the session, report and share-claim calls are injected
callbacks (lines 313–316), so no `api.ts` split is needed; `keys.ts`, `useVoice.ts` and every
`.tsx` bind the DOM. `generated.ts` holds 238 exported types and is written by `go generate
./pkg/schema` from `pkg/schema/ts.go`; `object()` in `validate.ts` ignores unknown fields.

**Action:** a React Native client imports these as they stand (M6 decides in place or a second
generated copy); a Kotlin or Swift client needs new emitters with fixture conformance before any
screen (design §1.3, §1.4).

### F3 — No page read lets anyone promise voice through a screen lock from a phone browser

**Verified-by-docs (negative):** MDN's `getUserMedia` page says nothing about backgrounding;
the WebKit Web Push post covers push only (16.4, Home Screen web apps); web.dev's push overview
is a 2020 page about service workers. No Chrome or WebKit statement on microphone capture in a
backgrounded or locked phone browser was found. **Source-inspected:** the React client shuts the
gate on `blur`, `pagehide` and `visibilitychange` (`useVoice.ts` lines 433–450) and says so to
the person (mobile-browser-layout §6).

**Verified-by-docs (positive, for a native app):** the LiveKit React Native README names an
Android foreground service with `FOREGROUND_SERVICE_MICROPHONE` and
`FOREGROUND_SERVICE_MEDIA_PLAYBACK` for staying connected in the background, and the `audio`
and `voip` `UIBackgroundModes` on iOS; Apple's `playAndRecord` page says the `audio` background
mode continues audio "when the screen locks"; Android requires a declared service type from
Android 14 and refuses a microphone service created from the background with a
`SecurityException` on 14+.

**Action:** the PWA is not the phone app (design §2.1); row M-L1 keeps the browser question
honest; rows M-L2 to M-L5 are the native experiments.

### F4 — One voice owner **per room** is already enforced by displacement, and every client already stops on it; across rooms nothing is

**Source-inspected:** `voiceIdentity` gives `p<id>`, "one principal, one connection per room"
(`voice.go` line 31); rooms are per channel, and LiveKit's displacement acts inside one room, so
a desktop in channel A's voice and a phone in channel B's voice both stand and both may
transmit; `VoiceChannelOccupancy` is `{channel_id, in_voice}`, counts only, so a client cannot
cheaply see that its own principal is in voice elsewhere. `voiceSession` records a holder and
signs on every request (conchd does not refuse a second session; `TestVoiceSessionRecordsTheHolder`);
`afterDisconnect('duplicate_identity')` is `stop`, "Another device took over your place in this
voice room" (`web/src/voice/disconnect.ts` line 24); conch-voice.md §5 has the helper's row;
presence folds `.screen` and `.view` into one `VoiceParticipant` per principal, so a phone can
see its own principal already present. **Synthetic (reused):** the Linux report's 507 ms
`DUPLICATE_IDENTITY` receipt. **Source-inspected:** the companion route issues `.screen` only
with `p<id>` present and refuses a second (409 `voice_companion_busy`); `.view` is for a browser
tab beside native voice on one machine.

**Action:** the design proposes hand-off by explicit displacement within a room, with a
presence check and words before Join and Watch, no simultaneous devices in a room, no phone use
of `.screen` or `.view`, and names a server-enforced global owner as a future issue and a
question for Nick (design §3.2 point 5, §10). Rows M-S1 to M-S6.

### F5 — A VoIP push is not a chat notification; FCM needs the Play Store app

**Verified-by-docs:** "For apps built using the iOS 13 SDK or later, PushKit requires you to use
CallKit when handling VoIP calls" and "If you are unable to support CallKit in your app, you
cannot use PushKit to handle push notifications" (Apple DocC JSON); "FCM clients require devices
running Android 7.0 or higher that also have the Google Play Store app installed" (Firebase).

**Action:** constraints handed to M3 (#1108); the design fixes only that no push is a supported
configuration and that a notification never joins or unmutes (design §6).

### F6 — Toolchain and licence facts for the experiment pins

**Verified-by-docs (2026-11-01):** React Native 0.88.1 (MIT; New Architecture default; Node
22.11+; JDK 17; Android SDK Platform 35, Build-Tools 36.0.0; "A Mac is required to build
projects with native code for iOS"); `@livekit/react-native` 3.1.0 (Apache-2.0; requires
`livekit-client` ^2.20.0; Expo through development builds; CocoaPods trunk stops accepting new
pods 2026-12-02); `livekit-client` 2.22.3 pinned, 2.22.4 and 2.23.0 exist; `react-native-webrtc`
MIT and "not available in the Expo Go app"; Expo SDK 58 (MIT; RN 0.88; Android 7+; iOS 16.4+);
Capacitor 8.6.0 (MIT; WKWebView; Android System WebView); `client-sdk-android` 2.29.2 and
`client-sdk-swift` 2.19.0 (Apache-2.0); Xcode 27.2 on macOS 26.6; Apple Developer Program 99 USD
a year; free provisioning 3 devices and 7-day profiles; Play 25 USD and the 12-tester, 14-day
closed test for personal accounts made after 2023-11-13; Swift Apache-2.0 with runtime library
exception. **Not read:** the Android and Swift SDKs' minimum OS versions (their build files were
not fetched); `@livekit/react-native-webrtc`'s version; any transitive licence.

**Action:** design §7.2 is the proposed table; M4 and M5 pin, inventory and licence-scan the
resolved set and Nick signs each line in their PRs.

## Pending capability matrix

Rows carry ids the design's §4, §5, §7.4 and §11 use. Columns are the two OS lanes; a cell is
filled only by the experiment on a physical device of that OS and names the device model in the
receipt. `P` = pending real trial, `D` = verified-by-docs or source-inspected (mechanism only),
`—` = not applicable, `U` = unsupported by policy in this phase. **Every device cell is P.** An
emulator or simulator result goes in its own column of the M4 or M5 report, never here.

Pass criteria are measured; where a number is given it is the design's proposal and the
experiment records the actual figure beside the verdict.

| Id | Capability | Pass criterion (measured) | Android | iOS | Experiment |
| --- | --- | --- | --- | --- | --- |
| **Foreground** | | | | | |
| M-F1 | Join through `POST …/voice/session` and connect as `p<id>`; two-way audio with a second listener | heard both ways within 2 s of connect; presence lists the phone's principal | P | P | ME1 |
| M-F2 | Hold to talk: press opens after the `started` answer (bounded wait), release mutes before `stopped`; audit shows reported starts and stops, no `voice_transmit_unreported` | 20 presses, 0 unreported rows; first loud frame heard ≤ 200 ms after the unmute (the helper's measured figure is 14–80 ms, conch-voice.md §3; the browser's tail measurements are in `web/e2e/32-voice-onset.spec.ts`) | P | P | ME1 |
| M-F3 | Mute, deafen, listen-only (`can_publish: false` publishes no track and opens no microphone) | a second listener hears nothing while muted; deafen stops local playback; listen-only shows no OS microphone indicator | P | P | ME1 |
| M-F4 | Open microphone in the foreground: onset opens, hang shuts, speaker echo | with speakers on, the second listener's speech is not returned above the threshold for the device's processing settings; otherwise the warning is shown | P | P | ME1 |
| M-F5 | Touch cancel (system gesture, notification shade, incoming-call banner) shuts the gate with no tail | ≤ 50 ms from the cancel event to the mute; a fresh press needed | P | P | ME1 |
| M-F6 | Press past the bound shuts the gate | at 30 s (proposed) the track mutes, `stopped` is reported, UI says so | P | P | ME1 |
| M-F7 | Joining without a press sends no microphone audio: the track is muted before it is published and the publication muted again after publishing and after a republish (the browser's order, #501) | across 20 joins, and across a forced republish, LiveKit lists the track muted from the first; the poller never sees the microphone transmitting; the audit holds no `voice_transmit_*` row for the join; the second listener hears nothing | P | P | ME1 |
| **Lock and background** | | | | | |
| M-L1 | The **browser** (PWA) on a locked and backgrounded phone: does the room stay connected, does playback continue, does capture continue | recorded for the record only; no promise follows a pass | P | P | ME2 |
| M-L2 | The app, in voice, screen locked, 5 min silence: room stays connected, remote speech heard on unlock within 1 s, gate shut throughout | no `left` in presence; LiveKit does not drop the participant (15 s rule); OS indicator visible the whole time | P | P | ME2 |
| M-L3 | App switched away (another app in front), 5 min: as M-L2 | as M-L2 | P | P | ME2 |
| M-L4 | Open microphone kept in the background (the opt-in): capture continues, indicator persistent, interruption shuts, resume needs the person | all four of design §4.2 (a)–(d); else "not offered on this OS" | P | P | ME2 |
| M-L5 | Lock or switch **while a press is held**: gate shuts at once, no tail; unlock finds idle; nothing transmits until a fresh press | ≤ 100 ms from the lifecycle event to the mute; audit shows the `stopped` | P | P | ME2 |
| M-L6 | Resume from background or unlock never unmutes or joins | 10 cycles, 0 transmissions without a press; the detector in open mode needs a fresh onset | P | P | ME2 |
| M-L7 | Backgrounding where the OS does not keep the connection: the app leaves voice and says so on return; no silent zombie session | presence shows `left` within the poller's bounds; the UI's words recorded | P | P | ME2 |
| M-L8 | **Lifecycle block in open mode** (new work, design §4): with the open microphone on and a sustained loud source at the microphone, the app is backgrounded, then locked, then interrupted by a call | in each case the track mutes ≤ 100 ms after the lifecycle event and stays muted while the sound continues; `stopped` reported once; no `voice_transmit_unreported`; the second listener hears the sound stop and not return | P | P | ME2 |
| M-L9 | **Resume latch in open mode** (new work, design §4): the same sustained loud source, then the app returns to the foreground, the interruption ends, and separately a reconnect completes | nothing transmits while the sound continues; only after the hang time of quiet and then a fresh onset does the gate open; the second listener confirms; a mode change from push-to-talk to open under sustained sound behaves the same | P | P | ME2 |
| **Bluetooth and routes** | | | | | |
| M-B1 | Bluetooth headset attached before Join: capture and playback on it | the second listener confirms the headset microphone; route shown in the UI | P | P | ME3 |
| M-B2 | Headset attached mid-call and detached mid-call | route follows within 2 s; **a detach while a press is held shuts the gate** (the microphone switched: conch-voice.md §3 rule); no audio burst on the speaker above conversational level | P | P | ME3 |
| M-B3 | Earpiece, speaker, wired headset switching | each route works; the choice survives a reconnect (Android 12 earpiece bug fixed in LiveKit 2.26.1 is re-checked) | P | P | ME3 |
| M-B4 | Headset button as a talk key | **U** in this phase; recorded only: whether press and release both arrive to the app in the background | U / P | U / P | ME3 |
| **Interruptions** | | | | | |
| M-I1 | Incoming phone call while in voice (idle, and while a press is held) | gate shut ≤ 100 ms; room stays or is rejoined by explicit act only; after the call nothing transmits until a fresh press | P | P | ME4 |
| M-I2 | Siri / Assistant, alarm, another app taking the audio focus or session | as M-I1; the OS "should resume" hint is not acted on | P | P | ME4 |
| M-I3 | Permission revoked mid-call (microphone off in system settings) | capture ends, gate blocked, UI says why, no crash, no retry loop | P | P | ME4 |
| **Network** | | | | | |
| M-N1 | Wi-Fi to cellular and back while in voice | reconnected within 10 s each way (LiveKit ICE restart or full reconnect); gate shut after reconnect until a fresh press; no duplicate participant | P | P | ME5 |
| M-N2 | Airplane mode 60 s, then back | the disconnect table's `retry`, then a fresh session; the voice socket and the message feed re-ticket and reconnect; nothing transmits on its own | P | P | ME5 |
| M-N3 | Room rotation during the call | fresh session, gate shut, no resume of anything | P | P | ME5 |
| **Kill and cleanup** | | | | | |
| M-K1 | Force-stop or swipe-away during a held press | LiveKit drops the participant within ~15 s; the poller writes `left` and closes the reported `started` within its bounds; the next launch does not rejoin | P | P | ME6 |
| M-K2 | Leave: `disconnect()` called, foreground service stopped, audio session deactivated, OS indicator gone | indicator gone ≤ 2 s; presence shows `left` within one pass | P | P | ME6 |
| M-K3 | Sign out while in voice: media torn down before the credential is ended; keystore entry gone | no token or socket survives; relaunch shows the sign-in screen | P | P | ME6 |
| **Battery and data** | | | | | |
| M-E1 | 30 min in voice, screen locked, mostly silence, Wi-Fi: battery and data against a 30 min idle baseline on the same phone | recorded; the design proposes no threshold before a baseline exists; M9/M10 set one from these figures | P | P | ME7 |
| M-E2 | The same on cellular | recorded | P | P | ME7 |
| M-E3 | 30 min with the app open and no voice (chat feed only) | recorded | P | P | ME7 |
| **Self-hosted reachability** | | | | | |
| M-R1 | A server on a LAN address over Wi-Fi, plain HTTP development profile | works; the UI marks the profile as development | P | P | ME8 |
| M-R2 | A server behind a reverse proxy with a public certificate, over cellular | works; WebSocket tickets and the LiveKit URL reachable | P | P | ME8 |
| M-R3 | A server with a private CA | works only with the CA installed in the OS store; no validation override offered | P | P | ME8 |
| M-R4 | LiveKit down, conchd up | chat works; Join says voice is unavailable; no crash | P | P | ME8 |
| **Session and identity** | | | | | |
| M-S1 | Phone joins while the browser or desktop holds `p<id>`: presence check, the warning, the displacement | the other side stops with its "another device" words within 1 s and does not rejoin; the phone is heard | P | P | ME1 |
| M-S2 | The reverse: desktop or browser joins while the phone holds `p<id>` | the phone stops, says so, does not rejoin; the foreground service or audio session ends | P | P | ME1 |
| M-S3 | Credential ended from another device while the phone is in voice | socket closes ≤ 30 s (recheck) or at once; the participant is evicted; the phone shows signed out and tears media down | P | P | ME6 |
| M-S4 | Removed from the channel while in voice | 404 on the channel; evicted; the phone leaves and drops the channel | P | P | ME6 |
| M-S5 | The phone never calls `…/voice/companion`; a watch uses the person's own watch-only session | asserted in the spike's code and its network log | D / P | D / P | ME1 |
| M-S6 | **Different channels on two devices:** the desktop or browser in channel A's voice, the phone joins channel B's voice | **recorded, not judged:** both connections stay up (no `duplicate_identity`), presence lists the person in both rooms, each device can transmit into its own room; the phone shows no warning (it cannot see the other room from occupancy); the figures go to Nick's M1-4 decision on a global owner | D / P | D / P | ME1 |
| **Permissions** | | | | | |
| M-P1 | Microphone permission denied at Join | Join fails closed with the OS's words; chat unaffected | P | P | ME4 |
| M-P2 | Notification permission denied | the app works; no-push configuration | P | P | ME4 |
| M-P3 | Android 14+: the foreground service is started from the Join press (a visible activity) and never from the background | no `ForegroundServiceStartNotAllowedException` or `SecurityException` in 20 joins; a join attempted from a background path is refused by design | P | — | ME2 |
| **Accessibility** | | | | | |
| M-A1 | TalkBack / VoiceOver through sign-in, channel list, send, Join, Hold to talk, Leave | every control named; Hold to talk operable (the pattern is M2's) | P | P | ME9 |
| M-A2 | Largest system text size | no clipped control; the strip's four controls remain 44 pt | P | P | ME9 |
| **Hardware and background PTT (unsupported in this phase)** | | | | | |
| M-H1 | Lock-screen or notification Hold to talk control | **U**; recorded only: do press and release both arrive | U / P | U / P | ME3 |
| M-H2 | Volume or other hardware key as a talk key | **U**; recorded only | U / P | U / P | ME3 |
| M-H3 | iOS Push to Talk framework (system PTT UI, 16.0+) as a later path | **U**; recorded only: whether it fits Conch's gate rule (no system-initiated transmit) | — | U / P | ME3 |

There are no supported rows.

## Experiments

Each runs on a physical device, records date, commit, spike build, OS build, device model,
LiveKit RN SDK and `livekit-client` versions, the non-production server and LiveKit versions,
and keeps tokens, addresses, device identifiers, conversations and audio out of the receipt.
Every experiment but ME8's reachability rows needs a second listener. An emulator or simulator
run of any of these is recorded in a separate column of the M4 or M5 report.

| Id | Experiment | Settles |
| --- | --- | --- |
| ME1 | Join, talk, mute, deafen, listen-only, open microphone, touch cancel, the bound; join without a press and a forced republish, watching LiveKit's listed mute state and the audit; the hand-off in both directions against a browser tab and, where available, the Linux desktop or TUI, in the same room and then in different channels; the network log shows no companion request | M-F1 to M-F7, M-S1, M-S2, M-S5, M-S6 |
| ME2 | Lock and background: the PWA first for the record (M-L1), then the app: 5 min locked, 5 min switched away, each with silence and then speech; a press held into the lock; the open-mic opt-in; resume cycles; the Android 14 start rules; the open-mode lifecycle block and resume latch under a sustained loud source (a tone or speech recording at the microphone) | M-L1 to M-L9, M-P3 |
| ME3 | Routes: Bluetooth before and during, detach while held, earpiece, speaker, wired; headset and hardware buttons recorded for the unsupported rows; iOS Push to Talk read-only assessment | M-B1 to M-B4, M-H1 to M-H3 |
| ME4 | Interruptions: a real incoming call to the phone, Siri or Assistant, an alarm; permission revoked mid-call; denied at Join; notifications denied | M-I1 to M-I3, M-P1, M-P2 |
| ME5 | Network: Wi-Fi to cellular and back, airplane mode, a room rotation forced from the server | M-N1 to M-N3 |
| ME6 | Kill and cleanup: force-stop while held; Leave; sign out in voice; credential ended from another device; removed from the channel | M-K1 to M-K3, M-S3, M-S4 |
| ME7 | Energy and data: three 30 min runs against an idle baseline on the same phone, read from the OS's own battery and data screens | M-E1 to M-E3 |
| ME8 | Reachability: LAN plain HTTP, public certificate over cellular, private CA, LiveKit down | M-R1 to M-R4 |
| ME9 | Accessibility: screen reader walk-through and largest text, against M2's flows | M-A1, M-A2 |

## Fetches that returned no usable text (2026-11-01)

Recorded so that no one reads a missing citation as a checked one. Apple's HTML documentation
is script-rendered and returned titles only; the DocC JSON endpoints answered for most pages
and are the citations used.

| Page | Result | Consequence |
| --- | --- | --- |
| Apple `UIBackgroundModes` (HTML; JSON gave the value list but no per-value text) | value list only | the meaning of `audio` and `voip` is taken from the `playAndRecord` page and the LiveKit RN README |
| Apple `AVAudioSession` category options `allowBluetooth`, `allowBluetoothA2DP` (JSON 404) | not read | Bluetooth option facts come from the LiveKit RN release notes; verified on device in ME3 |
| Apple Push to Talk (JSON gave the abstract and overview, not the entitlement or push type) | partial | M-H3 stays a read-only assessment |
| Android `AudioManager` reference (index only), Bluetooth audio guide (404), media-button guide (404), battery-optimisation guide (404) | not read | `setCommunicationDevice`, audio focus on an incoming call and headset key events are memory, marked so; measured in ME3 and ME4 |
| Firebase `concept-options` (404) | not read | data versus notification messages left to M3 |
| Chrome or WebKit statement on background `getUserMedia` on phones | not found | M-L1 measures it |
| Expo `prebuild` and `expo-go` pages (404 at the paths tried) | not read | Expo facts limited to the versions table and the `react-native-webrtc` README |
| Android and Swift LiveKit SDK minimum OS versions (build files not fetched) | not read | the floors in design §5 rest on the OS rules and Expo's table, not on the SDKs; M4 and M5 confirm |

## Security review

None was requested for this record: it adds no code, route, dependency or wire shape. The
design's §8 lists the review targets; M4 and M5 carry a `security-reviewer` pass for their
credential handling; M7, M11, M12, M9 and M10 carry theirs before merge.

## Blockers and handoff

| Blocker | Concrete next result | Scope owner |
| --- | --- | --- |
| B1: ADR-016 and the candidate not accepted | Nick's answer to M1-1 to M1-9 on #1106; the ADR's status line updated in the landing PR | Nick |
| B2: experiment dependencies unsigned | each line of design §7.2 signed in the M4 and M5 PRs, with the spike allowlists and licence scans | Nick; #1109, #1110 |
| B3: no Mac, Xcode or iPhone to this work | M5 lists what it has; free provisioning or the Program (M1-8) | Nick; #1110 |
| B4: no Android phone model known | M4 names the device and whether it has Play services; a second, de-Googled phone if M3 relies on FCM | #1109 |
| B5: M2 and M3 in flight | the hand-off words, the Hold to talk pattern and the notification tap routing come from `mobile-app-ux.md` and `mobile-notifications.md`; this record's rows name the behaviour, not the words | #1107, #1108 |
| B6: shared-module consumption unsettled | M6 confirms that `web/src/voice/connection.ts`'s `import type` from `../api` compiles outside `web/` (it is already transport-injected) and decides whether `web/src/schema/` is consumed in place or regenerated | #1111 |

This record does not close #1106 (Nick's acceptance is the last criterion), changes no issue's
readiness and applies no runner label.

## Repository verification

This change is three documents: `docs/design/mobile-app.md`, `docs/adr/ADR-016-mobile-app.md`
and this record. No Go, TypeScript or Rust file, no dependency and no schema changed.

`make check` was run twice in the issue worktree on 2026-11-01:

- **Run 1 failed in the Go `test` leg** (`make: *** [Makefile:24: test] Error 1`):
  `TestServerChoice/none` in `internal/cli` printed a `whoami` answer where it wanted "a
  refused connection to the default". The test points `defaultServer` at a port nothing listens
  on at the moment it is chosen (`deadDefault`, `server_choice_test.go` line 20); several other
  `make check` runs were executing on the same host at the time (three test `conchd` processes
  were listening on ephemeral loopback ports), so the port could be taken before the CLI
  subprocess dialled. **Classified as environment**: the test passed on an isolated rerun
  (`go test ./internal/cli -run TestServerChoice -count=1`: `ok 0.024s`) and in run 2; no
  source, test or skip was changed; a documents-only change cannot cause it. It resembles the
  intermittent `cmd/conch` failure recorded in the Linux report and tracked as #886; whether it
  is the same cause is not established here.
- **Run 2 passed in full** (exit 0): gofmt, the invisible-character check, `go vet`,
  `golangci-lint`, `go test ./...`, the schema compatibility check, the generated TypeScript
  check, the Go dependency gate, `deploy-scripts-test` (all rows `ok`, shellcheck and
  `systemd-analyze` included), the Rust leg (fmt, clippy, tests: 70, 129, 1, 3, 1 passed, 0
  ignored, 0 failed; deny) and the web leg (77 files, 2592 tests; and 22 files, 771 tests; all
  passed). Nothing was skipped.

Local checks beside it: every relative link in the three documents resolves to a file on this
branch (the two M2 and M3 documents are named by path and not linked, since they are in flight
under #1107 and #1108), and no invisible or private-use character is present. `voice/target`
(8.7 GB after the Rust leg) was removed from the worktree afterwards.
