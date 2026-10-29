// The shell's startup (linux-desktop.md §2, §3). Input: Electron's app events.
// Output: one hardened window showing the packaged web client, and nothing
// else: no network, no authentication, no IPC method (D2-3 adds those).
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';

import { app, protocol, session } from 'electron';

import { debuggingVerdict } from './debugging.ts';
import { installGuards, guardSession } from './guards.ts';
import { handle, parseManifest, SCHEME, SCHEME_PRIVILEGES } from './scheme.ts';
import { createWindow, PARTITION } from './window.ts';

// registerScheme must run before the app is ready.
export function registerScheme(): void {
  protocol.registerSchemesAsPrivileged([{ scheme: SCHEME, privileges: { ...SCHEME_PRIVILEGES } }]);
}

function log(line: string): void {
  console.error(line);
}

export async function start(): Promise<void> {
  if (debuggingVerdict(app.isPackaged, (name) => app.commandLine.hasSwitch(name), process.env, log) === 'refused') {
    app.exit(1);
    return;
  }
  app.enableSandbox();
  installGuards(app, { log });
  app.on('window-all-closed', () => {
    app.quit();
  });

  await app.whenReady();

  // The first receipt: what this build really is.
  console.log(`conch-desktop: versions ${JSON.stringify({ electron: process.versions.electron, chrome: process.versions.chrome, node: process.versions.node })}`);

  const appPath = app.getAppPath();
  const ui = join(appPath, 'out', 'ui');
  const manifest = parseManifest(await readFile(join(appPath, 'out', 'ui-manifest.json'), 'utf8'));

  const ses = session.fromPartition(PARTITION);
  guardSession(ses, { log });
  ses.protocol.handle(SCHEME, (request) =>
    handle(request, { manifest, readFile: (path) => readFile(join(ui, ...path.split('/').slice(1))), log }),
  );

  createWindow(app.isPackaged, appPath);
}
