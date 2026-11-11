# Conch iOS spike (#1393, M5-A)

A bare React Native 0.88.1 app (no Expo) for the iOS real-device experiment, under [ADR-016](../../../docs/adr/ADR-016-mobile-app.md) and [the design's section 7.2](../../../docs/design/mobile-app.md). This slice is an empty app that builds. The app's logic is M5-B to M5-D. It lives outside `make check` and `web/`.

**Status: nothing iOS-native has been built or run.** This slice was written on Linux. The Xcode project, `Podfile` and `Info.plist` are the React Native 0.88.1 template (`@react-native-community/template@0.88.1`), renamed. `ios/Podfile.lock` and the Pod licence inventory are produced on the Mac by Nick, by hand (decisions of 2026-11-05 on #1110); `./check.sh` reports them "pending the Mac build".

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

## Checks

`./check.sh` (Linux or Mac) runs `scripts/web-depgate.sh` and `scripts/web-licences.sh` against this directory's manifest, lockfile and allowlists, then `npm ci --ignore-scripts` and `tsc --noEmit`. Then, if `ios/Podfile.lock` is not committed, it says "Pods PENDING the Mac build" and exits 0 for the npm half; once it is, it checks the lockfile has checksums and no ranges, and that `licences.md` has no placeholder or unread licence.

## Licences

`licences.md` is generated: `node gen-licences.mjs > licences.md`. The npm part is complete; the Pod part is a placeholder until step 4.
