// Desktop notifications (issue #1590, D5-04; linux-desktop.md §4). Main owns
// each native notification and the activation and document it belongs to. A
// click may reach only that same committed document; sign-out, profile switch,
// quit and lock close the notifications that could expose its message text.
import { NOTIFICATION_EVENTS, type NotificationEvent } from '../shared/bridge-names.ts';
import type { NotifyRequest } from '../../web/src/platform/desktop/bridge.ts';
import type { DocumentGate } from './document.ts';

// The fixed native fields; a tag and channel stay in main's ownership record.
export interface NotificationOptions {
  readonly title: string;
  readonly body: string;
  readonly icon: string;
}

// The part of Electron's Notification the registry uses.
export interface NotificationLike {
  on(event: 'click' | 'close', listener: () => void): unknown;
  show(): void;
  close(): void;
}

// A constructible Notification with Electron's support check.
export interface NotificationConstructor {
  new (options: NotificationOptions): NotificationLike;
  isSupported(): boolean;
}

interface WindowLike {
  readonly webContents: {
    send(channel: string, event: NotificationEvent): void;
  };
  isDestroyed(): boolean;
  isMinimized(): boolean;
  restore(): void;
  show(): void;
  focus(): void;
}

interface CurrentActivation {
  readonly activation: object;
  readonly gate: Pick<DocumentGate, 'shows' | 'onEnd'>;
}

// The native API and current window/activation, supplied by the app or fakes.
export interface NotificationsDeps {
  readonly Notification: NotificationConstructor;
  readonly icon: string;
  readonly window: () => WindowLike | null;
  readonly current: () => CurrentActivation | null;
}

// The most notifications main keeps open at once; past it the oldest closes.
const MAX_ENTRIES = 50;

// The fixed words of a locked notification: no channel, no message text.
const LOCKED_TITLE = 'Conch';
const LOCKED_BODY = 'New message';

// Electron 44 hands title and body to libnotify unescaped, and a freedesktop
// server that advertises body-markup, body-hyperlinks or body-images renders
// <b>, <a href> and <img src> in the body (the daemon then opens a link itself,
// past main's link checks). Main cannot read the server's capabilities, so it
// neutralises unconditionally. Entities (&lt; &amp;) would show literally on a
// server without body-markup, so the three markup characters become visible
// look-alikes that no server parses: single angle quotation marks and the
// fullwidth ampersand. Written as escapes so no source character is invisible.
function plain(text: string): string {
  return text.replaceAll('<', '\u2039').replaceAll('>', '\u203A').replaceAll('&', '\uFF06');
}

interface Entry {
  readonly activation: object;
  readonly document: number;
  readonly tag: string;
  readonly channel: string;
}

// Owns the native notifications of every live activation, with no content logging.
export class Notifications {
  readonly #deps: NotificationsDeps;
  readonly #entries = new Map<NotificationLike, Entry>();
  readonly #watched = new WeakSet<object>();
  #locked = false;
  #tearingDown = false;

  constructor(deps: NotificationsDeps) {
    this.#deps = deps;
  }

  // Whether the OS supports Electron notifications.
  supported(): boolean {
    return this.#deps.Notification.isSupported();
  }

  // Lock also closes existing notifications: their bodies cannot be redacted in place.
  setLocked(locked: boolean): void {
    this.#locked = locked;
    if (locked) this.#closeWhere(() => true);
  }

  // Teardown begins: close everything (one activation is live at a time) and
  // refuse new notifications, because the old renderer can still ask for one
  // until its web contents are destroyed.
  beginTeardown(): void {
    this.#tearingDown = true;
    this.closeAll();
  }

  // The old renderer is gone: close what slipped in meanwhile and accept again.
  endTeardown(): void {
    this.closeAll();
    this.#tearingDown = false;
  }

  // Closes every notification; only one activation is ever live.
  closeAll(): void {
    this.#closeWhere(() => true);
  }

  // Shows one notification owned by this activation and document generation.
  show(activation: object | null, document: number, request: NotifyRequest): boolean {
    if (activation === null || this.#tearingDown || !this.supported()) return false;
    this.#watch(activation);
    const notification = new this.#deps.Notification({
      title: this.#locked ? LOCKED_TITLE : plain(request.title),
      body: this.#locked ? LOCKED_BODY : plain(request.body),
      icon: this.#deps.icon,
    });
    const entry: Entry = { activation, document, tag: request.tag, channel: request.channel };
    this.#entries.set(notification, entry);
    // A Map iterates in insertion order, so the first key is the oldest.
    while (this.#entries.size > MAX_ENTRIES) {
      const oldest = this.#entries.keys().next().value;
      if (oldest === undefined) break;
      this.#entries.delete(oldest);
      oldest.close();
    }
    notification.on('close', () => {
      if (this.#entries.get(notification) === entry) this.#entries.delete(notification);
    });
    notification.on('click', () => {
      this.#click(notification);
    });
    notification.show();
    return true;
  }

  // Closes one notification of the caller's activation and document.
  close(activation: object | null, document: number, tag: string): boolean {
    let found = false;
    this.#closeWhere((entry) => {
      const match = activation !== null && entry.activation === activation && entry.document === document && entry.tag === tag;
      found ||= match;
      return match;
    });
    return found;
  }

  // Closes everything an activation owns on sign-out, switch or quit.
  closeActivation(activation: object | null): void {
    if (activation !== null) this.#closeWhere((entry) => entry.activation === activation);
  }

  // When the activation's document ends, its notifications from older
  // documents can no longer be closed by the page that showed them (a close
  // must name the current document), so main closes them. The gate is
  // subscribed once, the first time the activation shows something.
  #watch(activation: object): void {
    const current = this.#deps.current();
    if (current === null || current.activation !== activation || this.#watched.has(current.gate)) return;
    this.#watched.add(current.gate);
    current.gate.onEnd((generation) => {
      this.#closeWhere((entry) => entry.activation === activation && entry.document < generation);
    });
  }

  #closeWhere(matches: (entry: Entry) => boolean): void {
    for (const [notification, entry] of [...this.#entries]) {
      if (!matches(entry)) continue;
      this.#entries.delete(notification);
      notification.close();
    }
  }

  #click(notification: NotificationLike): void {
    const entry = this.#entries.get(notification);
    if (entry === undefined) return;
    this.#entries.delete(notification);
    try {
      const window = this.#deps.window();
      if (window === null || window.isDestroyed()) return;
      if (window.isMinimized()) window.restore();
      window.show();
      window.focus();
      const current = this.#deps.current();
      if (current === null || current.activation !== entry.activation || !current.gate.shows(entry.document)) return;
      window.webContents.send(NOTIFICATION_EVENTS, { type: 'click', tag: entry.tag });
    } finally {
      notification.close();
    }
  }
}
