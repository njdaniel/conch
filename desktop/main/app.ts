// The shell's startup (linux-desktop.md §2, §3). Input: Electron's app events.
// Output: one hardened window showing the packaged web client, and nothing
// else but main's public-API client behind the bridge's IPC handlers (D2-3)
// and the live feeds main holds for the page (D2-4). The renderer itself
// still makes no network request.
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';

import tls from 'node:tls';

import { app, ipcMain, protocol, safeStorage } from 'electron';

import { allowUnlisted, OVERRIDE_FLAG, parseCompat } from './compat.ts';
import { activationFactory, healthReader, Live, prober, revoker, verifier } from './profiles/activation.ts';
import { createProfileFiles } from './profiles/files.ts';
import { TeardownHooks } from './profiles/hooks.ts';
import { ProfilesService } from './profiles/service.ts';
import { ProfileStore } from './profiles/store.ts';
import { acquire, prepareRoot } from './instance.ts';
import { createNodeConnect } from './api/socket.ts';
import { transportVerdict, trustSystemRoots } from './api/trust.ts';
import { debuggingVerdict } from './debugging.ts';
import { installGuards } from './guards.ts';
import { Ipc } from './ipc.ts';
import { parseManifest, SCHEME, SCHEME_PRIVILEGES } from './scheme.ts';
import { electronStorage } from './vault/electron-storage.ts';
import { createVault, type Vault } from './vault/vault.ts';

// The hooks #771 (native voice) and #772 (share) register their teardown on:
// run first on every switch and sign-out (profiles/switch.ts).
export const teardownHooks = new TeardownHooks();

// registerScheme must run before the app is ready.
export function registerScheme(): void {
  protocol.registerSchemesAsPrivileged([{ scheme: SCHEME, privileges: { ...SCHEME_PRIVILEGES } }]);
}

function log(line: string): void {
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
  app.enableSandbox();
  installGuards(app, { log });

  const live = new Live();
  // One instance per data root: the lock comes before anything reads or
  // writes the root. A second launch focuses the first and exits.
  const first = acquire(app, () => {
    const window = live.window;
    if (window === null || window.isDestroyed()) return;
    if (window.isMinimized()) window.restore();
    window.focus();
  });
  if (!first) return;

  app.on('window-all-closed', () => {
    // A window destroyed for a switch or a sign-out is not the app ending.
    if (!live.destroying) app.quit();
  });

  await app.whenReady();

  // The first receipt: what this build really is.
  console.log(`conch-desktop: versions ${JSON.stringify({ electron: process.versions.electron, chrome: process.versions.chrome, node: process.versions.node })}`);

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
  const profiles = new ProfilesService({
    store,
    vault,
    hooks: teardownHooks,
    createActivation: activationFactory(
      { packaged: app.isPackaged, appPath, manifest, readUi: (path) => readFile(join(ui, ...path.split('/').slice(1))), connect: createNodeConnect(log), now: Date.now, log },
      live,
    ),
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

  // The one registry of IPC handlers, reading the newest activation's parts.
  // Nothing is registered before a window exists to be checked against.
  const current = <T>(part: T | null): T => {
    if (part === null) throw new Error('no activation');
    return part;
  };
  new Ipc({
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
    profiles,
    now: Date.now,
    log,
  }).register(ipcMain);

  let ending = false;
  app.on('before-quit', (event) => {
    if (ending) return;
    ending = true;
    event.preventDefault();
    // Media stops before the process goes.
    void profiles.close().finally(() => {
      app.quit();
    });
  });

  await profiles.start();
}
