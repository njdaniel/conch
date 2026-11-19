# Conch Android spike (#1109, M4-A)

A bare React Native 0.88.1 app (no Expo) for the Android real-device experiment, under [ADR-016](../../../docs/adr/ADR-016-mobile-app.md) and [the design's section 7.2](../../../docs/design/mobile-app.md). M4-A is the scaffold, M4-B the logic (`src/logic`), M4-C the voice wiring (`src/app`, the Kotlin in `android/app/src/main/java/com/conchspike`): sign-in, channel list, Join/Leave, hold to talk, the foreground service, the lifecycle bridge and the diagnostics screen. The camera is M4-D. It lives outside `make check` and `web/`.

## Pins

React Native 0.88.1, React 19.3.0, Node 24.21.0 (`.nvmrc`, `engines`), JDK 17 (Temurin 17.0.21+9), Android SDK Platform 35, Build-Tools 36.0.0, NDK 27.1.12297006, Gradle 9.4.1 (wrapper, with its checksum). Every npm dependency is an exact version; `deps-allowlist.txt` and `licences-allowlist.txt` are signed line by line in the PR that adds them. No test dependencies: no jest, eslint or prettier.

Deviation from the RN template: it names compileSdk 37, Build-Tools 37.0.0 and targetSdk 36. Here they are 35, 36.0.0 and 35, per the pins. It builds as is. The iOS project, `react-native-safe-area-context` and `@react-native/new-app-screen` are dropped.

## Toolchain (no root, all under `~/.cache/conch`)

    CONCH_ACCEPT_ANDROID_LICENCES=1 ./toolchain.sh   # accepts Google's SDK licence for that SDK only
    source ~/.cache/conch/android-env.sh
    nvm use                                          # 24.21.0
    npm ci
    (cd android && ./gradlew assembleDebug)

Roughly 13 GB for the SDK, system image, Gradle cache and AVD, plus about 3 GB of build output in `android/app/build`.

## Standalone APK for the device run (#1920)

The debug build loads its JavaScript from Metro, so it needs the computer. For the unplugged M4 rows (lock, background, interruptions) build the release variant: it bundles the JS (`assets/index.android.bundle`), is signed with the debug keystore (`android/app/debug.keystore`), and needs no Metro or USB. No Gradle change is needed: the release build type already does this. It refuses cleartext (`usesCleartextTraffic=false`), so the server must be `https://`.

    source ~/.cache/conch/android-env.sh
    nvm use && npm ci
    (cd android && ./gradlew assembleRelease)
    # -> android/app/build/outputs/apk/release/app-release.apk (about 107 MiB)
    adb install -r android/app/build/outputs/apk/release/app-release.apk   # or copy the file to the phone and open it

Open the app and, at sign-in, type `https://alpha.gankbears.com` as the server, then the name and password, or paste a one-use sign-in link (`conch admin principal sign-in-link <id>`). Check the bundle with `unzip -l app-release.apk | grep index.android.bundle`. An emulator start with Metro stopped is not device evidence.

## Checks

`./check.sh` runs `scripts/web-depgate.sh` and `scripts/web-licences.sh` against this directory's manifest, lockfile and allowlists, then `npm ci` and `tsc --noEmit`, then checks the Gradle lockfiles for exact versions.

## Lockfiles

`package-lock.json`; `android/app/gradle.lockfile` (all configurations, release and debug) and `android/buildscript-gradle.lockfile` (AGP, Kotlin, the RN plugin). After changing a Gradle dependency: `./gradlew assembleDebug assembleRelease --write-locks`. The React Native Gradle plugin's own builds (included from `node_modules`) are locked by npm, not Gradle.

## Licences

`licences.md` is generated: `node gen-licences.mjs > licences.md` after a build.

## Emulator

    avdmanager create avd -n conch35 -k "system-images;android-35;google_apis;x86_64" -d pixel_6
    emulator -avd conch35 -no-window -gpu swiftshader_indirect &
    npx react-native start & adb reverse tcp:8081 tcp:8081
    adb install -r android/app/build/outputs/apk/debug/app-debug.apk
    adb shell am start -n com.conchspike/.MainActivity

An emulator run is never device evidence.

## Dry run against the dev stack (M4-C)

    scripts/dev-stack.sh up                         # note the ports it prints
    conch admin principal create --kind human sam; conch admin member add general <id>
    conch admin principal sign-in-link <id>         # one use; paste into the app
    adb reverse tcp:8081 tcp:8081                   # Metro
    adb reverse tcp:<conchd> tcp:<conchd>; adb reverse tcp:<livekit> tcp:<livekit>; adb reverse tcp:<livekit+1> tcp:<livekit+1>

### Away with the camera on (#1972)

    REPRO_PRINCIPAL=<id> ./repro-background-camera.sh 5

With the app signed in on the channel's Voice view and someone else in the room (a second person matters: without one the race below was not seen), each run joins if needed, turns the camera on, presses HOME for 40 s, returns, and watches for 45 s, sampling the person's LiveKit participant SIDs every 3 s and reading LiveKit's log (`docker logs`) for when each connection started and why each ended. The race it checks: on return the SDK restarts its connection, a leave from the server lands mid-restart and the Room reports a disconnect, the app rejoins, and the SDK's restart (which closing does not stop in livekit-client 2.22.3) used to rejoin too and displace it. A run passes only if it saw that set up (the connection from before HOME ended and at least two new ones started; otherwise it says NO-RACE and fails), no sample had two connections at once, no connection started after the one the phone settled on, that one did not end, and the settled samples agree with `conch voice status` and the app's "In voice". On exit, an interrupt included, it turns off the camera and leaves voice if it turned them on. A dropped room is now sealed against rejoining and settles (`src/logic/dropped.ts`, `src/app/engine.ts`) before the next connection is opened.

The debug build allows cleartext, so it sends the password (and a sign-in link's code) over a typed `http://` origin: for the development profile only. The release build refuses cleartext. The app takes the server address typed in at sign-in; the bearer is held in memory only. The SDK needs `LiveKitReactNative.setup` in `MainApplication.onCreate` (native). Audio focus is owned by `ConchVoiceModule.kt`, and the SDK is told not to manage it, so a focus loss reaches the lifecycle.
