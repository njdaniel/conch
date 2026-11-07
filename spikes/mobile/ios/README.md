# Conch iOS spike (#1393 M5-A, #1394 M5-B, #1395 M5-C)

A bare React Native 0.88.1 app (no Expo) for the iOS real-device experiment, under [ADR-016](../../../docs/adr/ADR-016-mobile-app.md) and [the design's section 7.2](../../../docs/design/mobile-app.md). M5-A is the scaffold, M5-B the logic (`src/logic/`, unit tested), and M5-C (this slice) the app: sign-in, the channel list, Join, hold to talk, mute, deafen, open microphone, listen-only, diagnostics, and the native bridge for `AVAudioSession`. The camera is M5-D. It lives outside `make check` and `web/`.

**Status: nothing iOS-native has been built or run.** M5-C was written on Linux, where Swift, Xcode and CocoaPods do not exist: `ConchVoice.swift`, `ConchVoice.m`, the bridging header, the `AppDelegate`, the `project.pbxproj` edits and `Info.plist` are uncompiled. Only the TypeScript is type-checked and unit tested (`npm test`, `./check.sh`). The Xcode project, `Podfile` and `Info.plist` are the React Native 0.88.1 template (`@react-native-community/template@0.88.1`), renamed. `ios/Podfile.lock` and the Pod licence inventory are produced on the Mac by Nick, by hand (decisions of 2026-11-05 on #1110); `./check.sh` reports them "pending the Mac build".

## Pins

React Native 0.88.1, React 19.3.0, Node 24.21.0 (`.nvmrc`, `engines`), Xcode 27.2 on macOS 26.6 or later, iOS 16.4 deployment floor. Every npm dependency is an exact version; `deps-allowlist.txt` and `licences-allowlist.txt` are signed line by line in the PR that adds them. No test dependencies: no jest, eslint or prettier.

Deviations from the RN template: the deployment target is 16.4 (the template follows `min_ios_version_supported`, 15.1), the bundle id is `com.conchspike`, and the display name is "Conch Spike". The Android project, `react-native-safe-area-context` and `@react-native/new-app-screen` are dropped.

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

1. On the Mac, start the dev stack as #1366 describes (conchd and LiveKit reachable on the Mac's LAN address or `.local` name); mint a one-use sign-in link for a test principal. Write the server's address down (it is typed into the app, never stored).
2. `npx react-native start` in `spikes/mobile/ios`, and Run `ConchSpike` on an iPhone Simulator (Debug). The Simulator reaches the Mac's `localhost`.
3. Sign in by link: the server typed in, the code pasted. Expect the channel list to show your channels only.
4. Open a channel, Join. Expect: presence check, then the microphone prompt (at Join, not launch), then connected. If the SDK says "Audio device module is not initialized", `LivekitReactNative.setup()` did not run first.
5. Hold to talk for 3 to 8 s. In the server's presence (`GET /v1/channels/{c}/voice/presence`) `transmitting` is true while held and false after; the audit shows `voice_transmit_started` and `_stopped` and no `_unreported`.
6. Press the Simulator's Home (Device > Home) then return, and Device > Lock then unlock, while in voice. Expect the Diagnostics screen to show `app inactive` / `app background` and the gate held shut; after returning nothing transmits until a new press.
7. Optionally simulate an interruption (Siri, or `xcrun simctl` where it supports it) and a route change (Simulator I/O > Audio output); the Diagnostics "delivered" list shows exactly what iOS sent.
8. Leave: presence empties within 5 s. Sign out.
9. Fill the Simulator column of `docs/reviews/mobile-ios-voice.md`, labelled "Simulator". A Simulator pass is never device evidence, and has no real audio route.

### Device run (iPhone, free provisioning, as Mac step 6)

Same stack with the Mac's LAN address (the phone and the Mac on one Wi-Fi; accept the Local Network prompt). Build Debug with Metro on the Mac (or Release to embed the bundle). Then the rows of `docs/reviews/mobile-ios-voice.md` for M5-C: M-F1 to M-F7, M-L1 to M-L9 (lock, switch away, 5 min silence; background audio keeps the room only if `UIBackgroundModes` `audio` is honoured), M-B1 to M-B3, M-I1 to M-I3 (an incoming call while in voice and while a press is held; Siri; an alarm; microphone revoked in Settings), M-N1 to M-N3, M-K1 to M-K3, M-R1, M-R4, M-S1 to M-S6, M-P1, M-P2, M-A1, M-A2. Record the model, iOS build, Xcode and macOS versions, and the Diagnostics "delivered" list for each interruption row. Camera rows M-C1 to M-C10 are M5-D.

## Checks

`./check.sh` (Linux or Mac) runs `scripts/web-depgate.sh` and `scripts/web-licences.sh` against this directory's manifest, lockfile and allowlists, then `npm ci --ignore-scripts` and `tsc --noEmit`. Then, if `ios/Podfile.lock` is not committed, it says "Pods PENDING the Mac build" and exits 0 for the npm half; once it is, it checks the lockfile has checksums and no ranges, and that `licences.md` has no placeholder or unread licence.

## Licences

`licences.md` is generated: `node gen-licences.mjs > licences.md`. The npm part is complete; the Pod part is a placeholder until step 4.
