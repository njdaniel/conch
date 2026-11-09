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
  readonly gate: Pick<DocumentGate, 'shows'>;
}

// The native API and current window/activation, supplied by the app or fakes.
export interface NotificationsDeps {
  readonly Notification: NotificationConstructor;
  readonly icon: string;
  readonly window: () => WindowLike | null;
  readonly current: () => CurrentActivation | null;
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
  #locked = false;

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

  // Shows one notification owned by this activation and document generation.
  show(activation: object | null, document: number, request: NotifyRequest): boolean {
    if (activation === null || !this.supported()) return false;
    const notification = new this.#deps.Notification({
      title: request.title,
      body: this.#locked ? 'New message' : request.body,
      icon: this.#deps.icon,
    });
    const entry: Entry = { activation, document, tag: request.tag, channel: request.channel };
    this.#entries.set(notification, entry);
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
