// The app's own settings, kept by main (issue #1587, linux-desktop.md §9).
// Input: <userData>/settings.json, and the person's changes arriving over IPC
// after a gesture. Output: the two app-wide settings, the rule for what
// closing the window does, and the launch-at-login entry kept in step.
//
// The file holds non-secret booleans only: mode 0600, replaced atomically as
// profiles.json is. It is never stored in a profile or in the renderer. A file
// that is missing, unreadable or malformed means the defaults, logged, with no
// recovery screen; unknown keys are dropped. Nothing here starts a helper,
// joins voice or resumes capture: a start at login opens the app as any launch
// does.
import { randomBytes } from 'node:crypto';
import { lstat, mkdir, open, readFile, rename, rm } from 'node:fs/promises';
import { join } from 'node:path';

import type { DesktopSettingsView, SettingsChange, SettingsResult } from '../../web/src/platform/desktop/bridge.ts';
import type { Autostart } from './autostart.ts';

const FILE_NAME = 'settings.json';
const FILE_MODE = 0o600;
const MAX_BYTES = 4 * 1024;

export interface AppSettings {
  // Closing the window keeps the app running, but only while a working tray
  // exists (see closeKeepsRunning).
  readonly closeToTray: boolean;
  // Open the app when the person logs in (an XDG autostart entry).
  readonly launchAtLogin: boolean;
}

export const DEFAULT_SETTINGS: AppSettings = { closeToTray: false, launchAtLogin: false };

// parseSettings reads the file's text: a JSON object whose known keys are
// booleans. A key that is absent or not a boolean takes its default; unknown
// keys are dropped. Text that is not an object at all is null (malformed).
export function parseSettings(text: string): AppSettings | null {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  const pick = (key: keyof AppSettings): boolean => (typeof record[key] === 'boolean' ? record[key] : DEFAULT_SETTINGS[key]);
  return { closeToTray: pick('closeToTray'), launchAtLogin: pick('launchAtLogin') };
}

export function serializeSettings(settings: AppSettings): string {
  return `${JSON.stringify({ closeToTray: settings.closeToTray, launchAtLogin: settings.launchAtLogin }, null, 2)}\n`;
}

// What closing the last window does. The app keeps running only when the
// person chose that and a working tray exists to bring it back; with no tray
// it quits (§9: "no tray means quit"), stopping media and reaping the helper
// in before-quit as today. A window closed for a switch or a sign-out is not
// the app ending at all.
export type CloseAction = 'stay' | 'quit';

export function closeAction(input: { readonly destroying: boolean; readonly closeToTray: boolean; readonly trayWorking: boolean }): CloseAction {
  if (input.destroying) return 'stay';
  return input.closeToTray && input.trayWorking ? 'stay' : 'quit';
}

export type ReadResult = { readonly kind: 'absent' } | { readonly kind: 'text'; readonly text: string } | { readonly kind: 'unreadable' };

export interface SettingsFiles {
  read(): Promise<ReadResult>;
  write(text: string): Promise<void>;
}

function isCode(err: unknown, code: string): boolean {
  return typeof err === 'object' && err !== null && (err as { code?: unknown }).code === code;
}

// The disk: <root>/settings.json. The root is the data root, already checked
// private by prepareRoot; the file is 0600 and replaced by rename.
export function createSettingsFiles(root: string): SettingsFiles {
  const file = join(root, FILE_NAME);
  return {
    async read() {
      try {
        const st = await lstat(file);
        if (!st.isFile() || st.size > MAX_BYTES) return { kind: 'unreadable' };
        return { kind: 'text', text: await readFile(file, 'utf8') };
      } catch (err) {
        return isCode(err, 'ENOENT') ? { kind: 'absent' } : { kind: 'unreadable' };
      }
    },
    async write(text) {
      await mkdir(root, { recursive: true, mode: 0o700 });
      const tmp = join(root, `${FILE_NAME}.${randomBytes(8).toString('hex')}.tmp`);
      try {
        const fh = await open(tmp, 'wx', FILE_MODE);
        try {
          await fh.chmod(FILE_MODE);
          await fh.writeFile(text, 'utf8');
          await fh.sync();
        } finally {
          await fh.close();
        }
        await rename(tmp, file);
      } catch (err) {
        await rm(tmp, { force: true });
        throw err;
      }
    },
  };
}

