// The shell's startup (linux-desktop.md §2, §3). Input: Electron's app events.
// Output: one hardened window showing the packaged web client, and nothing
// else but main's public-API client behind the bridge's IPC handlers (D2-3)
// and the live feeds main holds for the page (D2-4). The renderer itself
// makes no HTTP or WebSocket request but the screen companion's, to exactly
// the live grant's LiveKit origin, and no capture but one portal-picked screen
// (D4-03, #1580: guards.ts, display-capture.ts, portal.ts). WebRTC (ICE, STUN
// and TURN) and WebTransport are outside the CSP and the request filter, which
// see HTTP and WebSocket requests only (linux-desktop.md §3).
import { readFile } from 'node:fs/promises';
import { isAbsolute, join } from 'node:path';

import tls from 'node:tls';

import { app, BrowserWindow, desktopCapturer, dialog, ipcMain, Menu, Notification, powerMonitor, protocol, safeStorage, shell, Tray } from 'electron';

import { allowUnlisted, OVERRIDE_FLAG, parseCompat } from './compat.ts';
import { activationFactory, healthReader, Live, prober, revoker, verifier } from './profiles/activation.ts';
import { deliverInvite, Invites, registerInviteProtocol } from './invites.ts';
import { createProfileFiles } from './profiles/files.ts';
import { TeardownHooks } from './profiles/hooks.ts';
import { ProfilesService } from './profiles/service.ts';
import { ProfileStore } from './profiles/store.ts';
import { dataRootVerdict } from './data-root.ts';
import { acquire, keepNewest, prepareRoot } from './instance.ts';
import { Client } from './api/client.ts';
import { Session } from './api/session.ts';
import { deliverVoice } from './voice/events.ts';
import { Companion, stillHeldBy } from './voice/companion.ts';
import { watchMuteKey } from './voice/mute-key.ts';
import { OneShot } from './voice/oneshot.ts';
import { OWNER_VERSIONS_SHARE_AUDIO } from './voice/protocol.ts';
import { linuxShareAudioEnabled, registeredApplicationName, ShareSound } from './voice/share-audio.ts';
import { VoiceService } from './voice/service.ts';
import { createVoiceIdentity } from './voice/identity.ts';
import { Supervisor } from './voice/supervisor.ts';
import { trayHostPresent } from './tray-detect.ts';
import { TrayController, unblankSoon } from './tray.ts';
import { createNodeConnect } from './api/socket.ts';
import { transportVerdict, trustSystemRoots } from './api/trust.ts';
import { debuggingVerdict } from './debugging.ts';
import { captureVerdict, DisplayCapture } from './display-capture.ts';
import { Files } from './files.ts';
import { Gestures } from './gestures.ts';
import { ShareAllowance } from './share-allowance.ts';
import { shareTeardown } from './share-hook.ts';
import { installGuards } from './guards.ts';
import { deliverUpdate, Installed, shouldAskAfterFailure } from './installed.ts';
import { updateDialogButtons, updateDialogDetail, updateDialogMessage } from '../../web/src/platform/desktop/ui/text.ts';
import { Ipc } from './ipc.ts';
import { Links } from './links.ts';
import { integrationsView } from './integrations.ts';
import { Lifecycle } from './lifecycle.ts';
import { LinuxLifecycle } from './lifecycle-linux.ts';
import { WindowsLifecycle } from './lifecycle-windows.ts';
import { executable, Portal, runFile } from './portal.ts';
import { COMPANION_EVENTS } from '../shared/bridge-names.ts';
import { Autostart } from './autostart.ts';
import { Notifications } from './notifications.ts';
import { createSettingsFiles, memorySettingsFiles, SettingsService } from './settings.ts';
import { WindowMaximized } from './window.ts';
import { Diagnostics, LogRing } from './diagnostics.ts';
import { saveChosenFile } from './save-file.ts';
import { parseManifest, SCHEME, SCHEME_PRIVILEGES } from './scheme.ts';
import { electronStorage } from './vault/electron-storage.ts';
import { profileConfigDir } from './vault/files.ts';
import { createVault, type Vault } from './vault/vault.ts';

