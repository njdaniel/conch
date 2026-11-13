# desktop/: the Conch desktop shell

An Electron app that opens the existing web client from packaged files at
`conch-app://ui/`. Installing the app as a user: [docs/desktop/linux.md](../docs/desktop/linux.md). Design: [docs/design/linux-desktop.md](../docs/design/linux-desktop.md)
§2 to §4 and §10; epic #770, this skeleton #940; governed by ADR-007.

**What it does today:** one hardened window showing the React client, which
talks to **one** conchd over REST through the main process (issue #942, D2-3):
a "Server address" step, then the existing sign-in views, then chat, with the
credential held in main's memory only (restart means signing in again), and
live feeds that main opens and holds for the page (issue #943, D2-4). **What it
does not do yet:** saved profiles and persistence (#944, #945), attachments, clipboard, notifications and
links (#947, #773), the operations whose answers hold a credential the renderer
may not see (re-authentication, password change),
voice in the UI (#771: the supervisor that runs `conch-voice` exists, in
`main/voice/`, but nothing starts it yet), tray, installer. Later children of
#770 add those.

## Layout

| Path | Owns |
| --- | --- |
| `main/scheme.ts` | the `conch-app:` handler: serves only what the build-time manifest lists, checks each file's SHA-256, refuses every other URL |
| `main/csp.ts` | the Content-Security-Policy of every response: `connect-src wss: https:` (Q-D4-1), narrowed to the live grant's origin by `guards.ts` (HTTP and WebSocket requests only; WebRTC and WebTransport are outside both) |
| `main/window.ts` | the one window and its `webPreferences` (pinned by a unit test) |
| `main/guards.ts` | what a web contents may not do: navigate, open windows, attach webviews, download, ask for permissions, choose devices; and the request filter, which passes only the live companion grant's origin (wss or https, same host and port, no userinfo) besides the app's own scheme |
| `main/display-capture.ts` | the one screen capture: the permission and display-media rules (`Screen capture`, below) |
| `main/share-allowance.ts` | the share allowance: one `shareArm()` click, a grant use good for 10 s and a capture use good for 10 s from the later of the arm and the grant |
| `main/portal.ts` | whether the screen-cast portal can answer (Wayland session, a read-only `gdbus` property call) |
| `main/api/origin.ts` | the server-address rules (https; http only for loopback; no userinfo, path, query or fragment) |
| `main/api/target.ts` | an operation id plus values to a request target; refuses anything that is not a call of the table |
| `main/api/client.ts` | the one request executor: bearer only for bearer operations, no Origin or Cookie, no redirects, size cap, response validation, generation check |
| `main/api/session.ts` | the in-memory credential, the server address and the generation; holds the operator code-step challenge behind a handle |
| `main/api/service.ts` | what each bridge function does: the sign-in sequence, sign-out, restore, and the six secrets made for someone else (`display`) |
| `main/api/feeds.ts` | the live feeds: subscriptions, frame validation, the bounded queue, the held-socket count and cap, and teardown at unsubscribe, reload, crash and sign-out |
| `main/api/socket.ts` | the feed socket: Node's `WebSocket` with the bearer header, through undici's `Agent` for the frame-size cap and a connection main can destroy |
| `main/api/trust.ts` | adds the system trust store to Node's roots; classifies a certificate failure |
| `main/ipc.ts` | the one registry of IPC handlers and the caller check every invocation passes |
| `main/voice/protocol.ts` | the voice-control protocol as the owner speaks it: Go's framing and event rules over the types generated into `web/src/schema`, version negotiation, the stream tracker (#948) |
| `main/voice/endpoint.ts` | the helper's Linux events endpoint: the verified `$XDG_RUNTIME_DIR`, `conch/<profile-id>` (0700), the stale sweep, the `sun_path` budget, the socket and the attach (#948); on Windows the owner's named pipe `\\.\pipe\conch-voice-<id>` and the same attach (#1785) |
| `main/voice/spawn.ts` | the bundled `conch-voice` and its trusted-path checks, the environment built from empty by an allowlist, the spawn options (#948); on Windows `conch-voice.exe`, the case-insensitive allowlist and `%SystemRoot%` as its directory (#1785) |
| `main/voice/supervisor.ts` | one helper per instance: start, commands and acks, the stop escalation and the reap, helper loss as one typed state (#948); on Windows the nonce is spent at the attach and the stop has no SIGTERM step (#1785) |
| `main/instance.ts` | one instance per data root (`requestSingleInstanceLock`), and the data root made 0700 with its ancestors checked (#945) |
| `main/data-root.ts` | on Windows, a `--user-data-dir` must be under `%USERPROFILE%`, `%APPDATA%` or `%LOCALAPPDATA%`, or the app does not start; `--allow-shared-user-data-dir` overrides it with a warning (#1788) |
| `main/profiles/store.ts`, `files.ts`, `ids.ts` | the saved servers: random 128-bit ids, the (origin, principal) binding rule, `profiles.json` (non-secret, 0600, atomic) and the profile directories (#945) |
| `main/profiles/switch.ts`, `hooks.ts` | the switch sequence (media hooks, sockets, requests, renderer, helper hooks, then the next profile, each bounded) and the hook interface #771 and #772 register on (#945) |
| `main/profiles/service.ts` | which profile is active, where its credential lives (the vault, or main's memory when storage is not protected), sign-out and remove; the compatibility gate of #946 is in `tokenFor`, before the vault is read or the server is asked about a credential (#945, #946) |
| `main/compat.ts`, `build/compat.json` | the allowlist of server releases (exact versions, never a `>=` range), the verdict on what `/healthz` said, the required response families, and the development flag (#946) |
| `main/profiles/activation.ts` | one activation in Electron: its own Session, Client, Service, feeds, gate and window in a fresh in-memory partition (#945) |
| `main/vault/electron-storage.ts` | the one adapter from Electron's `safeStorage` to the vault (#945) |
| `main/document.ts` | which frame shows the current document (the document generation) |
| `main/app.ts`, `main/main.ts` | startup |
| `main/app-user-model.ts` | the Windows AppUserModelID (the app id), set by `main.ts` before `start()` can make a window or a notification (#900) |
| `preload/bridge.ts`, `preload/index.ts` | `window.conchDesktop`: a frozen object whose keys are exactly the functions of `web/src/platform/desktop/bridge.ts`; the page-side copy is not frozen, so nothing may rely on that |
| `shared/bridge-names.ts` | the function names and channels, shared by the preload and main |
| `build/manifest.ts` | writes `out/ui-manifest.json` (path, SHA-256, content type) |
| `build/electron-builder.config.json` | the Linux `dir` target, the Windows NSIS target (#900), asar and the fuses |
| `build/installer.nsh` | the Windows installer's own steps: per-user only, and the `conch://` key under HKCU (#900) |
| `tests/unit/` | Vitest, no Electron runtime, no display, no network |
| `tests/fixtures/fake-helper.ts` | a stand-in `conch-voice` that replays `pkg/schema/testdata/voice-control*/` transcripts |
| `tests/integration/` | the supervisor with the real `conch-voice` and a stub conchd (`make desktop-voice-it`) |
| `tests/smoke/` | the packaged smoke test (Playwright over CDP) |

Generated, git-ignored: `node_modules/`, `out/` (bundles, the web client, the
manifest), `dist/` (the unpacked app).

## Server compatibility (issue #946)

Before a saved server's credential is read, or the server asked about it, main
reads `/healthz` and compares its `version` with `build/compat.json`. A version
on the list proceeds. Any other (or none) shows "This server's version (X) has
not been tested with this app (Y)" and offers no sign-in: the page shows no
client, and main refuses every sign-in step, whatever the page sends. A server
that does not answer `/healthz` is the same, with its own wording. Nothing is
forgotten or revoked for it; the stored sign-in waits for a supported server.
After sign-in, an answer of a required family (`REQUIRED_FAMILIES` in
`main/compat.ts`) that is not the document this app was built for is shown as
an incompatibility, not a network error.

`build/compat.json` is the development list: checked in, hand-kept, and what a
build without a release tag copies into the app (the build says so). A release
build (`CONCH_RELEASE_VERSION=v0.1.0-alpha.18 make desktop-build`; the release
tag is the version, #1595) generates `out/compat.json` instead: that tag plus
the patch releases in `build/compat-extra.json` (hand-kept, may be empty), by
`build/manifest.ts`. It also stamps the tag into the packaged app's version
(`0.1.0-alpha.18`; the checked-in `package.json` stays `0.0.0`, a development
build says `0.0.0-dev+<short sha>`) and fails unless the bundled
`resources/conch-voice --version` names that version. The tag rules and the
Debian spelling (`0.1.0~alpha.18`) are in `scripts/desktop-version.sh`. The
matching is still exact, never a range. Development
against an unlisted server: `electron . --allow-unlisted-server` in an
**unpackaged** run. It shows a persistent warning. A packaged app ignores the
flag (`allowUnlisted`, tested), and a server that did not answer is never let
through. The smoke test stamps a disposable conchd with the first listed release
(and, for the unsupported case, with another) by `-ldflags`.

## Build, run, test

Needs the Node in `.nvmrc` (the Makefile uses nvm's copy if there is one, and
fails naming the version otherwise). Only `npm ci` installs.

```sh
make desktop-check   # gates, npm ci, typecheck, lint, unit tests; part of make check
make desktop-build   # fetch Electron, build, pack: desktop/dist/linux-unpacked/conch-desktop
make desktop-smoke   # drive that app; needs a display
```

`desktop-build` also copies the voice helper, `bin/conch-voice` (`make rust-build`)
or the file `CONCH_VOICE_BIN` names, to `dist/linux-unpacked/resources/conch-voice`
(mode 0755, `scripts/desktop-voice-helper.sh`), the one path `findHelper` runs;
nothing is looked up on `PATH`. A `bin/conch-voice` older than any file under
`voice/` is refused as not built from this tree. With no helper the build
succeeds and says "built without the voice helper" (the app shows voice
unavailable, text works); `DESKTOP_REQUIRE_VOICE=1` makes that a failure. The
`.deb` is `make desktop-deb` (below). CI's `desktop-smoke` job builds without
the helper today, so the smoke test's presence and mode checks run only on a
machine that built `bin/conch-voice`; `desktop/tests/unit/voice-bundle.test.ts`
covers the copy and `findHelper` in CI.

`desktop-check` downloads no Electron and needs no display. `desktop-build`
fetches the pinned Electron with `scripts/desktop-electron.sh` into
`~/.cache/conch/electron/` (a directory of its own that the script makes private, mode 700, and refuses if group or other can write to it) (`CONCH_CACHE` moves it), once. It does not
use the `electron` package's `install-electron`: that extracts with a package
that loads prebuilt native code, and `.npmrc` keeps install scripts off. The
script downloads only from Electron's GitHub release and keeps the zip only if
its SHA-256 equals both the pin in `scripts/desktop-pins.sh` and the hash in the
`electron` npm package's `checksums.json`. Only that verified zip is shared
(re-hashed on every build); each build unpacks a fresh copy into its own
`out/electron-dist/`, so builds in two worktrees never touch each other's tree.
electron-builder then packs around that directory (`electronDist`) and
downloads nothing.

The web client is built by web's own Vite with `--base /` into `out/ui`; no
file under `web/` changes and `web/dist` is not touched. `scripts/desktop-web-dist-check.sh` and the
CI job check that no desktop code is in the browser build.

Run the unpacked app by hand: `desktop/dist/linux-unpacked/conch-desktop`.

### The `.deb`

```sh
make desktop-deb     # build in release mode, pack and check: desktop/dist/conch-desktop_<version>_amd64.deb
CONCH_RELEASE_VERSION=v0.1.0-alpha.18 make desktop-deb   # the release package, version 0.1.0~alpha.18
```

`scripts/desktop-deb.sh` packs `dist/linux-unpacked` with `dpkg-deb
--root-owner-group` (needs `dpkg-dev`; it installs nothing and never runs as
root) and refuses to run without `resources/conch-voice`. The decisions are
Nick's, recorded on #1595:

- The tree is installed root-owned at `/opt/conch-desktop`, with `resources/conch-voice` at 0755
  (`findHelper` accepts a root-owned install). `/usr/bin/conch-desktop` links to
  `/opt/conch-desktop/conch-desktop`; `conch` stays the CLI.
- The desktop file is `chat.conch.desktop.desktop` (`Exec=conch-desktop %U`,
  `StartupWMClass=conch-desktop`, `desktopName` in `package.json`), with
  placeholder icons from `build/icons/` under `hicolor/<size>x<size>/apps`. No
  `MimeType=x-scheme-handler/conch;` is the one line it sets: the app takes
  `conch://invite?server=<origin>&code=<code>` links (issue #1592;
  linux-desktop.md section 9) and no other scheme or file type. `xdg-mime` reads it
  from the installed file; there is no `postinst` step for it.
- The sandbox stays on and there is **no setuid `chrome-sandbox`** (left out of the package):
  `/etc/apparmor.d/conch-desktop` (a conffile) gives the one executable the `userns`
  permission, for systems that restrict unprivileged user namespaces (Ubuntu 23.10
  and later). `postinst` loads it, and refreshes the desktop and icon caches;
  `postrm` unloads it. All of it is best effort. On an older AppArmor that cannot parse it,
  the install says so and goes on (user namespaces are unrestricted there).
- **The package needs AppArmor 4 and an Ubuntu 24.04-class system** (the profile
  is `abi <abi/4.0>`; the control description says so). If `apparmor_parser` cannot
  load it, `postinst` says plainly that the profile cannot load on this host and that
  the app will not start where user namespaces are restricted, and the install goes on.
- Nothing in the package is a udev rule, a group change, `setcap` or setuid, and
  nothing is group- or other-writable. The talk-key permission stays an explicit step
  the app shows (#1381).
- `Depends` is computed by `dpkg-shlibdeps` from the ELF files in the tree, on the
  machine that builds, so it names that machine's library packages: build releases on the oldest
  system that should install them.
- Two builds of one commit are byte-identical (`SOURCE_DATE_EPOCH` is the commit
  time, entries are sorted by `dpkg-deb`, modes are set by the script).
- The version is the release tag (`scripts/desktop-version.sh deb`). Updates are manual: install the
  newer `.deb`.
- A package replaced under a running app (#1600): `resources/version` holds the installed app
  version (the build writes it beside the asar). The app reads it at start, when a window gains
  focus and every ten minutes (`main/installed.ts`); a different one shows "Conch was updated to X.
  Restart when you're ready" with a Restart button (the normal quit, then `app.relaunch()`). Until
  then a call goes on, Join and Retry say "Restart Conch to finish the update", and nothing
  restarts by itself. A page that does not start after the update asks in a native dialog.

`scripts/desktop-deb-test.sh <deb>` checks a built package (`make desktop-deb`
runs it); with no argument it builds packages from a fixture tree, checks them,
and breaks them one way at a time to show that each check can fail (it is part
of `make desktop-check`). Beyond modes and owners it pins the two things that run
as root or change host policy: the AppArmor profile's rules must equal the expected
lines exactly (no wider attachment, no added `capability` or `mount`), and the control
members must be exactly `control conffiles postinst postrm`, with `postinst` and
`postrm` byte-identical to `desktop/build/deb/`. It also requires one `Exec=` line and
exactly `MimeType=x-scheme-handler/conch;` and no `Actions=` in the desktop file, no `chrome-sandbox`, and no symlink
but `/usr/bin/conch-desktop`.

`scripts/desktop-deb-lifecycle.sh` (`make desktop-deb-lifecycle`, issue #1599) is the
package's lifecycle test: in an `ubuntu:24.04` container (pinned by digest) it installs
an older build with `apt install ./old.deb` (apt resolves `Depends` from the archive, so
it needs Docker and network), checks the files, owners and modes, the launcher link, the
desktop file and `xdg-mime query default x-scheme-handler/conch`, seeds a test user's
`~/.config/Conch/profiles.json` and `settings.json`, upgrades to a higher version
(those files stay byte-identical, and `/opt/conch-desktop` is `diff -r`-identical to the
new `.deb`'s own tree, as it was to the old one after the install), checks that `ldd`
resolves every library of every ELF file (a `.so` may name a sibling in the tree; real
packages must have `conch-desktop` and `conch-voice` among them) and
that `resources/conch-voice` passes `findHelper`'s trust rules, then removes and purges
(nothing of the package left in `/opt`, `/usr` or `/etc`; the conffile goes at purge; the
user's `~/.config/Conch` stays). `postinst` must succeed with no AppArmor in the
container. With no argument it builds two fixture packages (`v0.1.0-alpha.1` and
`.2`); `make desktop-deb-lifecycle DEB_OLD=<a.deb> DEB_NEW=<b.deb>` takes real ones.
`scripts/desktop-deb-lifecycle.sh --broken` is the test of the test: the fixture packages
without `Depends` must fail at the `ldd` check, and `--broken=userdata` (a `postinst` that
writes under `~/.config/Conch`) must fail at the user-data check. The fixture run needs a
host no newer than Ubuntu 24.04 with `curl` installed (its ELF stand-in). A container is not a clean machine (no
display, portal, keyring or PipeWire; Chromium's sandbox under Docker's seccomp is not
representative), so the script never runs the app: the app on a real machine is #1602
(D6-08). Without Docker it prints `SKIPPED`; with `CI` set it fails. It is not part of
`make check` or `make desktop-check`, and has no CI job yet (it follows #1598's `.deb` job).

### The Windows installer (issue #900): an unsigned preview

**Unsigned.** It is a preview for testing on Nick's own PC (#903), not a release. Signing
is #780 (decision record: [docs/design/windows-signing.md](../docs/design/windows-signing.md)),
updates are deferred ([windows-desktop.md](../docs/design/windows-desktop.md) §9.5), and no
release carries it. Its name says so: `Conch-<version>-unsigned-preview-setup.exe`, and the
`desktop-windows` CI job keeps it as the artifact of the same name. Using it:
[docs/manual.md](../docs/manual.md#the-desktop-app-on-windows-unsigned-preview).

It is electron-builder's NSIS target (§9.3), the same `electron-builder` 26.17.0 that packs
the Linux tree; no dependency was added. The options, each read in that version's
`app-builder-lib/out/targets/nsis/nsisOptions.d.ts` and `NsisTarget.js` (§9.3 had them from
memory):

- `oneClick: false` (its default is `true`): the assisted installer, which may show the
  folder page; `allowToChangeInstallationDirectory: true` turns that page on (default
  `false`, assisted only).
- `perMachine: false` (the default): per-user. On an assisted installer that alone still
  shows a page offering "all users" too, which an administrator could pick, and
  `/allusers` on the command line asks for elevation whatever `allowElevation` says
  (`templates/nsis/multiUserUi.nsh`, `assistedInstaller.nsh`). `build/installer.nsh`
  therefore makes every install the current user's, `/allusers` included, and the page
  is never shown. One gap remains: the uninstaller, started by hand with `/allusers`,
  asks for elevation before any hook runs, and then still removes only the current
  user's install.
- `allowElevation: false` (default `true`, assisted only) and `packElevateHelper: false`
  (default `true`; it exists for electron-updater's per-machine updates): no elevation is
  asked for and no `elevate.exe` is shipped.
- `deleteAppDataOnUninstall: false` (the default; the type says one-click only, but
  `uninstaller.nsh` honours it either way). The uninstaller removes the program, its
  shortcuts, its uninstall entry and the `conch://` key, and leaves `%APPDATA%`. The app's
  data is Electron's `userData`, `%APPDATA%\conch-desktop` (the package name, as
  `~/.config/conch-desktop` on Linux). Only `Uninstall Conch.exe --delete-app-data`, which
  someone must type, removes it.
- `artifactName` puts `unsigned-preview` in the file name.

The `conch://` handler: electron-builder's `protocols` is macOS only
(`out/options/PlatformSpecificBuildOptions.d.ts`), so `build/installer.nsh` writes
`HKCU\Software\Classes\conch` (`URL Protocol`, and `shell\open\command` =
`"<install dir>\Conch.exe" "%1"`), as the `.deb`'s `MimeType=x-scheme-handler/conch;` does on
Linux, and its uninstall step deletes it. The link reaches the app on its command line, which
`main/instance.ts` and `main/invites.ts` read on every platform.

The install goes to NSIS's per-user `$LocalAppData\Programs\Conch` (the `UserProgramFiles`
known folder, `%LOCALAPPDATA%\Programs` by default): `Conch.exe`, and `resources\` with
`app.asar`, `conch-voice.exe` (the one path `findHelper` runs on Windows,
`main/voice/spawn.ts`) and `version` (the installed stamp, `main/installed.ts`). The executable
keeps electron-builder's Windows name, `Conch.exe`: the Linux name `conch-desktop` avoids the
CLI's `conch` on `PATH`, and the per-user install folder is not on `PATH`. The Start menu and
desktop shortcuts carry the AppUserModelID `chat.conch.desktop`, the `appId`, which
`main/app-user-model.ts` also sets in the app.

**It is built on Windows only.** On Linux, electron-builder 26.17.0 packs everything and then
runs the uninstaller it has just compiled under `wine` to extract it (`NsisTarget.js`,
`computeScriptAndSignUninstaller`): tried on 2026-11-12, it fails there with `spawn wine
ENOENT`. The repository installs no wine, so there is no Make target. `npm run dist:win` is the
build step, and CI's `desktop-windows` job runs it after the steps of
`scripts/desktop-build.sh` that are not Linux's: the release `conch-voice.exe` (`cargo build
--release -p conch-voice`), the Windows Electron, web's Vite build, the bundles and the
manifest. It stages the helper and the stamp in `out/win-resources/` (`win.extraResources`),
because electron-builder packs the installer itself. With a compile-only stand-in for `wine`,
both halves of the script compiled on Linux under `makensis -WX`. That installer was never run.

What electron-builder downloads for it, each checked against a SHA-256 written in its own
source before it is unpacked (`downloadBuilderToolset` in `out/util/electronGet.js` passes the
hash to `@electron/get` 5.1.0, whose `validateArtifact` refuses a mismatch), from
`electron-userland/electron-builder-binaries`: `nsis-3.0.4.1.7z` and `nsis-resources-3.4.1.7z`
(the default `toolsets.nsis` "0.0.0"; `out/toolsets/windows.js`), `7zip-win-x64.tar.gz`
(`out/toolsets/7zip.js`) and `icons-bundle.tar.gz` (the icon from `build/icons/`,
`out/toolsets/icons.js`). The version resource is edited in JavaScript (`resedit`), so no
`rcedit` and no `winCodeSign` is fetched, and nothing is signed. Electron itself is not
downloaded by electron-builder: the job fetches `electron-v<version>-win32-x64.zip` from
Electron's GitHub release and unpacks it only if its SHA-256 equals the hash in
`node_modules/electron/checksums.json`, which `npm ci` verified against the lockfile, then
passes it as `electronDist`. That is one of the two hashes `scripts/desktop-electron.sh` checks
on Linux. `scripts/desktop-pins.sh` has no Windows hash, and adding one is a pin change.

CI installs it with `/S` on `windows-latest` and checks the files, `Conch.exe`'s version
resource (the app has no window-less version check), the HKCU `conch` key and uninstall
entry, the Start menu shortcut, and that nothing went to `Program Files` or `HKLM`. It then
seeds `%APPDATA%\conch-desktop` and `%APPDATA%\Conch`, uninstalls with `/S`, and waits for the
copy the uninstaller runs from `%TEMP%` to finish. The program, the key, the entry and the
shortcut must be gone, and every seeded file must be unchanged. The job does not start the app.
Electron and `conch-voice` together on a Windows machine is #903.

### A data root of your own on Windows (`--user-data-dir`, issue #1788)

Chromium's `--user-data-dir=<dir>` moves the data root (`app.getPath('userData')`, by default
`%APPDATA%\conch-desktop`): `profiles.json`, `settings.json`, the vault, each profile's
`voice.toml` for the helper, and Chromium's own files. On Linux and macOS `main/instance.ts`
judges it by its mode bits and its ancestors' (0700, no group- or other-writable parent). Windows
has no mode bits: the data is private only because the folder it is in is. `%USERPROFILE%`,
`%APPDATA%` and `%LOCALAPPDATA%` are, under their default ACL (you, SYSTEM and
Administrators), but a folder made under a drive root (`C:\conch-data`) gives Authenticated
Users Modify by default, so any other local account could write `profiles\<id>\voice.toml`
there and choose the helper's devices and keys (not its server or login: in the app those come
from the owner's `start` command), or change the app's other files.

So on Windows, when `--user-data-dir` is given, `main/data-root.ts` checks where it resolved to
before the single-instance lock writes anything into it. The root must be inside `%USERPROFILE%`,
`%APPDATA%` or `%LOCALAPPDATA%` (each as written and resolved) twice over: as given, and with
junctions and symbolic links resolved by `fs.realpathSync.native` (through its nearest existing
ancestor when it does not exist yet). The path as given counts because the app goes on using it: a
junction at a shared place such as `C:\conch-data` that leads into the profile is refused, since
whoever owns that junction could point it elsewhere later. Paths are compared case-insensitively on
whole path segments, so `C:\Users\nickx` is not inside `C:\Users\nick`. A relative, drive-relative
(`C:foo`) or root-relative (`\foo`) path is refused, as are a root that cannot be resolved, a
dangling link on its path, and a base variable that names a volume or share root (`C:\`,
`\\server\share`). Refused, the app prints one line saying the data root must be
under your user profile and exits. `--allow-shared-user-data-dir` is the deliberate way past it: the
app starts and prints a warning that other local accounts may be able to change its files, the
voice helper's settings among them. Without `--user-data-dir` nothing is checked, and Linux and
macOS are unchanged.

**Why by path, not by ACL:** Node has no API that reads a Windows ACL; one would need a native
module (or a new dependency that ships one), which this app does not take. So the check is a
guard against a mistaken launch, not proof: a profile folder whose ACL someone has widened passes,
and a user who passes the override flag is trusted to mean it, as is anything else running as that
user. The standalone helper has the same residual for a `CONCH_CONFIG_DIR` under a drive root
([windows-desktop.md](../docs/design/windows-desktop.md) §2.7, #904); it is documented there and not
checked.

### The smoke test

`tests/smoke/` starts the **packaged, fused** app and connects to it with
`chromium.connectOverCDP`. Playwright's own `_electron.launch` waits for the Node
inspector's "Debugger listening on", which the `EnableNodeCliInspectArguments`
fuse turns off in the shipped build, and the test must not use an unfused
build. So the test passes Chromium's `--remote-debugging-port=0` (an OS-chosen
loopback port) itself; **the app never turns that port on**, and a launch
without the flag has none. The feeds spec (`tests/smoke/feeds.spec.ts`, #943)
also puts a browser user beside the app, in the Chromium that Playwright's own
version pins, signed in as another member at the disposable conchd's `/app/`
(which serves `web/dist`: `make web-build` first). Fetch that browser once with
`cd desktop && npx playwright install chromium`.

A packaged app that is started with `--remote-debugging-port` or
`--remote-debugging-pipe` prints `conch-desktop: remote debugging is ON` and
exits 1 unless `CONCH_DESKTOP_ALLOW_CDP=1` is set (`main/debugging.ts`; the
smoke test's launcher sets it, and a smoke test asserts the refusal). That gate
catches launcher mistakes and keeps the choice explicit. It is not a defence
against a same-user attacker, who can set the variable too. The port is not
opened on a refused launch (checked by the smoke test).

It checks the window, the response headers, the renderer's lack of Node, the
refusals (navigation, windows, URLs outside the manifest, foreign requests),
that the renderer is in a PID namespace of its own (the Chromium sandbox is
engaged), and the fuses by behaviour. It prints the app's `process.versions` as
its first receipt.

CI runs it under `xvfb-run` in the `desktop-smoke` job. With no X server to
hand, `CONCH_SMOKE_HEADLESS=1 make desktop-smoke` uses Chromium's headless Ozone
platform instead.

### The sandbox

The Chromium sandbox is never turned off: no file in the repository passes the
flag that does that (`scripts/desktop-lint.sh` greps for it). On Ubuntu-24.04
family kernels, AppArmor can restrict the unprivileged user namespaces the
sandbox's namespace mode needs; the other mode is a setuid-root `chrome-sandbox`.
On the Pop!_OS 24.04 workstation `unshare --user true` works (observed
2026-10-28) and the unpacked build runs sandboxed as is. On the CI runner the
job makes the helper setuid root (see the comment in `ci.yml`). The packaged
answer, decided by Nick on 2026-11-09 (#1595), is an AppArmor profile installed
with the `.deb`, not a setuid sandbox; see
[docs/design/linux-desktop.md](../docs/design/linux-desktop.md) §9
(Distribution) for the package decisions.

The fuses are set by electron-builder (`electronFuses`): RunAsNode off,
NODE_OPTIONS off, Node CLI inspect arguments off, embedded asar integrity
validation on, only-load-app-from-asar on, file-protocol extra privileges off,
cookie encryption on. Electron validates the asar's embedded hash on macOS and
Windows only; on Linux that fuse is set and does nothing. The scheme handler's
per-file SHA-256 check verifies that the UI files it serves are the ones the
build listed; it is **not tamper protection**, because the manifest, `main.cjs`
and the preload come from the same asar and nothing checks them. On Linux, what
keeps the installed app from being modified is the install directory's
ownership (a root-owned package directory, #774), not anything in the app.

## Dependencies

All are `devDependencies`; nothing from `node_modules` ships in the app. The
two shell packages are `electron` and `electron-builder`, exact versions; the
rest are the tools `web/` already uses, at `web/`'s exact versions
(`scripts/desktop-lint.sh` checks they stay equal). A new direct dependency
needs Nick's sign-off and a line in `deps-allowlist.txt`; a new licence needs
his sign-off and a line in `licences-allowlist.txt`.

## Talking to the server (issue #942)

The renderer makes no HTTP or WebSocket request (its CSP says `connect-src wss: https:`, and `main/guards.ts` cancels all of them but exactly the live companion grant's LiveKit origin; when the grant ends main asks the session to close its connections, which reaches idle and pending sockets only; a live WebSocket is ended by conchd's removal of the participant, and when that removal is not confirmed main reloads the window's document). WebRTC (ICE, STUN, TURN) and WebTransport are outside both the CSP and that request filter. Main
makes them all, with Node's `fetch`, from `main/api/client.ts`. The renderer
names an operation of the table in `web/src/platform/operations.ts` and its
values; main builds the URL, sends `Authorization: Bearer` for a bearer
operation only, sends no `Origin` and no `Cookie`, uses no proxy, follows no
redirect (a redirect is a typed failure on every operation, and the credential
is never sent to its target), reads at most 4 MiB of answer and validates it
with the operation's validator before the renderer sees it. A 401 on a bearer
operation signs the window out. Sign-in steps (password, operator code,
invite, sign-in link, setup token, reset, guest, account, pasted credential)
are bridge functions of their own: main consumes the token and returns the
account; the operator's code-step challenge stays in main behind a handle.

Six operations make a secret for someone or something else, which the person
has to read or copy (issue #1768): an invite's link (`createMemberLink`), a
sign-in link for another device (`createSignInLink`), the authenticator's seed
(`totpEnroll`), an administrator's sign-in link for a person
(`createPrincipalSignInLink`), a script credential (`createPrincipalCredential`)
and a hook's secret (`createHook`). They are not the app's own credentials, so
unlike a bearer the page may hold them for display. Each is a bridge function of
its own (`Service.display` in `main/api/service.ts`, never a generic
pass-through): main makes the request with its bearer, exactly as the page
would, and the server's validated reply goes back once, in that call's result.
Main does not log it (a line names the operation and a reason, never a body),
store it or keep it after returning. The operations stay `renderer: false`, so
the generic `request` still refuses all six. The page shows and copies the
secret, and keeps it in no URL, title or log; a copy button goes through main's
clipboard write in the click handler, as AddDevice and the invite panel do.

Every IPC invocation is checked in `main/ipc.ts` before anything else: the
sender is the owned web contents and is not destroyed; the frame is its live
main frame (not a subframe, not detached) at `conch-app://ui/`; it is the frame
of the current document generation; then the function name, a per-function rate
limit, a byte cap, and the argument shape. A refusal is one opaque `refused`,
counted and logged by reason only.

### Live feeds (issue #943)

The renderer subscribes by feed kind (`messages`, `events`, `voice`,
`occupancy`) with exactly that kind's parameters (a channel name, the cursor,
the renderer's session), under a number the preload chose; main refuses
anything else. Main opens the socket itself (`main/api/feeds.ts`), with Node's
`WebSocket` (Q7: no `ws` package). Measured on the pinned Electron 44.10.0
(Node 24.22.1), 2026-10-31: its `WebSocket` takes a `headers` option, sends
the `Authorization` header it is given, and sends no `Origin` and no `Cookie`;
conchd reads a ticket only when no `Authorization` header is present, so main
sends the bearer header, asks for no ticket, and the bearer is never in a URL.
An upgrade answered with a redirect fails, and its target is never asked.

Every frame is checked before the page sees it: text only, at most 1 MiB (a
larger one is refused at its header, before its payload is read), and
valid for its feed by the generated validators (a v2 message or the heartbeat;
an events document the client's own parser accepts, or the heartbeat; a
presence document; an occupancy document). Anything else is dropped and
counted. Frames go to the page one batch at a time, the next only once the
preload has acknowledged the last; at most 512 frames or 4 MiB wait, and past
either main closes the socket and tells the page 1013, which the page's own
feed state machine retries like any lost socket (the messages feed resuming
after the newest id it holds). The reconnect and the heartbeat watchdog stay
the page's; main reports open, the frames and one close.

An unsubscribe closes its socket. A reload, a crash or the window closing ends
the document, and every socket it owned is closed before the next document can
subscribe. Sign-out, a new sign-in and a change of server close every socket
before the next identity's first request (the session's abort signal, in the
same turn), and a frame that arrives after that is dropped, never sent on. A
socket that fails before it opens is followed by one `whoami`: a 401 there
signs the window out, and the page is told 1008, as the browser learns from a
ticket refused with 401. Main logs each socket opened and closed, with its
kind, the reason and how many are open and closing, never its address.

A socket main has closed stays held, and counted as closing, until its close
event: a server that never answers the close frame cannot keep it, because
main destroys it 5 s after closing it. With 64 sockets held, open or closing,
a new subscription is refused (the page retries it, 1013), so no server can
make main run out of descriptors. Both limits come from undici's own `Agent`,
the class of Node's global dispatcher (no `undici` package): one per socket,
with `webSocket.maxPayloadSize` (the 1 MiB cap, at the frame header, where
undici's default would buffer 128 MiB) and a `connect` hook that makes the TCP
or TLS connection with Node's `net` and `tls` defaults, so main can destroy it.
The hook passes no CA list and no verification option: the process-wide roots
and the hostname check apply as they do to a fetch (`tests/unit/feeds.test.ts`
runs both TLS cases in a fresh process). Measured on the pinned Electron's Node
24.22.1, 2026-10-31: the global dispatcher is an `Agent` before the first
request, a 2 MiB frame header ends the socket in about 20 ms, and a socket whose
server ignores the close frame has no close event until it is destroyed. If the
dispatcher is not an `Agent`, main says so once in its log and opens a plain
`WebSocket`, which still has the after-the-fact size check and the count.

### Saved servers (issue #945)

A profile is a random 32-hex id mapped to a canonical origin and, after the
first sign-in, the account (principal id) it is bound to. Signing in as another
account on a bound profile is refused and its credential forgotten; a second
account of a server is a second entry. A switch or a sign-out destroys the
window and makes new web contents in a new in-memory partition, with a new
Session, Client, feeds and gate, so nothing of one profile's (a pending
response, a late socket frame, a draft, a preference) reaches another's. Only a
member's sign-in is kept: in the vault when storage is protected, otherwise in
main's memory for this run, with the reason shown and a Retry. The data root is
`app.getPath('userData')` (0700); `profiles.json` is metadata only. Two
instances on one root: the second focuses the first and exits. A `profiles.json`
that cannot be read gives a recovery screen and changes nothing until asked.

### Which HTTP stack (Q6)

Measured on the pinned Electron 44.10.0 (Node 24.22.1, Chromium 152.0.7977.169), against a
local HTTPS server whose certificate was signed by a throwaway CA, 2026-10-29:

| Stack | CA not installed | `NODE_EXTRA_CA_CERTS=ca.pem` | System store (`SSL_CERT_FILE`/`/etc/ssl/certs`) |
| --- | --- | --- | --- |
| Node `fetch` (undici), in Electron's main | fails `UNABLE_TO_VERIFY_LEAF_SIGNATURE` | trusts | trusts after `tls.setDefaultCACertificates([...default, ...system])` (or `NODE_USE_SYSTEM_CA=1`) |
| `net.fetch` (Chromium's stack) | fails `ERR_CERT_AUTHORITY_INVALID` | **ignores it** (still fails) | not read; Chromium's own store plus the NSS database `~/.pki/nssdb`, which needs `certutil` (not installed here) to change |

Decision: **Node's `fetch`**. Electron's main has no WebSocket client of its
own, Node's `WebSocket` is the same undici and trusts the same roots, and Q7
forbids the `ws` package, so one stack for REST and for the feeds of #943 is
Node's. `net.fetch` would need a second, different trust procedure for the
sockets. Node's `fetch` also reads no system proxy, which §3 asks for. Main adds
the operating system's store to Node's bundled roots once at startup
(`main/api/trust.ts`); nothing relaxes a check.

### Private CA

Install the CA the way the system documents (Debian and Ubuntu: copy it to
`/usr/local/share/ca-certificates/` and run `sudo update-ca-certificates`) and
start the app. Or start it with `NODE_EXTRA_CA_CERTS=/path/to/ca.pem`, which
Node reads at start. Nothing else is supported: the app never asks to trust a
certificate it has refused, and no code sets `rejectUnauthorized`,
`NODE_TLS_REJECT_UNAUTHORIZED`, a certificate-verify procedure or the
certificate-error event (`tests/unit/trust.test.ts` greps for them, and runs a
child process against a local HTTPS server with a test CA, both ways). The app
says "does not trust that server's certificate" on the address step.

### Localhost development

Plain `http://` is accepted for `localhost`, `127.0.0.0/8` and `[::1]` only, the
same rule as `web/src/transport.ts`: `conchd serve --listen 127.0.0.1:8080
--no-web`, then enter `http://127.0.0.1:8080`. The server refuses passwords over
plain HTTP from anywhere else, and so does the address step.

## Screen capture

The renderer captures the screen only through Chromium's PipeWire capturer, so only through the xdg-desktop-portal ScreenCast picker (Q-D4-4 on #1578). `main/app.ts` turns the capturer on before `ready`; `main/portal.ts` asks the portal whether it can answer, and where it cannot (no portal, or an X11 session, where Chromium would list every source with no picker) Share is unavailable and the page is told "share unavailable: no screen-cast portal". `main/display-capture.ts` grants a capture only from the owned window's live main frame at `conch-app://ui/`, in the current document generation, with a live companion grant, and with the share allowance's capture use unspent (see below), and only when `desktopCapturer.getSources` returns exactly one source (none is a cancel; several are refused, never narrowed to the first). Video only: audio is never granted, and microphone and camera stay denied. A packaged launch with `--disable-features` naming `WebRTCPipeWireCapturer` is refused at start (`captureVerdict`), and a renderer that asks for a desktop capture without a picker is crashed, then its page closed if the process has not gone a second later.

### The share allowance (`main/share-allowance.ts`)

One click on Share is followed by two things that each need the person's act: the companion's grant, then `getDisplayMedia`. The act is one-shot, so the page calls `shareArm()` synchronously at the very start of the Share click, before anything else. It takes the click and arms an allowance for that web contents and document: it has two uses: one for `companionGrant` (consumed in place of a fresh click; good for `SHARE_ARM_MS`, 10 s, from the arm) and one for the capture (consumed when the pick starts; good for `SHARE_CAPTURE_MS`, 10 s, from the later of the arm and the grant being issued from that allowance, so the warm path has 10 s from the arm and the cold path 10 s after the grant round trip, long enough for the companion's LiveKit connect on a slow link, #1632; a grant confirmed more than `SHARE_CONFIRM_MS`, 15 s, after the grant use was spent is some other grant and moves nothing). Residual, stated plainly: a compromised page can still call `shareArm` within 1 s of any act, so the OS portal picker is the consent, and the allowance only stops an honest page arming capture by accident. Arming again replaces it. Nothing else arms a capture: a raw click, or the Enter that sends a chat message, does not. Watching needs no arming: its `companionGrant` uses its own fresh click as before, and leaves an armed allowance alone. The allowance is cleared by any companion end or stop (leave, helper, renewal, sign-out, switch, quit, crash, reload, lock, suspend, the page's `companionEnd`) and by a document change. Everything else still applies: owned main frame, current document generation, a live grant, the portal. A grant's `shareUnavailable` (`no-portal`, `not-wayland`) tells the page before the pick that there is no screen-cast portal.
