# Conch iOS spike (#1393 M5-A, #1394 M5-B, #1395 M5-C)

A bare React Native 0.88.1 app (no Expo) for the iOS real-device experiment, under [ADR-016](../../../docs/adr/ADR-016-mobile-app.md) and [the design's section 7.2](../../../docs/design/mobile-app.md). M5-A is the scaffold, M5-B the logic (`src/logic/`, unit tested), and M5-C (this slice) the app: sign-in, the channel list, Join, hold to talk, mute, deafen, open microphone, listen-only, diagnostics, and the native bridge for `AVAudioSession`. The camera is M5-D. It lives outside `make check` and `web/`.

**Status: nothing iOS-native has been built or run.** M5-C was written on Linux, where Swift, Xcode and CocoaPods do not exist: `ConchVoice.swift`, `ConchVoice.m`, the bridging header, the `AppDelegate`, the `project.pbxproj` edits and `Info.plist` are uncompiled. Only the TypeScript is type-checked and unit tested (`npm test`, `./check.sh`). The Xcode project, `Podfile` and `Info.plist` are the React Native 0.88.1 template (`@react-native-community/template@0.88.1`), renamed. `ios/Podfile.lock` and the Pod licence inventory are produced on the Mac by Nick, by hand (decisions of 2026-11-05 on #1110); `./check.sh` reports them "pending the Mac build".

## Pins

React Native 0.88.1, React 19.3.0, Node 24.21.0 (`.nvmrc`, `engines`), Xcode 27.2 on macOS 26.6 or later, iOS 16.4 deployment floor. Every npm dependency is an exact version; `deps-allowlist.txt` and `licences-allowlist.txt` are signed line by line in the PR that adds them. No test dependencies: no jest, eslint or prettier.

Deviations from the RN template: the deployment target is 16.4 (the template follows `min_ios_version_supported`, 15.1), the bundle id is `com.conchspike`, and the display name is "Conch Spike". The template's empty `NSLocationWhenInUseUsageDescription` is dropped: nothing here reads the location, and the spike declares only the permissions and capabilities its matrix needs (the iOS counterpart of trimming the Android manifest). The Android project, `react-native-safe-area-context` and `@react-native/new-app-screen` are dropped.

The `Gemfile` is kept because `pod install` is run through it (`bundle exec pod install`, the template's way of pinning CocoaPods). Its gem requirements are ranges, so the Mac run produces `Gemfile.lock` with the exact versions, and that is committed too.

## Mac steps (Nick, by hand)

Prerequisites: Xcode 27.2 (`xcodebuild -version`), its command-line tools (`xcode-select --install`), Ruby with Bundler, Node 24.21.0 via `nvm`, an iPhone on iOS 16.4 or later, a USB cable, and an Apple ID signed in under Xcode > Settings > Accounts.

1. `cd spikes/mobile/ios && nvm use && npm ci`
2. `bundle config set path vendor/bundle && bundle install`  (writes `Gemfile.lock`)
3. `cd ios && bundle exec pod install`  (writes `ios/Podfile.lock` and `ios/Pods/`). If `pod install` complains about the CocoaPods trunk or a version, stop and record the message in the PR; do not loosen a pin.
4. Regenerate the Pod section: from `spikes/mobile/ios`, `node gen-licences.mjs > licences.md`. Any Pod row that says `NOT READ` needs its licence filled in from the pod's repository before signing (`check.sh` fails on it).
5. Commit `ios/Podfile.lock`, `Gemfile.lock` and `licences.md`, then run `./check.sh` from this directory: it must end `Pods OK`.
6. Build and run on the iPhone with free provisioning (a personal team, 7-day profile):
   - Open `ios/ConchSpike.xcworkspace` (the workspace, not the `.xcodeproj`).
   - Select the `ConchSpike` target > Signing & Capabilities > tick "Automatically manage signing" > Team: your "Personal Team". If Xcode says the bundle id is taken, change `com.conchspike` to something unique to you for the run and do not commit that change.
   - Connect the iPhone, unlock it, trust the computer. On iOS 16 and later enable Settings > Privacy & Security > Developer Mode and restart when asked.
   - Pick the iPhone as the run destination, Debug configuration, and press Run. The first launch shows "Untrusted Developer": Settings > General > VPN & Device Management > your Apple ID > Trust.
   - In another terminal run `npx react-native start`; the phone must reach the Mac's port 8081 (same Wi-Fi). Alternatively build the Release configuration, which embeds the JS bundle.
   - The expected result is the text "Conch spike". The profile expires after 7 days; re-run from Xcode to re-sign.
7. Record the Xcode, macOS and iOS versions you actually used in the PR. A Simulator run, if you do one, is labelled as such and is never device evidence.

## M5-C: the voice slice

Layout: `src/app/` is the app (`client.ts` the API client holding the bearer in memory, `controller.ts` the voice controller over ports, `livekit.ts` the SDK adapter, `bridge.ts` and `services.ts` the lifecycle bridge, `ui/` the screens). The Swift is in `ios/ConchSpike/`:

| File | Does |
| --- | --- |
| `AppDelegate.swift` | calls `LivekitReactNative.setup()` first in `didFinishLaunching` (without it the SDK fails with "Audio device module is not initialized", as on Android) |
| `ConchVoice.swift` | the `ConchVoice` module: microphone permission (asked at Join), `AVAudioSession` interruption and route-change observers, the output port types; emits `ConchVoiceEvent` |
| `ConchVoice.m` | the `RCT_EXTERN_MODULE` registration of the above |
| `ConchSpike-Bridging-Header.h` | React's module headers for the Swift file (set as `SWIFT_OBJC_BRIDGING_HEADER` in the project) |
| `Info.plist` | `UIBackgroundModes` `audio`, `NSMicrophoneUsageDescription`, `NSLocalNetworkUsageDescription`, ATS `NSAllowsLocalNetworking` |

The module only observes the session; `@livekit/react-native` owns its category and activation. Events go `ConchVoice.swift` -> `native.ts` -> `bridge.ts` -> `VoiceController.interruption / routeChange` -> `logic/ios.ts` `IosAudioEvents` -> `Lifecycle` and `TransmitGate`. AppState `inactive` and `background` are lifecycle reasons of their own. The "should resume" option is recorded in `controller.audio.delivered` and read by no decision.

### Build this slice on the Mac

Do README "Mac steps" 1 to 5 first (`npm ci`, `bundle install`, `pod install`, regenerate `licences.md`, `./check.sh` ending `Pods OK`). `pod install` now also pulls `livekit-react-native` and `livekit-react-native-webrtc` and the WebRTC pod (`LiveKitWebRTC`): their versions and licences are what `licences.md` step 4 inventories; any `NOT READ` row is filled by hand before signing. Commit `Podfile.lock`, `Gemfile.lock` and `licences.md`.

Then, from `spikes/mobile/ios/ios`, open `ConchSpike.xcworkspace` and build the `ConchSpike` scheme for an iPhone Simulator (Cmd-B). First build is the first time any Swift here is compiled. Expected trouble spots, in order of likelihood: (1) `import livekit_react_native` not found in `AppDelegate.swift` (module name or `DEFINES_MODULE`: look at the generated module map under `Pods/Headers`); (2) the bridging header or `RCT_EXTERN_MODULE` not seen (check Build Settings > Objective-C Bridging Header is `ConchSpike/ConchSpike-Bridging-Header.h`, and that `ConchVoice.m` is in Compile Sources); (3) Swift 6 strict-concurrency diagnostics in `ConchVoice.swift` (the project is Swift 5 mode; do not upgrade it); (4) an `AVAudioApplication` or `AVAudioSession` API name differing in the SDK. Fix by the smallest edit and record it in the PR.

### Simulator dry run (needs #1366: the dev stack in LAN mode)

1. Start the dev stack on the Linux dev machine with `CONCH_DEV_LAN=<its LAN address> scripts/dev-stack.sh up` (#1366: conchd and LiveKit listen on that address, plain HTTP); mint a one-use sign-in link for a test principal (`conch admin principal sign-in-link <id>`). The server's address is typed into the app as `http://<address>:<conchd port>` and never stored; a pasted link made for a different address is refused.
2. `npx react-native start` in `spikes/mobile/ios`, and Run `ConchSpike` on an iPhone Simulator (Debug). The Simulator reaches the Linux machine by its LAN address, like the phone (it shares the Mac's network); the Mac must be on the same network.
3. Sign in by link: the server typed in, the code pasted. Expect the channel list to show your channels only.
4. Open a channel, Join. Expect: presence check, then the microphone prompt (at Join, not launch), then connected. If the SDK says "Audio device module is not initialized", `LivekitReactNative.setup()` did not run first.
5. Hold to talk for 3 to 8 s. In the server's presence (`GET /v1/channels/{c}/voice/presence`) `transmitting` is true while held and false after; the audit shows `voice_transmit_started` and `_stopped` and no `_unreported`.
6. Press the Simulator's Home (Device > Home) then return, and Device > Lock then unlock, while in voice. Expect the Diagnostics screen to show `app inactive` / `app background` and the gate held shut; after returning nothing transmits until a new press.
7. Optionally simulate an interruption (Siri, or `xcrun simctl` where it supports it) and a route change (Simulator I/O > Audio output); the Diagnostics "delivered" list shows exactly what iOS sent.
8. Leave: presence empties within 5 s. Sign out.
9. Fill the Simulator column of `docs/reviews/mobile-ios-voice.md`, labelled "Simulator". A Simulator pass is never device evidence, and has no real audio route.

### Device run (iPhone, free provisioning, as Mac step 6)

Same stack, with the Linux machine's LAN address (the phone, the Mac and the Linux machine on one Wi-Fi; accept the Local Network prompt). Build Debug with Metro on the Mac (or Release to embed the bundle). Then the rows of `docs/reviews/mobile-ios-voice.md` for M5-C: M-F1 to M-F7, M-L1 to M-L9 (lock, switch away, 5 min silence; background audio keeps the room only if `UIBackgroundModes` `audio` is honoured), M-B1 to M-B3, M-I1 to M-I3 (an incoming call while in voice and while a press is held; Siri; an alarm; microphone revoked in Settings), M-N1 to M-N3, M-K1 to M-K3, M-R1, M-R4, M-S1 to M-S6, M-P1, M-P2, M-A1, M-A2. Record the model, iOS build, Xcode and macOS versions, and the Diagnostics "delivered" list for each interruption row. Camera rows M-C1 to M-C10 are M5-D.

### Review fixes ported from the Android spike

The Android spike (#1109) was reviewed after this slice was first written, and the iOS code mirrors its logic and wiring, so each fix there was checked here. What each one became:

| Android fix | iOS |
| --- | --- |
| The open microphone fails shut: the gate's 1.5 s sample watchdog, and the joined channel's Voice view closing shuts the gate and arms the resume latch (cc5fab02) | Ported: `logic/gate.ts` (`sampleWatchdog`, `shutAndArm`, M5-B) and `VoiceController.watch` (M5-C); tests in `test/gate.test.ts` and `test/controller.test.ts` |
| Audio focus latch: a permanent loss or a denied request holds the block, a grant after Leave must not find a stale loss (858b0bb5) | Different: iOS has no focus. The same hazard is an interruption that *began* and whose *end* never arrives (the observers stop at Leave, or the app was suspended). Leave and tear-down now clear it, so the next join is not shut for good; on return to the foreground with one still pending the gate stays shut and the app says so, because there is no call to ask the session. Test: `an interruption that never ended does not outlive Leave`. Device rows M-I1 and M-I2 record whether iOS delivers the end |
| Route change under an open gate arms the resume latch (858b0bb5) | Already true by construction: `IosAudioEvents.routeChange` blocks and unblocks the gate, which arms the latch. A controller-level test now pins it for the open microphone |
| Swipe-away ends voice (`onTaskRemoved`) | Not applicable: iOS tells the app nothing when it is force-quit. Voice ends with the process, and conchd's own unreported-transmit handling is the backstop (row M-K1) |
| Presence follows the joined channel, not the one on screen; Leave starts it for the channel on screen again (858b0bb5) | Ported: `controller.ts` `watch` and `leave`, `ui/Voice.tsx` |
| Client typing: request bodies are the generated `*RequestV1` types (858b0bb5) | Ported: `client.ts` |
| A pasted sign-in link must name the typed server; a malformed escape is a refusal; changing the server forgets the bearer (858b0bb5) | Ported: `signInTarget`, `setServer`, the two words in `ui/SignIn.tsx`; tests in `test/client.test.ts` |
| A sign-in whose `whoami` fails signs out, and the password and link fields are cleared after every attempt (cc5fab02) | Ported: `ui/SignIn.tsx` |
| The LiveKit host and `host:port` are held by the diagnostics; compressed IPv6 and more host endings are redacted (cc5fab02) | Ported: `logic/diagnostics.ts`, `controller.ts` and `services.ts` (`hold`); tests in `test/redact.test.ts` and `test/controller.test.ts` |
| The blocked-camera note names Android settings (9515c1f5) | Ported with iOS words ("iOS Settings"): `logic/camera.ts`, test in `test/camera.test.ts` |
| The manifest drops `SYSTEM_ALERT_WINDOW` and the `mediaProjection` service; the receipt quotes the merged manifest (cc5fab02) | Counterpart: nothing is declared that the matrix does not need. The template's empty `NSLocationWhenInUseUsageDescription` is gone (M5-A); the Info.plist holds `UIBackgroundModes` `audio`, the microphone and local-network descriptions, and ATS local networking; no entitlements, no push, no `voip`. The Mac run should quote the *built* app's Info.plist and entitlements, as the Android receipt quotes the merged manifest |
| Cleartext only in the debug build | Different: one Info.plist. `NSAllowsLocalNetworking` exempts local addresses (an IP literal, a `.local` name) and never a public host, which is narrower than Android's debug-wide cleartext. A release build would drop it (M6) |
| `allowBackup="false"` | Not applicable: the spike writes nothing to disk (the bearer is in memory), so there is nothing for a backup to carry |
| Hardware Back, the lock broadcast and the `isLocked` check on return, the foreground service and its `stopSelf` finding | Not applicable: no hardware back on iOS; locking is AppState `inactive` then `background`, and `active` lifts them; there is no foreground service |

Findings for M6 that carry over (recorded, not fixed here): React Native's JavaScript timers pause while the app is suspended, so the report retry, presence poll and the camera logic's bounded waits may wait for the app's return (M-L2 and M-L5 should watch for it, and M5-D's camera does not wait for an unpublish for this reason); `livekit_url` from the session grant is used as given, and a production app must require `wss:`; tiles name people "Person N" because presence carries ids only.

## Checks

`./check.sh` (Linux or Mac) runs `scripts/web-depgate.sh` and `scripts/web-licences.sh` against this directory's manifest, lockfile and allowlists, then `npm ci --ignore-scripts` and `tsc --noEmit`. Then, if `ios/Podfile.lock` is not committed, it says "Pods PENDING the Mac build" and exits 0 for the npm half; once it is, it checks the lockfile has checksums and no ranges, and that `licences.md` has no placeholder or unread licence.

## Licences

`licences.md` is generated: `node gen-licences.mjs > licences.md`. The npm part is complete; the Pod part is a placeholder until step 4.