// The hooks #771 (native voice) and #772 (share) register their teardown on:
// run first on every switch and sign-out (profiles/switch.ts).
export const teardownHooks = new TeardownHooks();

// registerScheme must run before the app is ready.
export function registerScheme(): void {
  protocol.registerSchemesAsPrivileged([{ scheme: SCHEME, privileges: { ...SCHEME_PRIVILEGES } }]);
}

// The last lines logged, kept in memory for the diagnostics bundle (issue
// #2084). Every line main logs goes through record(), scrubbed as it is kept.
const ring = new LogRing();

function log(line: string): void {
  ring.record(line);
  console.error(line);
}

// A vault that stores nothing, for a data root that is not private to this user.
function refusingVault(): Vault {
  const unsafe = { kind: 'sessionOnly', reason: 'unsafe-root' } as const;
  return {
    classify: () => Promise.resolve(unsafe),
    put: () => Promise.resolve(unsafe),
    get: () => Promise.resolve(unsafe),
    remove: () => Promise.resolve({ kind: 'removed' }),
  };
}

export async function start(): Promise<void> {
  if (debuggingVerdict(app.isPackaged, (name) => app.commandLine.hasSwitch(name), process.env, log) === 'refused') {
    app.exit(1);
    return;
  }
  if (transportVerdict(process.env, log) === 'refused') {
    app.exit(1);
    return;
  }
  // A launch that turned the PipeWire capturer off would let one source
  // stand in for the portal's pick (display-capture.ts).
  if (captureVerdict(app.isPackaged, (name) => app.commandLine.getSwitchValue(name), log) === 'refused') {
    app.exit(1);
    return;
  }
  // On Windows, a --user-data-dir must be under the user's profile, whose ACL
  // keeps other local accounts out (data-root.ts, issue #1788). Checked before
  // the lock below, which is the first thing to write into the root.
  const rootVerdict = dataRootVerdict(
    { platform: process.platform, hasSwitch: (name) => app.commandLine.hasSwitch(name), root: app.getPath('userData'), env: process.env },
    log,
  );
  if (rootVerdict === 'refused') {
    app.exit(1);
    return;
  }
  app.enableSandbox();
  // Screen capture goes through Chromium's PipeWire capturer, and so the
  // screen-cast portal's own picker, alone (Q-D4-4 on #1578). The switch is
  // read when Chromium starts its capturers, so it is set before ready.
  // Chromium uses that capturer only under Wayland; portal.ts says Share is
  // unavailable anywhere else.
  app.commandLine.appendSwitch('enable-features', 'WebRTCPipeWireCapturer');
  const live = new Live();
  // What the person has just done, and the one way out of the window: a link
  // they clicked, validated, to the OS browser (issue #947). Both read the
  // window of the activation in force.
  const owned = () => live.owned();
  const parent = () => (live.window === null || live.window.isDestroyed() ? null : live.window);
  const gestures = new Gestures(Date.now);
  const links = new Links({
    gestures,
    owned,
    openExternal: (url) => shell.openExternal(url),
    confirm: async (host, url) => {
      const window = parent();
      const options = {
        type: 'question' as const,
        message: 'Open this link in your browser?',
        detail: `${host}\n\n${url}`,
        buttons: ['Open', 'Cancel'],
        defaultId: 1,
        cancelId: 1,
        noLink: true,
      };
      const answer = await (window === null ? dialog.showMessageBox(options) : dialog.showMessageBox(window, options));
      return answer.response === 0;
    },
    tell: (message) => {
      const window = parent();
      const options = { type: 'warning' as const, message: 'Conch did not open that link', detail: message, buttons: ['OK'] };
      void (window === null ? dialog.showMessageBox(options) : dialog.showMessageBox(window, options));
    },
    log,
  });
  // Whether this desktop can share a screen (portal.ts): asked after ready,
  // and again with every companion grant.
  const portal = new Portal({ platform: process.platform, env: process.env, run: runFile, executable, log });
  // The LiveKit origin of the live companion grant: none until the companion
  // exists (below), and then its own, read afresh on every request.
  let liveOrigin = (): string | null => null;
  // The share allowance (share-allowance.ts): the Share click's act, spent
  // once on the grant and once on the capture. The gate it reads is the
  // newest activation's.
  const share = new ShareAllowance({ now: Date.now, generation: () => live.gate.generation });
  // The share sound's word that a capture was granted (issue #1854): nothing
  // until the share sound exists (below, with main's switch on).
  let captureGranted = (): void => undefined;
  const capture = new DisplayCapture({
    owned: () => (live.window === null || live.window.isDestroyed() ? null : live.window.webContents),
    gate: () => live.gate,
    share,
    live: () => liveOrigin() !== null,
    available: () => portal.last(),
    getSources: (options) => desktopCapturer.getSources(options),
    granted: () => {
      captureGranted();
    },
    log,
  });
  // One set of guard dependencies, for every web contents and every
  // activation's partition alike.
  const guard = { log, gestures, owned, links, liveOrigin: () => liveOrigin(), capture };
  installGuards(app, guard);

  // One instance per data root: the lock comes before anything reads or
  // writes the root. A second launch focuses the first and exits.
  //
  // A conch:// invite link (issue #1592) arrives in a launch's arguments. The
  // invite router does not exist yet at this point, so what a second launch
  // sends before it does is kept (the newest four, newest last) and given to it first
  // thing. The arguments are never logged.
  let argvSink: ((argv: readonly string[]) => void) | null = null;
  const earlyArgv: (readonly string[])[] = [];
  const first = acquire(
    app,
    () => {
      const window = live.window;
      if (window === null || window.isDestroyed()) return;
      if (window.isMinimized()) window.restore();
      // A window hidden by a close comes back.
      window.show();
      window.focus();
    },
    (argv) => {
      if (argvSink !== null) argvSink(argv);
      else keepNewest(earlyArgv, argv, 4);
    },
  );
  if (!first) return;

  // Only the owning process registers the Windows invite handler. Failure
  // leaves pasted invites available; never include argv or a link in the log.
  if (!registerInviteProtocol(app)) log('conch-desktop: could not register the conch: protocol');

  // The app's settings, once the data root is known (below). Until then, and
  // with none, closing the last window quits.
  let settings: SettingsService | null = null;
  // The opt-in tray (#1589): made after voice, below. Until then, and while
  // closeToTray is off, there is none.
  let tray: TrayController | null = null;
  // Set once the app is ending, so that a close is never turned into a hide.
  let quitting = false;
  // Windows whose close has been decided to go ahead.
  const closing = new WeakSet<BrowserWindow>();
  app.on('window-all-closed', () => {
    // A window destroyed for a switch or a sign-out is not the app ending.
    // Otherwise closing quits (media stopped, helper reaped, in before-quit),
    // unless the person chose to keep running and a working tray exists.
    const action = settings === null ? (live.destroying ? 'stay' : 'quit') : settings.closeAction(live.destroying);
    if (action === 'quit') app.quit();
  });

  // Closing hides the window instead only while closeToTray is on and a
  // working tray exists, checked again now (the tray host may have gone since
  // the last check). Otherwise the close goes on and, as the last window,
  // quits (window-all-closed).
  app.on('browser-window-created', (_event, created) => {
    created.on('close', (event) => {
      const t = tray;
      if (quitting || live.destroying || created !== live.window || closing.has(created) || t === null || settings === null || !settings.current.closeToTray) return;
      event.preventDefault();
      // A check that fails lets the close go on: no answer is no tray.
      void t.check().then(
        () => t.working(),
        () => false,
      ).then((working) => {
        if (working && !created.isDestroyed()) {
          created.hide();
          return;
        }
        if (created.isDestroyed()) return;
        closing.add(created);
        created.close();
      });
    });
  });

  await app.whenReady();

  void portal.probe();

  // The first receipt: what this build really is.
  const versions = { app: app.getVersion(), electron: process.versions.electron, chrome: process.versions.chrome, node: process.versions.node, platform: process.platform };
  const versionsLine = `conch-desktop: versions ${JSON.stringify({ electron: versions.electron, chrome: versions.chrome, node: versions.node })}`;
  ring.record(versionsLine);
  console.log(versionsLine);

  const appPath = app.getAppPath();
  const ui = join(appPath, 'out', 'ui');
  const manifest = parseManifest(await readFile(join(appPath, 'out', 'ui-manifest.json'), 'utf8'));

  // The allowlist of server releases this build was tested with (issue #946).
  // One that cannot be read supports nothing. The development flag is honoured
  // only by an unpackaged build, and says so on the log.
  const allowlist = parseCompat(await readFile(join(appPath, 'out', 'compat.json'), 'utf8').catch(() => ''));
  if (allowlist === null) log('conch-desktop: compat.json is missing or malformed; no server release is supported');
  const override = allowUnlisted(app.isPackaged, process.argv);
  if (override) log(`conch-desktop: ${OVERRIDE_FLAG}: servers that are not on the tested list are allowed (development build)`);

  // The data root, private to this user. The vault lives in <root>/vault,
  // which the vault itself creates and nothing else.
  const root = app.getPath('userData');
  const checked = await prepareRoot(root);
  if (!checked.ok) log('conch-desktop: the data directory is not private to you; nothing will be saved');
  const files = createProfileFiles(root);
  const maximized = process.platform === 'win32'
    ? new WindowMaximized(checked.ok ? createSettingsFiles(root, 'window-state.json') : memorySettingsFiles(), log)
    : undefined;
  await maximized?.load();
  // The app-wide settings: <root>/settings.json, in memory only when the root
  // is not private. The tray (below) answers trayWorking; with none, closing
  // always quits.
  settings = new SettingsService({
    files: checked.ok ? createSettingsFiles(root) : memorySettingsFiles(),
    autostart: new Autostart({ packaged: app.isPackaged, execPath: process.execPath, env: process.env, home: app.getPath('home'), log }),
    trayWorking: () => tray?.working() ?? false,
    trayExists: () => tray?.exists ?? false,
    trayDetected: () => trayHostPresent(runFile),
    changed: () => {
      void tray?.sync().catch(() => {
        log('tray: could not follow the setting');
      });
    },
    log,
  });
  await settings.load();
  // Recheck launch at login after loading: the install may have been
  // loosened, or the setting restored on an unsupported platform.
  await settings.rejudge();
  const store = new ProfileStore(
    checked.ok
      ? files
      : {
          // Not private: the list is kept in memory only.
          read: () => Promise.resolve({ kind: 'absent' }),
          write: () => Promise.resolve(),
          quarantine: () => Promise.resolve(),
          ensureProfileDir: () => Promise.resolve(),
          removeProfileDir: () => Promise.resolve(),
        },
  );
  const vault = checked.ok
    ? createVault({ storage: electronStorage(safeStorage), root: join(root, 'vault'), platform: process.platform, isReady: () => app.isReady(), log })
    : refusingVault();

  trustSystemRoots(tls);
  // The request executor of the activation in force, which the screen
  // companion asks conchd with (issue #1579): each activation makes its
  // attachments from its own client, below, so this is always the newest.
  let activeClient: Client | null = null;
  // The attachments of the activation in force: its own client and session.
  const makeFiles = (client: Client, identity: Session): Files => {
    activeClient = client;
    return new Files({
      client,
      session: identity,
      chooseSavePath: async (suggested) => {
        const options = { title: 'Save attachment', defaultPath: suggested, properties: ['showOverwriteConfirmation' as const, 'createDirectory' as const] };
        const window = parent();
        const chosen = await (window === null ? dialog.showSaveDialog(options) : dialog.showSaveDialog(window, options));
        return chosen.canceled || chosen.filePath === '' ? null : chosen.filePath;
      },
      writeFile: saveChosenFile,
      downloadsDir: () => app.getPath('downloads'),
      join,
      isAbsolute,
      log,
    });
  };
  const openActivation = activationFactory(
    {
      packaged: app.isPackaged, appPath, manifest, readUi: (path) => readFile(join(ui, ...path.split('/').slice(1))),
      connect: createNodeConnect(log), guard, makeFiles, now: Date.now, log,
      ...(maximized === undefined ? {} : { maximized }),
    },
    live,
  );
  const profiles = new ProfilesService({
    store,
    vault,
    hooks: teardownHooks,
    createActivation: async (spec) => {
      const activation = await openActivation(spec);
      // The switcher records the activation once this resolves: the menu is
      // filled from it on the next turn, not at the next 10 s check.
      unblankSoon(() => tray, (generation) => profiles.switcher.isCurrent(generation), spec.generation);
      return activation;
    },
    compat: { servers: allowlist?.servers ?? [], override },
    desktopVersion: app.getVersion(),
    readHealth: healthReader(),
    probe: prober(log),
    revokeStored: revoker(log),
    verifyStored: verifier(log),
    platform: process.platform,
    now: Date.now,
    log,
  });

  // Notifications (issue #1590) belong to the activation and document that
  // showed them. The teardown hook also runs on quit; #1588 owns setLocked.
  const notifications = new Notifications({
    Notification,
    icon: join(ui, 'favicon-32.png'),
    window: () => live.window,
    current: () => {
      const activation = profiles.active;
      return activation === null ? null : { activation, gate: live.gate };
    },
  });
  // Media first: close all and refuse new ones, as the old renderer lives on
  // until its web contents are destroyed. The helper phase runs after that, so
  // it closes anything that slipped in and accepts notifications again.
  teardownHooks.media('notifications', () => {
    notifications.beginTeardown();
  });
  teardownHooks.helper('notifications', () => {
    notifications.endTeardown();
  });

  // Native voice (issue #1373): the one owner of the helper in this instance.
  // The helper is found only in the app's own resources directory, which is
  // Electron's, never an environment variable or an argument. Nothing here
  // starts voice: only the person's join and retry do (the IPC, issue #1374,
  // takes them only after a gesture). The
  // media hook leaves and stops the helper before identity changes.
  // The installed files replaced under this running app (issue #1600). The
  // stamp is resources/version, written by the build; it is read at start,
  // when a window gains focus and every ten minutes. Nothing restarts by
  // itself: the page's Restart button relaunches (a gesture-checked IPC), and
  // the relaunch goes through the quit below, so the helper is reaped first.
  // True once the app is quitting (set by before-quit below): a window or
  // renderer going away then is the quit, not a failed page.
  let ending = false;
  // Whether a call is running, said in the notice and the dialog; set once the
  // voice service exists.
  let callActive = (): boolean => false;
  const installed = new Installed({
    read: () => readFile(join(process.resourcesPath, 'version'), 'utf8'),
    running: app.getVersion(),
    every: (fn, ms) => {
      const timer = setInterval(fn, ms);
      timer.unref();
      return () => {
        clearInterval(timer);
      };
    },
    relaunch: () => {
      app.relaunch();
      app.quit();
    },
    ask: async (version) => {
      const options = {
        type: 'info' as const,
        message: updateDialogMessage(version),
        detail: updateDialogDetail(callActive()),
        buttons: [...updateDialogButtons],
        defaultId: 1,
        cancelId: 1,
        noLink: true,
      };
      const window = parent();
      const answer = await (window === null ? dialog.showMessageBox(options) : dialog.showMessageBox(window, options));
      return answer.response === 0;
    },
    log,
  });
  installed.subscribe((view) => {
    deliverUpdate({ contents: () => (live.owned() === null ? null : (live.window?.webContents ?? null)), gate: () => live.gate }, view);
  });
  app.on('browser-window-focus', () => {
    void installed.check();
  });
  // A page that starts after an update is measured; one that does not asks
  // for a restart in a native dialog, since it cannot show the notice itself.
  app.on('web-contents-created', (_event, contents) => {
    contents.on('did-finish-load', () => {
      installed.pageStarted();
    });
    contents.on('did-fail-load', (_e, code, _description, _url, isMainFrame) => {
      if (shouldAskAfterFailure({ kind: 'did-fail-load', code, isMainFrame, ending })) void installed.pageFailed();
    });
    contents.on('render-process-gone', (_e, details) => {
      if (shouldAskAfterFailure({ kind: 'render-process-gone', reason: details.reason, ending })) void installed.pageFailed();
    });
  });
  await installed.start();

  // The Linux screen share's sound (issue #1854): one experimental switch,
  // CONCH_LINUX_SHARE_AUDIO=1 in main's own environment, on Linux, read here
  // once. Off (the default and every release's): the helper is offered the
  // versions it always was, no sound object exists, the view says nothing of
  // sound and no share-sound IPC function answers. Neither the renderer nor a
  // profile can turn it on; the default becomes on only in a later change that
  // cites a passed L-S6 receipt.
  const shareAudio = linuxShareAudioEnabled(process.env, process.platform);
  if (shareAudio) log('conch-desktop: CONCH_LINUX_SHARE_AUDIO is on: the Linux screen share offers its sound (experimental)');
  // Set once Lifecycle exists (below); voice and the companion read it.
  let screenLocked: () => boolean = () => false;
  let unmuteBlocked: () => boolean = () => true;
  const hasMuteKey = process.platform === 'linux' || process.platform === 'win32';
  const voice = new VoiceService({
    locked: () => screenLocked() || (hasMuteKey && unmuteBlocked()),
    supervisor: new Supervisor({ resources: process.resourcesPath, env: process.env, log, ...(shareAudio ? { versions: OWNER_VERSIONS_SHARE_AUDIO } : {}) }),
    ...(hasMuteKey ? {
      muteKey: {
        blocked: () => unmuteBlocked(),
        watch: (configDir: string, onPress: () => void) => watchMuteKey(process.resourcesPath, process.env, log, configDir, onPress),
      },
    } : {}),
    // The device list and the saves run the same trusted helper, with no token.
    oneShot: new OneShot({ resources: process.resourcesPath, env: process.env, log }),
    identity: async () => {
      const activation = profiles.active;
      const id = activation?.profileId ?? null;
      if (activation === null || id === null || !checked.ok) return null;
      const principal = store.get(id)?.principal ?? null;
      if (principal === null) return null;
      try {
        await files.ensureProfileDir(id);
      } catch {
        return null;
      }
      return createVoiceIdentity(activation, () => profiles.active === activation, {
        profileId: id,
        configDir: profileConfigDir(root, id),
        principalId: principal.id,
      });
    },
    updated: () => installed.updated(),
    recheck: () => installed.check(),
    log,
  });
  callActive = () => voice.state().status === 'running';
  // Main's share sound, made below once the companion exists; the companion
  // tells it of every end of a share (its soundEnded).
  let sound: ShareSound | null = null;
  // The screen companion (issue #1579, D4-02 of #772): the one holder of the
  // p<id>.screen grant. Its media hook is registered before voice's, so a
  // switch, sign-out or quit tells the page and removes the companion while
  // the helper and the credential are still in force. A crash or a reload of
  // the owned window's page ends the document the grant was given to.
  const companion = new Companion({
    voice,
    locked: () => screenLocked(),
    client: () => activeClient,
    document: () => live.gate.generation,
    // Any end or stop clears the share allowance, held or not.
    ended: () => {
      share.clear();
    },
    tell: (document, event) => {
      const contents = live.owned() === null ? null : (live.window?.webContents ?? null);
      if (contents !== null && live.gate.shows(document)) contents.send(COMPANION_EVENTS, event);
    },
    // Asked again with every grant: a portal started since is found.
    share: async () => {
      const answer = await portal.probe();
      return answer === 'available' ? null : answer;
    },
    // The owned window's session is asked to close its connections when the
    // grant ends. That reaches idle and pending sockets only: a live
    // WebSocket survives closeAllConnections (S2 of #1621). What ends it is
    // conchd's DELETE; when that is not confirmed the document is reloaded.
    closeConnections: async () => {
      const w = live.window;
      if (w !== null && !w.isDestroyed()) await w.webContents.session.closeAllConnections();
    },
    contents: () => live.owned(),
    // Only the contents and the document that held the grant (S2 of #1621): a
    // switch to another profile, or a new document, inside the removal's wait
    // is not reloaded.
    reload: (contents, document) => {
      const w = live.window;
      if (w === null || w.isDestroyed() || !stillHeldBy({ contents: w.webContents, generation: live.gate.generation }, { contents, document })) return;
      w.webContents.reload();
    },
    // The grant is issued: the share allowance's capture window starts here.
    granted: (contents, document) => {
      share.confirmGrant(contents, document);
    },
    // The id of the share's channel the sound learned from conchd, for the
    // companion's own DELETE after a rename (issue #1881). None with the
    // switch off.
    channelId: (held) => sound?.channelId(held) ?? null,
    // Every end and stop of a held share: the sound's helper stop, then its
    // consent's DELETE. Nothing with the switch off.
    soundEnded: (held, reason) => sound?.ended(held, reason) ?? Promise.resolve(),
    log,
  });
  liveOrigin = () => companion.liveOrigin();
  if (shareAudio) {
    const made = new ShareSound({
      voice,
      companion,
      // The choice is the active profile's own, kept by main; the page only
      // ever asks to change it, after a click.
      choice: () => profiles.shareSoundChoice(),
      setChoice: (choice) => profiles.setShareSoundChoice(choice),
      // The name this app's own audio streams carry, from the app's identity.
      applicationName: registeredApplicationName(app.name),
      log,
    });
    sound = made;
    // A granted capture is the share's, in the document the gate shows now.
    captureGranted = () => {
      made.captureGranted(live.gate.generation);
    };
    voice.attachShareSound({ view: () => made.view(), leaving: () => made.leaving() });
  }
  teardownHooks.media(
    'share',
    shareTeardown({
      dropClient: () => {
        activeClient = null;
      },
      companion,
      ...(sound === null ? {} : { sound }),
    }),
  );
  app.on('render-process-gone', (_event, contents) => {
    if (contents === live.owned()) void companion.end('crash');
  });
  app.on('web-contents-created', (_event, contents) => {
    contents.on('did-start-navigation', (details) => {
      if (details.isMainFrame && !details.isSameDocument && contents === live.owned()) void companion.end('reload');
    });
  });
  teardownHooks.media('voice', () => voice.teardown());

  // Lock and suspend fail closed (issue #1588, D5-02): mute on lock, leave on
  // suspend, end the share, hide notification text. Linux observes the lock
  // through logind; Windows through powerMonitor (#1935). Where it cannot be, the
  // rules keep notification text hidden and mute voice on observation loss.
  //
  // The companion is the real one (issue #1579, above): lock ends a live share
  // with reason 'lock' and suspend with 'suspend'.
  const lifecycle = new Lifecycle({
    voice,
    companion,
    notifications,
    log,
  });
  screenLocked = () => lifecycle.locked;
  unmuteBlocked = () => lifecycle.sleeping || lifecycle.lockObservation === 'unavailable';
  let watcher: LinuxLifecycle | WindowsLifecycle | null = null;
  if (process.platform === 'linux') {
    watcher = new LinuxLifecycle({ rules: lifecycle, powerMonitor, pid: process.pid, log });
    void watcher.start();
  } else if (process.platform === 'win32') {
    watcher = new WindowsLifecycle({ rules: lifecycle, powerMonitor });
    watcher.start();
  }
  // Every change of the helper's confirmed state goes to the owned window.
  voice.subscribe((view) => {
    deliverVoice({ contents: () => (live.owned() === null ? null : (live.window?.webContents ?? null)), gate: () => live.gate }, view);
  });

  // The one registry of IPC handlers, reading the newest activation's parts.
  // Nothing is registered before a window exists to be checked against.
  const current = <T>(part: T | null): T => {
    if (part === null) throw new Error('no activation');
    return part;
  };
  // Opening an invite link (issue #1591): parses in main, keeps the code
  // behind a handle, and switches only through the profiles service.
  const invites = new Invites({
    profiles,
    voiceRunning: () => {
      const { status } = voice.state();
      return status === 'starting' || status === 'running';
    },
    now: Date.now,
    log,
  });
  // A scheme link in any launch's arguments, this one's or a later one's, is
  // checked by the router and the page told; the page asks for the result
  // and shows the choice for a valid link or a message for a refusal. A link
  // arriving while a sign-in or switch is under way waits for the page.
  argvSink = (argv) => {
    if (invites.arrive(argv)) deliverInvite({ contents: () => (live.owned() === null ? null : (live.window?.webContents ?? null)), gate: () => live.gate });
  };
  for (const argv of [process.argv, ...earlyArgv]) argvSink(argv);
  earlyArgv.length = 0;
  const ipc = new Ipc({
    owned: () => live.owned(),
    get gate() {
      return live.gate;
    },
    get service() {
      return current(live.service);
    },
    get feeds() {
      return current(live.feeds);
    },
    get files() {
      return current(live.files);
    },
    gestures,
    share,
    profiles,
    voice,
    installed,
    invites,
    notifications,
    settings,
    integrations: {
      view: () =>
        integrationsView({
          settings: () => settingsService.view(),
          notificationSupported: () => notifications.supported(),
          lockObservation: () => lifecycle.lockObservation,
          lockTrying: () => watcher instanceof LinuxLifecycle && watcher.trying,
          share: () => portal.last(),
          storage: () => profiles.view().storage,
          voice: () => voice.state(),
        }),
    },
    diagnostics: new Diagnostics({
      ring,
      versions,
      now: () => new Date(),
      chooseSavePath: async (suggested) => {
        const options = { title: 'Save diagnostics', defaultPath: suggested, properties: ['showOverwriteConfirmation' as const, 'createDirectory' as const] };
        const window = parent();
        const chosen = await (window === null ? dialog.showSaveDialog(options) : dialog.showSaveDialog(window, options));
        return chosen.canceled || chosen.filePath === '' ? null : chosen.filePath;
      },
      writeFile: saveChosenFile,
      downloadsDir: () => app.getPath('downloads'),
      join,
      isAbsolute,
      log,
    }),
    companion,
    ...(sound === null ? {} : { shareSound: sound }),
    now: Date.now,
    log,
  });
  ipc.register(ipcMain);
  ipc.registerNotificationSupport(ipcMain);

  // The tray (issue #1589, D5-03). Its Quit is app.quit(): the same
  // before-quit teardown as the window's own (helper reaped, the companion's
  // DELETE sent by the media hooks).
  const settingsService = settings;
  const trayController = new TrayController({
    createTray: (icon) => new Tray(icon),
    buildMenu: (items) => Menu.buildFromTemplate([...items]),
    iconDir: join(appPath, 'build', 'icons', 'tray'),
    // The filled app icon is legible on both light and dark Windows taskbars.
    ...(process.platform === 'win32' ? { idleIcon: join(appPath, 'build', 'icons', '48.png') } : {}),
    voice,
    sharing: () => companion.liveOrigin() !== null,
    endCompanion: () => companion.end('lock'),
    profile: () => {
      const id = profiles.active?.profileId ?? null;
      return id === null ? null : (store.get(id)?.label ?? null);
    },
    show: () => {
      const window = live.window;
      if (window === null || window.isDestroyed()) return;
      if (window.isMinimized()) window.restore();
      window.show();
      window.focus();
    },
    hidden: () => live.window !== null && !live.window.isDestroyed() && !live.window.isVisible(),
    quit: () => {
      app.quit();
    },
    detect: () => trayHostPresent(runFile),
    revoke: () => {
      settingsService.revokeTrayConfirmation();
    },
    settings: () => settingsService.current,
    confirmed: () => settingsService.trayConfirmed,
    log,
  });
  tray = trayController;
  // Media ends for a switch or a sign-out: the menu shows nothing of the old
  // profile until it is rebuilt from the activation in force.
  teardownHooks.media('tray', () => {
    trayController.blank();
  });
  // The strip shows "Sharing your screen" as the companion starts and ends.
  companion.onChange(() => {
    trayController.refresh();
  });
  void trayController.sync().catch(() => {
    log('tray: could not start');
  });

  app.on('before-quit', (event) => {
    quitting = true;
    if (ending) return;
    ending = true;
    event.preventDefault();
    installed.stop();
    // Stop power listeners and, on Linux, the monitor child by its own handle.
    watcher?.stop();
    // Media stops before the process goes.
    void profiles.close().finally(async () => {
      await maximized?.flush();
      app.quit();
    });
  });

  await profiles.start();
}
