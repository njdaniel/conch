// The shell's startup (linux-desktop.md §2, §3). Input: Electron's app events.
// Output: one hardened window showing the packaged web client, and nothing
// else but main's public-API client behind the bridge's IPC handlers (D2-3)
// and the live feeds main holds for the page (D2-4). The renderer itself
// still makes no network request.
import { readFile } from 'node:fs/promises';
import { isAbsolute, join } from 'node:path';

import tls from 'node:tls';

import { app, dialog, ipcMain, protocol, session, shell, type BrowserWindow } from 'electron';

import { FEED_EVENTS } from '../shared/bridge-names.ts';
import { Client } from './api/client.ts';
import { Feeds } from './api/feeds.ts';
import { Service } from './api/service.ts';
import { Session } from './api/session.ts';
import { createNodeConnect } from './api/socket.ts';
import { transportVerdict, trustSystemRoots } from './api/trust.ts';
import { debuggingVerdict } from './debugging.ts';
import { DocumentGate } from './document.ts';
import { Files } from './files.ts';
import { Gestures } from './gestures.ts';
import { installGuards, guardSession } from './guards.ts';
import { Ipc } from './ipc.ts';
import { saveChosenFile } from './save-file.ts';
import { Links } from './links.ts';
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
  if (transportVerdict(process.env, log) === 'refused') {
    app.exit(1);
    return;
  }
  app.enableSandbox();
  let window: BrowserWindow | null = null;
  const owned = () => (window === null || window.isDestroyed() ? null : window.webContents);
  // What the person has just done, and the one way out of the window: a link
  // they clicked, validated, to the OS browser (issue #947).
  const gestures = new Gestures(Date.now);
  const links = new Links({
    gestures,
    owned,
    openExternal: (url) => shell.openExternal(url),
    confirm: async (host, url) => {
      const parent = window === null || window.isDestroyed() ? null : window;
      const options = {
        type: 'question' as const,
        message: 'Open this link in your browser?',
        detail: `${host}\n\n${url}`,
        buttons: ['Open', 'Cancel'],
        defaultId: 1,
        cancelId: 1,
        noLink: true,
      };
      const answer = await (parent === null ? dialog.showMessageBox(options) : dialog.showMessageBox(parent, options));
      return answer.response === 0;
    },
    tell: (message) => {
      const parent = window === null || window.isDestroyed() ? null : window;
      const options = { type: 'warning' as const, message: 'Conch did not open that link', detail: message, buttons: ['OK'] };
      void (parent === null ? dialog.showMessageBox(options) : dialog.showMessageBox(parent, options));
    },
    log,
  });
  installGuards(app, { log, gestures, owned, links });
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

  // Main's public-API client and the one registry of IPC handlers. Nothing is
  // registered before the window exists to be checked against.
  trustSystemRoots(tls);
  const identity = new Session();
  const client = new Client({ fetch: globalThis.fetch, session: identity });
  const service = new Service({ client, session: identity, now: Date.now, log });
  const gate = new DocumentGate();
  // A feed event goes to the owned web contents, and only while the document
  // that subscribed is the one it shows.
  const feeds = new Feeds({
    session: identity,
    client,
    connect: createNodeConnect(log),
    deliver: (document, event) => {
      const contents = owned();
      if (contents !== null && gate.shows(document)) contents.send(FEED_EVENTS, event);
    },
    log,
  });
  gate.onEnd((current) => {
    feeds.endDocument(current);
  });
  const files = new Files({
    client,
    session: identity,
    chooseSavePath: async (suggested) => {
      const options = { title: 'Save attachment', defaultPath: suggested, properties: ['showOverwriteConfirmation' as const, 'createDirectory' as const] };
      const parent = window === null || window.isDestroyed() ? null : window;
      const chosen = await (parent === null ? dialog.showSaveDialog(options) : dialog.showSaveDialog(parent, options));
      return chosen.canceled || chosen.filePath === '' ? null : chosen.filePath;
    },
    writeFile: saveChosenFile,
    downloadsDir: () => app.getPath('downloads'),
    join,
    isAbsolute,
    log,
  });
  new Ipc({ owned, gate, service, feeds, files, gestures, now: Date.now, log }).register(ipcMain);
  window = createWindow(app.isPackaged, appPath, gate);
}
