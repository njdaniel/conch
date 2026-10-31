# desktop/: the Conch desktop shell

An Electron app that opens the existing web client from packaged files at
`conch-app://ui/`. Design: [docs/design/linux-desktop.md](../docs/design/linux-desktop.md)
§2 to §4 and §10; epic #770, this skeleton #940; governed by ADR-007.

**What it does today:** one hardened window showing the React client, which
talks to **one** conchd over REST through the main process (issue #942, D2-3):
a "Server address" step, then the existing sign-in views, then chat, with the
credential held in main's memory only (restart means signing in again), and
live feeds that main opens and holds for the page (issue #943, D2-4). **What it
does not do yet:** saved profiles and persistence (#944, #945), attachments, clipboard, notifications and
links (#947, #773), the operations whose answers hold a credential the renderer
may not see (re-authentication, password change, invite and device-link codes),
voice in the UI (#771: the supervisor that runs `conch-voice` exists, in
`main/voice/`, but nothing starts it yet), tray, installer. Later children of
#770 add those.

## Layout

| Path | Owns |
| --- | --- |
| `main/scheme.ts` | the `conch-app:` handler: serves only what the build-time manifest lists, checks each file's SHA-256, refuses every other URL |
| `main/csp.ts` | the Content-Security-Policy of every response |
| `main/window.ts` | the one window and its `webPreferences` (pinned by a unit test) |
| `main/guards.ts` | what a web contents may not do: navigate, open windows, attach webviews, download, ask for permissions, choose devices |
| `main/api/origin.ts` | the server-address rules (https; http only for loopback; no userinfo, path, query or fragment) |
| `main/api/target.ts` | an operation id plus values to a request target; refuses anything that is not a call of the table |
| `main/api/client.ts` | the one request executor: bearer only for bearer operations, no Origin or Cookie, no redirects, size cap, response validation, generation check |
| `main/api/session.ts` | the in-memory credential, the server address and the generation; holds the operator code-step challenge behind a handle |
| `main/api/service.ts` | what each bridge function does: the sign-in sequence, sign-out, restore |
| `main/api/feeds.ts` | the live feeds: subscriptions, frame validation, the bounded queue, the held-socket count and cap, and teardown at unsubscribe, reload, crash and sign-out |
| `main/api/socket.ts` | the feed socket: Node's `WebSocket` with the bearer header, through undici's `Agent` for the frame-size cap and a connection main can destroy |
| `main/api/trust.ts` | adds the system trust store to Node's roots; classifies a certificate failure |
| `main/ipc.ts` | the one registry of IPC handlers and the caller check every invocation passes |
| `main/voice/protocol.ts` | the voice-control protocol as the owner speaks it: Go's framing and event rules over the types generated into `web/src/schema`, version negotiation, the stream tracker (#948) |
| `main/voice/endpoint.ts` | the helper's Linux events endpoint: the verified `$XDG_RUNTIME_DIR`, `conch/<profile-id>` (0700), the stale sweep, the `sun_path` budget, the socket and the attach (#948) |
| `main/voice/spawn.ts` | the bundled `conch-voice` and its trusted-path checks, the environment built from empty by an allowlist, the spawn options (#948) |
| `main/voice/supervisor.ts` | one helper per instance: start, commands and acks, the stop escalation and the reap, helper loss as one typed state (#948) |
| `main/document.ts` | which frame shows the current document (the document generation) |
| `main/app.ts`, `main/main.ts` | startup |
| `preload/bridge.ts`, `preload/index.ts` | `window.conchDesktop`: a frozen object whose keys are exactly the functions of `web/src/platform/desktop/bridge.ts`; the page-side copy is not frozen, so nothing may rely on that |
| `shared/bridge-names.ts` | the function names and channels, shared by the preload and main |
| `build/manifest.ts` | writes `out/ui-manifest.json` (path, SHA-256, content type) |
| `build/electron-builder.config.json` | the Linux `dir` target, asar and the fuses |
| `tests/unit/` | Vitest, no Electron runtime, no display, no network |
| `tests/fixtures/fake-helper.ts` | a stand-in `conch-voice` that replays `pkg/schema/testdata/voice-control*/` transcripts |
| `tests/integration/` | the supervisor with the real `conch-voice` and a stub conchd (`make desktop-voice-it`) |
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

## Talking to the server (issue #942)

The renderer makes no network request (its CSP says `connect-src 'none'`). Main
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
