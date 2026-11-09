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
  // Whether a tray is up and working. #1589 (D5-03) supplies the real answer;
  // until then it is false and closing always quits.
  readonly trayWorking: () => boolean;
  readonly log: (line: string) => void;
}

export class SettingsService {
  readonly #deps: SettingsDeps;
  #current: AppSettings = DEFAULT_SETTINGS;
  // One change at a time, so two writes cannot interleave.
  #queue: Promise<unknown> = Promise.resolve();

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

  get current(): AppSettings {
    return this.#current;
  }

  // The rule app.ts consults when the last window closes.
  closeAction(destroying: boolean): CloseAction {
    return closeAction({ destroying, closeToTray: this.#current.closeToTray, trayWorking: this.#deps.trayWorking() });
  }

  async view(): Promise<DesktopSettingsView> {
    const why = await this.#deps.autostart.availability();
    return { ...this.#current, trayWorking: this.#deps.trayWorking(), launchAvailable: why === null, ...(why === null ? {} : { launchUnavailable: why }) };
  }

  set(change: SettingsChange): Promise<SettingsResult> {
    const run = this.#queue.then(() => this.#apply(change));
    this.#queue = run.catch(() => undefined);
    return run;
  }

  async #apply(change: SettingsChange): Promise<SettingsResult> {
    const next: AppSettings = { ...this.#current, ...change };
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
    return { ok: true, view: await this.view() };
  }
}
