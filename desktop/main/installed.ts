// Whether the installed files were replaced under this running app (issue
// #1600, D6-06; linux-desktop.md section 9; #1595 Q-D6-8: updates are manual,
// there is no in-app download and no auto-update). `apt install` of a newer
// .deb replaces /opt/conch-desktop while the app runs. Chromium can fail when
// a running app later starts a process from replaced files, so the person is
// told, and nothing restarts by itself.
//
// Input: a reader of the installed stamp (resources/version, written by the
// build), a repeating timer, the running version, and the person's click on
// Restart. Output: an InstalledView for the page, sent when it changes, and the
// answer main's voice service asks (updated()) before it starts a helper.
// Owns: the stamp read at start, the latest stamp read, the once-only log of
// an unreadable stamp, and the one call that relaunches.
//
// What it keeps:
//   - the stamp is read at start and again when a window gains focus (check)
//     and every INTERVAL_MS; a stamp that differs from the one at start is the
//     notice, an unchanged one is none, and an unreadable one is logged once
//     and changes nothing;
//   - the version shown is the stamp's, and only if it looks like a release
//     version (STAMP_PATTERN): it is the person's text to read, never markup,
//     and a file with anything else in it is treated as unreadable;
//   - restart() is the only way out: it runs only after the person's click (the
//     IPC takes it with a gesture) and goes through the app's normal quit,
//     whose teardown stops the helper and removes the companion;
//   - nothing here stops a running helper: a call continues until the person
//     restarts.
import type { InstalledView } from '../../web/src/platform/desktop/bridge.ts';
import { UPDATE_EVENTS } from '../shared/bridge-names.ts';
import type { DocumentGate } from './document.ts';

// How often the stamp is read besides at focus.
export const INTERVAL_MS = 10 * 60 * 1000;

// What a stamp may say: the app version of scripts/desktop-version.sh
// (0.1.0-alpha.18, 0.0.0-dev+abc1234), short and without whitespace.
export const STAMP_PATTERN = /^[0-9A-Za-z][0-9A-Za-z.+~-]{0,63}$/;

export interface InstalledDeps {
  // The stamp's text, or a rejection when it cannot be read.
  readonly read: () => Promise<string>;
  // The version this process started as (package.json inside the app).
  readonly running: string;
  // Calls fn every ms until the returned function is called.
  readonly every: (fn: () => void, ms: number) => () => void;
  // Quits normally and relaunches: app.relaunch() then app.quit().
  readonly relaunch: () => void;
  // Tells the person in a native dialog that the page could not start after
  // the update, and resolves true when they chose Restart.
  readonly ask: (version: string | null) => Promise<boolean>;
  readonly log: (line: string) => void;
}

// parseStamp is the version in a stamp file's text, or null.
export function parseStamp(text: string): string | null {
  const trimmed = text.trim();
  return STAMP_PATTERN.test(trimmed) ? trimmed : null;
}

export class Installed {
  // The stamp at start, or the running version when it could not be read.
  #baseline: string;
  #latest: string | null = null;
  #unreadableLogged = false;
  #reading: Promise<void> | null = null;
  #stopTimer: (() => void) | null = null;
  #asked = false;
  readonly #listeners = new Set<(view: InstalledView) => void>();

  constructor(private readonly deps: InstalledDeps) {
    this.#baseline = deps.running;
  }

  // start records the stamp and begins the interval. Nothing is shown at start.
  async start(): Promise<void> {
    const first = await this.#readStamp();
    if (first !== null) this.#baseline = first;
    this.#stopTimer?.();
    this.#stopTimer = this.deps.every(() => {
      void this.check();
    }, INTERVAL_MS);
  }

  stop(): void {
    this.#stopTimer?.();
    this.#stopTimer = null;
  }

  // check reads the stamp again (a window gained focus, or the interval ran).
  // Overlapping checks share one read.
  check(): Promise<void> {
    if (this.#reading !== null) return this.#reading;
    const p = (async () => {
      const before = this.view();
      const stamp = await this.#readStamp();
      if (stamp !== null) this.#latest = stamp;
      const after = this.view();
      if (after.updated !== before.updated || after.version !== before.version) this.#emit(after);
    })().finally(() => {
      this.#reading = null;
    });
    this.#reading = p;
    return p;
  }

  // updated says whether the installed files are no longer the running ones.
  updated(): boolean {
    return this.#latest !== null && this.#latest !== this.#baseline;
  }

  view(): InstalledView {
    return this.updated() ? { updated: true, version: this.#latest } : { updated: false, version: null };
  }

  subscribe(listener: (view: InstalledView) => void): () => void {
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  }

  // restart is the person's click: the normal quit, then the relaunch.
  restart(): void {
    if (!this.updated()) {
      this.deps.log('installed: restart refused: nothing was updated');
      return;
    }
    this.deps.relaunch();
  }

  // pageFailed is a new page that did not start after an update (a switch's
  // renderer gone or not loaded). The page cannot show the notice then, so
  // main asks in a native dialog, once.
  async pageFailed(): Promise<void> {
    if (!this.updated() || this.#asked) return;
    this.#asked = true;
    let restart = false;
    try {
      restart = await this.deps.ask(this.#latest);
    } catch {
      this.deps.log('installed: the update dialog failed');
    }
    if (restart) this.restart();
  }

  // pageStarted is a page that loaded after an update: measured, and logged.
  pageStarted(): void {
    if (this.updated()) this.deps.log('installed: a page started after the update');
  }

  async #readStamp(): Promise<string | null> {
    let text: string;
    try {
      text = await this.deps.read();
    } catch {
      this.#unreadable('cannot be read');
      return null;
    }
    const stamp = parseStamp(text);
    if (stamp === null) this.#unreadable('is not a version');
    return stamp;
  }

  #unreadable(why: string): void {
    if (this.#unreadableLogged) return;
    this.#unreadableLogged = true;
    this.deps.log(`installed: the installed version stamp ${why}; updates cannot be noticed`);
  }

  #emit(view: InstalledView): void {
    for (const l of [...this.#listeners]) {
      try {
        l(view);
      } catch {
        // A listener's failure is its own.
      }
    }
  }
}

export interface UpdateTarget {
  // The owned web contents' send, or null when there is no live window.
  readonly contents: () => { send(channel: string, view: InstalledView): void } | null;
  readonly gate: () => DocumentGate;
}

// deliverUpdate sends the view to the owned page, only while its current
// document has committed (as the voice state is).
export function deliverUpdate(target: UpdateTarget, view: InstalledView): void {
  const contents = target.contents();
  const gate = target.gate();
  if (contents === null || !gate.shows(gate.generation)) return;
  contents.send(UPDATE_EVENTS, view);
}
