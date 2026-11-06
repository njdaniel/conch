# Conch Android spike (#1109, M4-A)

A bare React Native 0.88.1 app (no Expo) for the Android real-device experiment, under [ADR-016](../../../docs/adr/ADR-016-mobile-app.md) and [the design's section 7.2](../../../docs/design/mobile-app.md). This slice is an empty app that builds. The app's logic is M4-B and M4-C. It lives outside `make check` and `web/`.

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