// Settings that are kept in memory only, for a data root that is not private
// to this user.
export function memorySettingsFiles(): SettingsFiles {
  return { read: () => Promise.resolve({ kind: 'absent' }), write: () => Promise.resolve() };
}

export interface SettingsDeps {
  readonly files: SettingsFiles;
  readonly autostart: Pick<Autostart, 'enable' | 'disable' | 'availability'>;
  // Whether a tray is up and working (tray.ts, #1589): false means closing
  // always quits.
  readonly trayWorking: () => boolean;
  // Whether the tray icon exists now. A confirmation is taken only for an
  // icon that exists; absent means none does.
  readonly trayExists?: () => boolean;
  // Whether a tray host is on the session bus now (tray-detect.ts). Absent
  // means not asked: closeToTray is accepted as it was before the tray.
  readonly trayDetected?: () => Promise<boolean>;
  // Called after a change is stored or the confirmation changes, so the tray
  // can follow closeToTray.
  readonly changed?: (settings: AppSettings) => void;
  readonly log: (line: string) => void;
  // The clock for the detector's short cache; a test supplies its own.
  readonly now?: () => number;
}

// How long view() reuses the detector's answer: settings reads that follow no
// gesture would otherwise run gdbus each time.
export const DETECT_CACHE_MS = 5_000;

export class SettingsService {
  readonly #deps: SettingsDeps;
  #current: AppSettings = DEFAULT_SETTINGS;
  // The person has said they can see the tray icon (Q-D5-2). Kept for this
  // run only: a new run asks again, as does losing the tray host.
  #confirmed = false;
  // One change at a time, so two writes cannot interleave.
  #queue: Promise<unknown> = Promise.resolve();
  // The last answer of the detector, for view() only.
  #seen: { readonly at: number; readonly value: boolean } | null = null;

  constructor(deps: SettingsDeps) {
    this.#deps = deps;
  }

  async load(): Promise<void> {
    const read = await this.#deps.files.read();
    if (read.kind === 'absent') return;
    const parsed = read.kind === 'text' ? parseSettings(read.text) : null;
    if (parsed === null) {
      this.#deps.log('conch-desktop: settings.json could not be read; the default settings apply');
      return;
    }
    this.#current = parsed;
  }

  // rejudge: run once after load(). An autostart entry is judged when it is
  // written, but the person can loosen an ancestor of the install later
  // (chmod 777), and the entry would keep naming that path. If launchAtLogin
  // is on and the build is now in an unsafe location, main says so, removes
  // its own entry (disable() touches only a file carrying the marker, never a
  // foreign one) and stores the setting off, so the next view shows it off
  // beside the unsafe-location reason. If the entry cannot be removed the
  // setting stays on: reading off while the file stays would be false.
  // The entry is removed from an unsafe location even when the stored setting
  // reads off (#1642's review): settings.json may have been unreadable, so the
  // default off says nothing about whether an entry of ours is still there.
  rejudge(): Promise<void> {
    const run = this.#queue.then(() => this.#rejudge());
    this.#queue = run.catch(() => undefined);
    return run;
  }

