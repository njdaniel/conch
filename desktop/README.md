# desktop/: the Conch desktop shell

An Electron app that opens the existing web client from packaged files at
`conch-app://ui/`. Design: [docs/design/linux-desktop.md](../docs/design/linux-desktop.md)
§2 to §4 and §10; epic #770, this skeleton #940; governed by ADR-007.

**What it does today:** one hardened window showing the React client. **What it
does not do yet:** no network (the CSP says `connect-src 'none'`, so the client
shows its "cannot reach the server" state), no sign-in, no profiles, no
credentials, no IPC method (the preload exposes an empty `conchDesktop`),
no voice, no tray, no installer. Later children of #770 add those.

## Layout

| Path | Owns |
| --- | --- |
| `main/scheme.ts` | the `conch-app:` handler: serves only what the build-time manifest lists, checks each file's SHA-256, refuses every other URL |
| `main/csp.ts` | the Content-Security-Policy of every response |
| `main/window.ts` | the one window and its `webPreferences` (pinned by a unit test) |
| `main/guards.ts` | what a web contents may not do: navigate, open windows, attach webviews, download, ask for permissions, choose devices |
| `main/app.ts`, `main/main.ts` | startup |
| `preload/index.ts` | `window.conchDesktop`, empty; the page-side copy is not frozen, so nothing may rely on that |
| `build/manifest.ts` | writes `out/ui-manifest.json` (path, SHA-256, content type) |
| `build/electron-builder.config.json` | the Linux `dir` target, asar and the fuses |
| `tests/unit/` | Vitest, no Electron runtime, no display, no network |
| `tests/smoke/` | the packaged smoke test (Playwright over CDP) |

Generated, git-ignored: `node_modules/`, `out/` (bundles, the web client, the
manifest), `dist/` (the unpacked app).

## Build, run, test

Needs the Node in `.nvmrc` (the Makefile uses nvm's copy if there is one, and
fails naming the version otherwise). Only `npm ci` installs.

```sh
make desktop-check   # gates, npm ci, typecheck, lint, unit tests; part of make check
make desktop-build   # fetch Electron, build, pack: desktop/dist/linux-unpacked/conch
make desktop-smoke   # drive that app; needs a display
```

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

Run the unpacked app by hand: `desktop/dist/linux-unpacked/conch`.

### The smoke test

`tests/smoke/` starts the **packaged, fused** app and connects to it with
`chromium.connectOverCDP`. Playwright's own `_electron.launch` waits for the Node
inspector's "Debugger listening on", which the `EnableNodeCliInspectArguments`
fuse turns off in the shipped build, and the test must not use an unfused
build. So the test passes Chromium's `--remote-debugging-port=0` (an OS-chosen
loopback port) itself; **the app never turns that port on**, and a launch
without the flag has none. Playwright launches no browser here, so none is
downloaded.

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
answer (an AppArmor profile, a setuid fallback) is the `.deb` work, #774.

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
