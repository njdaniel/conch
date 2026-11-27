# Conch component versioning audit

**Date** 2026-11-23 · **Issue** #2147 · **Audited main** `c3a639fc15b0e0471c479d929a92c423ff8870c5`

Conch already versions its wire contracts independently. The desktop's
pre-sign-in gate still compares the server's **build tag** with a packaged
allowlist. A compatible server patch can therefore require desktop repackaging
even when every required wire contract is unchanged. Preserve the existing
gate while collecting cross-version evidence. Keep alpha.22 on the current
coordinated release procedure. [#946](https://github.com/njdaniel/conch/issues/946),
[#1596](https://github.com/njdaniel/conch/issues/1596),
[release procedure](../../.claude/skills/release/SKILL.md)

## Artifact inventory

| Component | Version source and distribution on audited main |
| --- | --- |
| Server and CLI/TUI | A `v*` tag becomes `main.version` in both Go binaries. The release builds `conchd` and `conch` for Linux amd64, Linux arm64 and macOS arm64. The Makefile stamps local builds from `git describe`; unstamped binaries use Go build info. Server `version`, `/healthz` and authenticated `whoami.server_version` report the server build. CLI `version` reports the CLI build. [Release workflow](../../.github/workflows/release.yml), [Makefile](../../Makefile), [build info](../../internal/buildinfo/buildinfo.go), [health](../../pkg/schema/health.go), [whoami](../../pkg/schema/whoami.go) |
| Browser and PWA | Tagged `conchd` embeds the browser bundle at `/app/`. A self-hoster can serve the Vite build separately with `--web-dir`. The browser package's `0.0.0` is a source placeholder. The web client has no independent release artifact in the workflow. HTML uses `no-store`, hashed assets use immutable caching, and other static assets use `no-cache`. The existing service worker handles push and does not intercept fetches. Installation metadata is tracked in #1970. [Server static handler](../../internal/server/static.go), [web package](../../web/package.json), [push worker](../../web/public/sw.js), [#1970](https://github.com/njdaniel/conch/issues/1970) |
| Electron desktop | Linux amd64 ships a verified `.deb`. Audited main adds a tag-driven Windows x64 **unsigned preview** NSIS installer. The checked-in desktop package says `0.0.0`. Release builds stamp the tag minus `v` in the packaged app and `resources/version`; the Linux package also uses Debian's prerelease spelling, such as `0.1.0~alpha.21`. Development builds use `0.0.0-dev+<commit>`. macOS arm64's unsigned DMG is a CI tester artifact. [Release workflow](../../.github/workflows/release.yml), [CI workflow](../../.github/workflows/ci.yml), [desktop version script](../../scripts/desktop-version.sh) |
| Native helper | `voice/Cargo.toml` sets the Rust workspace version, currently `0.1.0-alpha.21`. The Linux and Windows release paths verify the bundled helper's reported version matches the desktop release. The helper is shipped inside those desktop packages. The release workflow has no standalone helper asset. Desktop upgrades are manual. The installed version stamp detects an on-disk replacement and gates starting the new helper from an old running app pending restart. [Cargo workspace](../../voice/Cargo.toml), [release guard](../../scripts/desktop-release-guard.sh), [installed version handling](../../desktop/main/installed.ts), [#774](https://github.com/njdaniel/conch/issues/774), [#1377](https://github.com/njdaniel/conch/issues/1377) |
| Native mobile | Separate React Native spikes. The Android project uses `versionName 1.0` and `versionCode 1`; CI produces a debug-signed, hand-installed lab APK. iOS uses marketing version `1.0` and build `1`; CI builds an unsigned simulator app and uploads launch evidence. Physical installation has a separate provisioning workflow. Store distribution requires its own owner decision. [Android build](../../spikes/mobile/android/android/app/build.gradle), [Android runbook](../../spikes/mobile/android/README.md), [iOS project](../../spikes/mobile/ios/ios/ConchSpike.xcodeproj/project.pbxproj), [iOS runbook](../../spikes/mobile/ios/README.md), [ADR-016](../adr/ADR-016-mobile-app.md) |
| Hosted server and Conch Cloud | The tracked self-hosted runbook installs the tagged Go binary under `/opt/conch/releases` and uses systemd. The Docker image in the runbook is digest-pinned LiveKit. The audited repo has no conchd OCI publication, Conch Cloud build pipeline or Cloud version source. Cloud is a separate future owner decision under this issue. [Deploy runbook](../deploy.md), [deployment unit](../../ops/deploy/livekit.service), [ADR-000](../adr/ADR-000-charter.md), [#2147](https://github.com/njdaniel/conch/issues/2147) |

The checkout's newest alpha tag is `v0.1.0-alpha.21` at `07fee44b`.
Its workflow predates the Windows release job. On audited main, publication
requires the matching Go files, Linux package and Windows installer for the
tag. The workflow collects the exact artifact set, writes one checksum file
and attests the files. The publish job verifies the attestations for the
workflow and tag. A plain tag yields a draft; a prerelease tag publishes a
prerelease. CI preview artifacts use a separate path. [alpha.21 workflow](https://github.com/njdaniel/conch/blob/v0.1.0-alpha.21/.github/workflows/release.yml),
[current release workflow](../../.github/workflows/release.yml)

## Where compatibility is checked

| Boundary | Current behavior |
| --- | --- |
| Desktop sign-in | Main reads unauthenticated `/healthz` before offering sign-in or getting a saved credential. It requires a successful response and an exact match in `compat.json`; an unreachable server and an answer with an unsupported version are separate states. A release lists its own tag plus `compat-extra.json`, which is empty on main. The development list on main ends at alpha.21. Only an unpackaged build honors `--allow-unlisted-server`. An incompatible verdict leaves stored credentials in place. [Health probe](../../desktop/main/profiles/activation.ts), [profile service](../../desktop/main/profiles/service.ts), [compatibility policy](../../desktop/main/compat.ts), [release manifest](../../desktop/build/manifest.ts), [development list](../../desktop/build/compat.json), [release extras](../../desktop/build/compat-extra.json) |
| REST and WebSocket | Routes and message families carry their own versions. The server still registers `/v0/ws`, `/v1/ws`, `/v2/ws` and separate events feeds; clients select a route. Voice presence offers v1 by default and v2 on explicit request, with unknown or repeated query values refused. Desktop and browser clients request v2 on their voice feeds. Desktop validates required REST response families after sign-in and reports unexpected shapes as incompatibilities. Current health reports runtime status and build identity. It advertises no supported client protocol range. Published schema fixtures are immutable, with newer versions added through schema review. [REST and WS routes](../../internal/server/routes.go), [presence version parser](../../internal/server/voice_presence.go), [operation and response map](../../web/src/platform/operations.ts), [desktop feed](../../desktop/main/api/feeds.ts), [schema compatibility gate](../../scripts/schema-compat.sh) |
| Owner to voice helper | The trusted executable starts with the frozen v1 hello containing the helper build version and its supported control versions. The owner chooses the best common version before starting the session. Desktop prefers v2 then v1 by default; the Linux share-sound opt-in offers v3 first. The TUI supports v2 then v1. The managed helper implements v1–v3 at this revision. V4 has schema conformance fixtures and awaits runtime wiring. For the network session, the helper separately requests versioned REST documents from conchd. [Desktop owner](../../desktop/main/voice/protocol.ts), [TUI control reader](../../internal/cli/voicehelper/protocol.go), [managed helper](../../voice/conch-voice/src/managed.rs), [helper REST client](../../voice/crates/conch-voice-api/src/client.rs), [v4 fixtures](../../pkg/schema/testdata/voice-control-v4) |
| Server upgrades and rollback | The deployment script verifies release checksums, takes and verifies each instance's backup, then repoints the shared binary, restarts the instances and checks every instance's reported version. A prior binary refuses a database with a newer migration number. A rollback across a migration restores the pre-upgrade snapshot with credentials invalidated by the restore procedure. The script keeps the displaced data directory for recovery, and post-upgrade writes are absent from the restored service. A rollback without a migration keeps the data in place. [Upgrade and rollback](../deploy.md#11-upgrading-and-rolling-back), [upgrade script](../../ops/deploy/conch-upgrade.sh), [store migrations](../../internal/server/store/store.go) |

The current desktop smoke test exercises exact allow/refuse behavior using a
disposable conchd with two version stamps. That covers the preflight gate.
It leaves cross-release contract compatibility to be measured using the
actual old and new artifacts. [Packaged desktop smoke test](../../desktop/tests/smoke/compat.spec.ts)

## Proposed migration in reviewable steps

1. **Pin a baseline compatibility matrix.** Add a CI job using the prior
   supported release artifacts, verified against the release workflow and tag,
   and current candidate builds. Publish build identities, tested contract
   versions and results as machine-readable test evidence. Distinguish tags
   stated by the binary from versions the matrix actually tested. Keep the
   package gate and coordinated release behavior unchanged in this first PR.
2. **Define a versioned discovery contract.** Have conchd advertise supported
   core protocol generations and optional capabilities on the configured
   server origin. Keep `/healthz` operational and its existing contract
   intact. Core generations map to required REST and socket families.
   Separate implemented capabilities from current permissions or optional
   backend readiness. Advertise a minimum supported client protocol generation
   and allow a prior generation where the published matrix proves it.
   Review the document's `pkg/schema` shape, bounds and rejection behavior in
   its own PR. Existing endpoints and fixtures continue to serve older clients.
3. **Run discovery alongside the exact gate.** Compare verdicts on candidate
   builds while the old gate still controls admission. Then enable negotiated
   admission in a separately reviewed PR after the matrix passes. A valid
   offer with a common core generation means compatible. A valid offer without
   one means update required. A compatible client can show update recommended
   only from trusted distribution metadata. Use the verified exact legacy
   list for a known legacy server with an absent discovery endpoint. Treat a
   malformed offer, a failed trust check, an outage and a valid incompatible
   offer as distinct outcomes. None permits login via legacy fallback.
   Preserve the existing credential and same-origin checks, with revalidation
   on a new connection or profile switch. A server-provided offer cannot
   nominate a package download or weaken authorization.
4. **Separate packaging once negotiation is proven.** Propose component
   release refs such as `conchd/vX`, `conch-cli/vX` and
   `conch-desktop/vX`. Keep the bundled browser tied to its server package
   initially, with an asset build digest and server identity recorded
   separately. Record the desktop build, helper build and tested control
   versions in package metadata. Retain existing helper version checks until
   an approved explicit pairing rule replaces tag equality. Give each
   component its own artifact set, checksums and verified provenance, with a
   coordinated `v*` series available as an index of already verified
   artifacts. Leave the existing `v*` pipeline available during transition.
   Start mobile and Cloud release refs only after their distribution and
   operating models are approved.
5. **Exercise upgrades before changing defaults.** Release the compatible
   desktop ahead of the first independent server update so current users can
   move while their old server stays supported. Canary server upgrades with
   a verified backup and both client generations, then test rollback after
   a compatible change and after a migration on disposable data. Mixed-client
   support across a restart is within scope. Running mixed server versions
   against the same SQLite database is a separate architecture decision.
   For browser and installed PWA, verify a fresh load gets the new build,
   a stale tab reconnects or offers reload, and the previous build loads
   after rollback. Keep private API data and credentials out of offline
   caches. Coordinate the PWA check with #1970.

The first change can measure compatibility without publishing anything.
Principal review covers the compatibility contract and follow-up issues.
The change to credential admission needs security review. Nick signs off
on ADR changes, schema version bumps, roadmap changes and releases. Keep
approval-path label, end-to-end and merge requirements when a later change
touches that path. [ADR-000](../adr/ADR-000-charter.md#governance-who-decides-what),
[schema-change procedure](../../.claude/skills/schema-change/SKILL.md)

### Compatibility matrix for CI

Start with the matching release, current mixed-version behavior and existing
upgrade simulations. Add negotiation and independent packaging assertions in
the corresponding implementation PRs.

| Pair or condition | Proof to record |
| --- | --- |
| Prior released desktop, CLI and browser with their matching server | Sign in, list and send channel messages, reconnect and backfill. Exercise the release's REST and socket families. Capture which packaged desktop policy allowed access. |
| Prior negotiator with candidate server, and candidate negotiator with prior server | Select their common core generation. Run the same smoke with real versioned artifacts. Exercise downgrade to prior voice presence and legacy admission from the proven exact list where applicable. Run an existing non-negotiating desktop against an unlisted tag to record the current refusal. |
| Candidate desktop with a compatible server-only patch, and with an incompatible generation | Compatible remains usable without repackaging. Incompatible reports the direction of the required update before touching stored credentials. Malformed or spoofed offers, unverified artifacts and unreachable servers never enable admission. |
| Candidate server with voice disabled or LiveKit unavailable | Messaging, approvals and audit remain available. Supported voice contracts and current voice readiness are distinguishable. |
| Desktop with packaged helper, supported older control peer and no common version | Verify versions, provenance and the packaged pairing before start. Test hello selection or fail-closed refusal. Text remains usable when the voice pairing fails. |
| Server restart, rollback and stale browser assets | Verify client reconnect, retained writes on an unchanged database schema, restored data and ended credentials after a migration rollback, and fresh client assets without private caching. Report simulator and physical device checks separately. |

This report records the repository audit. The proposed negotiated behavior
and cross-release matrix are follow-up implementation and test work.