  async #rejudge(): Promise<void> {
    if ((await this.#deps.autostart.availability()) !== 'unsafe-location') return;
    if (!this.#current.launchAtLogin) {
      // Nothing to store; only an entry of ours that outlived the setting.
      const stale = await this.#deps.autostart.disable();
      if (!stale.ok && stale.reason !== 'foreign-file') this.#deps.log('conch-desktop: a stale autostart entry could not be removed');
      return;
    }
    this.#deps.log('conch-desktop: launch at login is on but the app no longer sits where only you or the system can change it; the autostart entry is removed and the setting turned off');
    const done = await this.#deps.autostart.disable();
    // A foreign file is left alone and there is nothing of ours to remove.
    if (!done.ok && done.reason !== 'foreign-file') {
      this.#deps.log('conch-desktop: the autostart entry could not be removed; the setting stays on');
      return;
    }
    const next: AppSettings = { ...this.#current, launchAtLogin: false };
    this.#current = next;
    try {
      await this.#deps.files.write(serializeSettings(next));
    } catch {
      this.#deps.log('conch-desktop: settings.json could not be written');
    }
    this.#deps.changed?.(next);
  }

  get current(): AppSettings {
    return this.#current;
  }

  // The rule app.ts consults when the last window closes.
  closeAction(destroying: boolean): CloseAction {
    return closeAction({ destroying, closeToTray: this.#current.closeToTray, trayWorking: this.#deps.trayWorking() });
  }

  get trayConfirmed(): boolean {
    return this.#confirmed;
  }

  // revokeTrayConfirmation: the tray host went away, so the person must
  // confirm again before closing keeps the app running.
  revokeTrayConfirmation(): void {
    this.#confirmed = false;
  }

  async view(): Promise<DesktopSettingsView> {
    const why = await this.#deps.autostart.availability();
    const detected = await this.#detectedForView();
    return { ...this.#current, trayWorking: this.#deps.trayWorking(), trayDetected: detected, trayConfirmed: this.#confirmed, launchAvailable: why === null, ...(why === null ? {} : { launchUnavailable: why }) };
  }

  async #detectedForView(): Promise<boolean> {
    const detect = this.#deps.trayDetected;
    if (detect === undefined) return true;
    const now = (this.#deps.now ?? Date.now)();
    if (this.#seen !== null && now - this.#seen.at >= 0 && now - this.#seen.at < DETECT_CACHE_MS) return this.#seen.value;
    const value = await detect();
    this.#seen = { at: now, value };
    return value;
  }

  // detectFresh asks the bus now and writes the answer into view()'s cache, so
  // a refused no-tray is not followed by a stale trayDetected: true.
  async #detectFresh(detect: () => Promise<boolean>): Promise<boolean> {
    const value = await detect();
    this.#seen = { at: (this.#deps.now ?? Date.now)(), value };
    return value;
  }

  set(change: SettingsChange): Promise<SettingsResult> {
    const run = this.#queue.then(() => this.#apply(change));
    this.#queue = run.catch(() => undefined);
    return run;
  }

  async #apply(change: SettingsChange): Promise<SettingsResult> {
    const { trayConfirmed, ...stored } = change;
    const next: AppSettings = { ...this.#current, ...stored };
    // A tray is turned on only while a tray host is on the bus (Q-D5-2).
    if (change.closeToTray === true && !this.#current.closeToTray && this.#deps.trayDetected !== undefined) {
      if (!(await this.#detectFresh(this.#deps.trayDetected))) return { ok: false, reason: 'no-tray' };
    }
    // The confirmation says "I can see the icon", so it is taken only for an
    // icon that exists: the stored setting is already on, the tray is up, and
    // the host is there. It never rides in the same change as the setting it
    // confirms: one call cannot turn the tray on and confirm it.
    if (trayConfirmed !== undefined && change.closeToTray !== undefined) return { ok: false, reason: 'no-tray' };
    if (trayConfirmed === true) {
      if ( !this.#current.closeToTray || this.#deps.trayExists?.() !== true) return { ok: false, reason: 'no-tray' };
      if (this.#deps.trayDetected !== undefined && !(await this.#detectFresh(this.#deps.trayDetected))) return { ok: false, reason: 'no-tray' };
    }
    // The autostart entry first: if it cannot be changed, nothing is stored.
    if (next.launchAtLogin !== this.#current.launchAtLogin) {
      const done = next.launchAtLogin ? await this.#deps.autostart.enable() : await this.#deps.autostart.disable();
      // Turning it off over a file Conch did not write leaves that file alone
      // and still stores the setting off: there is nothing of ours to remove.
      if (!done.ok && !(done.reason === 'foreign-file' && !next.launchAtLogin)) return { ok: false, reason: done.reason };
    }
    try {
      await this.#deps.files.write(serializeSettings(next));
    } catch {
      this.#deps.log('conch-desktop: settings.json could not be written');
      // Keep the entry and the stored setting in step.
      if (next.launchAtLogin !== this.#current.launchAtLogin) {
        await (this.#current.launchAtLogin ? this.#deps.autostart.enable() : this.#deps.autostart.disable());
      }
      return { ok: false, reason: 'failed' };
    }
    this.#current = next;
    if (!next.closeToTray) this.#confirmed = false;
    else if (trayConfirmed !== undefined) this.#confirmed = trayConfirmed;
    this.#deps.changed?.(next);
    return { ok: true, view: await this.view() };
  }
}
