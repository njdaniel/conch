// What a web contents may not do (linux-desktop.md §3, §10). Installed on
// EVERY web contents the app creates, not only the main window's: a window the
// page managed to open would otherwise start without them.
//
// Input: a web contents (and its session) from Electron. Output: handlers
// that refuse. This module owns the refusals; D2-8 adds the one way out (a
// validated link opened in the OS browser), and D4-03 (#1580) the companion's
// two: HTTP and WebSocket requests to exactly the live grant's LiveKit origin,
// and one screen capture by the rules of display-capture.ts. WebRTC (ICE,
// STUN, TURN) and WebTransport are not requests this module sees.
import type { App, Event, Session, WebContents } from 'electron';

import { displayPermission, type DisplayCapture } from './display-capture.ts';
import { CLIPBOARD_GESTURE_MS, type Gestures } from './gestures.ts';
import type { Links } from './links.ts';
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

// livekitAllowed says whether a request is to the live companion grant's
// LiveKit origin (linux-desktop.md §3 as amended by Q-D4-1 on #1578: the CSP
// allows `wss: https:`, and this filter lets through exactly that origin while
// the grant lives). Exactly: wss or https, the same host and the same port
// (each scheme's default being 443, wss://h and https://h:443 are one), no
// user information. ws: and http: never pass, so an origin conchd named over
// plain ws to loopback (development) passes nothing either; nor does any
// origin when no grant is live.
export function livekitAllowed(url: string, origin: string | null): boolean {
  if (origin === null) return false;
  let live: URL;
  let asked: URL;
  try {
    live = new URL(origin);
    asked = new URL(url);
  } catch {
    return false;
  }
  const secure = (u: URL): boolean => u.protocol === 'wss:' || u.protocol === 'https:';
  if (!secure(live) || !secure(asked)) return false;
  if (asked.username !== '' || asked.password !== '' || live.username !== '' || live.password !== '') return false;
  const port = (u: URL): string => (u.port === '' ? '443' : u.port);
  return asked.hostname !== '' && asked.hostname === live.hostname && port(asked) === port(live);
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
  // What lets a clipboard write through and a link out (issue #947). Without
  // them (a unit test of the refusals alone) nothing is let through.
  readonly gestures?: Gestures;
  // The one web contents that is the app's window.
  readonly owned?: () => object | null;
  readonly links?: Pick<Links, 'fromWindow'>;
  // The LiveKit origin of the live companion grant, or null (#1580:
  // companion.liveOrigin). Without it no request leaves the app's scheme.
  readonly liveOrigin?: () => string | null;
  // The rules of the one screen capture (display-capture.ts). Without them
  // display capture is refused outright.
  readonly capture?: Pick<DisplayCapture, 'permits' | 'allow' | 'handle'>;
}

// clipboardWriteAllowed is the one permission the page is given: writing
// plain text to the clipboard (clipboard-sanitized-write), from the owned
// window's main frame at the app's own origin, right after the person's own
// input. clipboard-read is never given; paste reaches the page as a paste
// event.
export function clipboardWriteAllowed(
  deps: GuardDeps,
  contents: object | null,
  permission: string,
  details: { readonly isMainFrame?: boolean; readonly requestingUrl?: string },
): boolean {
  if (permission !== 'clipboard-sanitized-write' || contents === null) return false;
  if (deps.owned === undefined || deps.gestures === undefined || contents !== deps.owned()) return false;
  if (details.isMainFrame !== true || !(details.requestingUrl ?? '').startsWith(ALLOWED_PREFIX)) return false;
  return deps.gestures.recent(contents, CLIPBOARD_GESTURE_MS);
}

// guardSession denies, on a session, what a page might ask of the host: every
// permission (the permission handlers answer for check and request alike),
// display capture, device choosers, downloads, and any request that is not to
// the app's own scheme. Idempotent in effect: handlers are replaced. Two
// exceptions, each with its own conditions: the permission request and the
// display-media request of one screen capture (display-capture.ts), and
// requests to the live companion grant's LiveKit origin (livekitAllowed).
// Microphone and camera stay denied, and no permission check is ever
// answered yes but the clipboard write's: getDisplayMedia makes none.
export function guardSession(ses: Session, deps: GuardDeps): void {
  ses.setPermissionRequestHandler((contents, permission, callback, details) => {
    const capture = deps.capture;
    if (capture !== undefined && displayPermission(permission, details)) {
      if (capture.permits(contents, details)) {
        capture.allow(contents, callback);
        return;
      }
      callback(false);
      return;
    }
    callback(clipboardWriteAllowed(deps, contents, permission, details));
  });
  ses.setPermissionCheckHandler((contents, permission, _origin, details) => clipboardWriteAllowed(deps, contents, permission, details));
  ses.setDevicePermissionHandler(() => false);
  ses.setDisplayMediaRequestHandler((request, callback) => {
    if (deps.capture === undefined) {
      callback(null);
      return;
    }
    deps.capture.handle(request, (streams) => {
      callback(streams === null ? null : { video: streams.video });
    });
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
  // The window makes no HTTP or WebSocket request that is not to
  // conch-app://ui/, but to the live companion grant's LiveKit origin. The CSP's connect-src (`wss:
  // https:`) lets any secure origin as far as here, so for those this is the
  // boundary, read afresh on every request: an end, a sign-out or a switch
  // forgets the grant first, and the next request is refused. It is also the
  // observer the smoke test reads: a refusal is one stderr line. WebRTC
  // (ICE, STUN, TURN) and WebTransport never reach onBeforeRequest: they are
  // outside this filter and the CSP's connect-src alike.
  ses.webRequest.onBeforeRequest((details, callback) => {
    if (foreign(details.url) && !livekitAllowed(details.url, deps.liveOrigin?.() ?? null)) {
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
export function guardContents(contents: WebContents, links?: Pick<Links, 'fromWindow'>): void {
  const refuse = (details: NavigationDetails): void => {
    if (details.url !== INITIAL_URL) details.preventDefault();
  };
  contents.on('will-navigate', refuse);
  contents.on('will-redirect', refuse);
  contents.on('will-frame-navigate', refuse);
  // No window is ever made. A link the person clicked, that opens a new
  // window, goes to the OS browser instead, after main's own checks.
  contents.setWindowOpenHandler((details) => {
    links?.fromWindow(contents, details.url, details.postBody !== undefined);
    return { action: 'deny' };
  });
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
    deps.gestures?.attach(contents);
    guardContents(contents, deps.links);
  });
}
