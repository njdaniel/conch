// What a web contents may not do (linux-desktop.md §3, §10). Installed on
// EVERY web contents the app creates, not only the main window's: a window the
// page managed to open would otherwise start without them.
//
// Input: a web contents (and its session) from Electron. Output: handlers
// that refuse. This module owns the refusals; D2-8 adds the one way out (a
// validated link opened in the OS browser).
import type { App, Event, Session, WebContents } from 'electron';

import { INITIAL_URL, SCHEME, HOST } from './scheme.ts';

interface NavigationDetails {
  readonly url: string;
  preventDefault(): void;
}

const ALLOWED_PREFIX = `${SCHEME}://${HOST}/`;

// foreign says whether a request URL is one the window may make: its own
// packaged files, and data: and blob: for images the page builds itself.
export function foreign(url: string): boolean {
  return !(url.startsWith(ALLOWED_PREFIX) || url.startsWith('data:') || url.startsWith('blob:'));
}

// describe names a refused URL without its path or query: scheme and host.
function describe(url: string): string {
  try {
    const u = new URL(url);
    return `${u.protocol}//${u.host}`;
  } catch {
    return '(unparsable)';
  }
}

export interface GuardDeps {
  readonly log: (line: string) => void;
}

// guardSession denies, on a session, what a page might ask of the host: every
// permission (the permission handlers answer for check and request alike),
// display capture, device choosers, downloads, and any request that is not to
// the app's own scheme. Idempotent in effect: handlers are replaced.
export function guardSession(ses: Session, deps: GuardDeps): void {
  ses.setPermissionRequestHandler((_contents, _permission, callback) => {
    callback(false);
  });
  ses.setPermissionCheckHandler(() => false);
  ses.setDevicePermissionHandler(() => false);
  ses.setDisplayMediaRequestHandler((_request, callback) => {
    callback(null);
  });
  ses.on('select-hid-device', (event, _details, callback) => {
    event.preventDefault();
    callback();
  });
  ses.on('select-usb-device', (event, _details, callback) => {
    event.preventDefault();
    callback();
  });
  ses.on('select-serial-port', (event, _ports, _contents, callback) => {
    event.preventDefault();
    callback('');
  });
  ses.on('will-download', (_event, item) => {
    item.cancel();
  });
  ses.setSpellCheckerEnabled(false);
  // The window makes no request that is not to conch-app://ui/. This is a
  // backstop (the CSP's connect-src 'none' and the navigation guards come
  // first) and the observer the smoke test reads: a refusal is one stderr line.
  ses.webRequest.onBeforeRequest((details, callback) => {
    if (foreign(details.url)) {
      deps.log(`conch-desktop: blocked a request to ${describe(details.url)}`);
      callback({ cancel: true });
      return;
    }
    callback({});
  });
}

// guardContents refuses, on one web contents: navigation away from the
// initial document (including redirects and frame navigation), every new
// window, every webview, and bluetooth choosers.
export function guardContents(contents: WebContents): void {
  const refuse = (details: NavigationDetails): void => {
    if (details.url !== INITIAL_URL) details.preventDefault();
  };
  contents.on('will-navigate', refuse);
  contents.on('will-redirect', refuse);
  contents.on('will-frame-navigate', refuse);
  contents.setWindowOpenHandler(() => ({ action: 'deny' }));
  contents.on('will-attach-webview', (event: Event) => {
    event.preventDefault();
  });
  contents.on('select-bluetooth-device', (event, _devices, callback) => {
    event.preventDefault();
    callback('');
  });
}

export function installGuards(app: App, deps: GuardDeps): void {
  app.on('web-contents-created', (_event, contents) => {
    guardSession(contents.session, deps);
    guardContents(contents);
  });
}
