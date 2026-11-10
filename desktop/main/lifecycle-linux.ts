// How the Linux desktop app learns that the screen locked or the machine is
// going to sleep (issue #1588, D5-02 of #773; decision Q-D5-1). Input: a
// `gdbus monitor` child on logind, the `gdbus call`s that find this session
// and read its LockedHint, Electron's powerMonitor, and a timer. Output:
// calls on the rules in lifecycle.ts. Owns: the monitor child (spawned with
// argv only, never detached, killed through its own ChildProcess), its
// restarts and the line parser. It imports Electron's powerMonitor only
// through the interface below, and child_process not at all (spawn.ts does).
//
// Why gdbus: Electron's lock-screen and unlock-screen events are macOS and
// Windows only, and there is no D-Bus package in the app (no new runtime
// dependency). `gdbus` ships with GLib. The system bus is monitored with
// `monitor --system --dest org.freedesktop.login1`, which needs no
// privileges (busctl monitor does). The idle-state poll is a second signal:
// the screen counts as locked if either says so.
//
// What it does not do: pretend. If there is no gdbus, no session for this
// process, or the monitor keeps dying, the lock is reported as not observed,
// once on the log, and the rules hide notification text. A watcher that gives
// up does not force-mute (S5 of #1588, decided by the principal): #1593's
// notice tells the person that lock won't mute them.
//
// Which session: a normally launched app may have no session of its own.
// On systemd 255 every process under user@UID.service, app scopes and
// app-*.service units included, gets NoSessionForPID from GetSessionByPID.
// Then the user's Display session (the graphical one) is watched instead.
import { stat } from 'node:fs/promises';

import type { Lifecycle } from './lifecycle.ts';
import { runFile, spawnWatch, type RunResult, type WatchChild } from './voice/spawn.ts';

const LOGIND = 'org.freedesktop.login1';
const MANAGER_PATH = '/org/freedesktop/login1';
const SESSION_IFACE = 'org.freedesktop.login1.Session';

// gdbus is run by absolute path, never found through PATH.
export const GDBUS_PATHS: readonly string[] = ['/usr/bin/gdbus', '/bin/gdbus'];

// The most restarts of a dying monitor, and the wait before each.
export const MAX_RESTARTS = 3;
export const RESTART_BACKOFF_MS: readonly number[] = [1000, 2000, 4000];

// How long a monitor must stay up before its restarts are forgiven.
const STABLE_MS = 30_000;

// How long a started monitor may take to print that it is subscribed.
export const READY_MS = 5000;

// How often the idle state is read (a second signal).
export const IDLE_POLL_MS = 1000;

// What the parser makes of one line of `gdbus monitor`.
export type MonitorEvent =
  | { readonly kind: 'lock'; readonly locked: boolean }
  | { readonly kind: 'sleep'; readonly sleeping: boolean };

export type ParsedLine = { readonly kind: 'event'; readonly event: MonitorEvent } | { readonly kind: 'ignored' } | { readonly kind: 'malformed' };

// A signal line: `<object path>: <interface>.<member> (<arguments>)`.
const SIGNAL = /^(\/[A-Za-z0-9_/]*): ([A-Za-z0-9_.]+)\.([A-Za-z0-9_]+) \((.*)\)$/u;
// The lines gdbus prints on its own.
const PREAMBLE = /^(Monitoring signals from all objects owned by |The name )[A-Za-z0-9_.:]+( is owned by | has no owner| )?/u;
const SESSION_PATH = /^\/org\/freedesktop\/login1\/session\/[A-Za-z0-9_]+$/u;
// The line gdbus prints once it has subscribed and learned the owner of the
// name: the first moment a signal can no longer be missed.
const READY = /^The name org\.freedesktop\.login1 is owned by /u;
const SLEEP_ARGS = /^(true|false),$/u;

// lockedHintOf reads the entries of a printed a{sv} dictionary (the text
// after its `{`) and returns the value of the key LockedHint, 'absent' when
// there is no such key, or 'invalid' when the dictionary does not parse. It
// walks key by key, so text inside another entry's string value (which can
// say anything, including `'LockedHint': <false>`) is never read as a key.
function lockedHintOf(text: string): boolean | 'absent' | 'invalid' {
  let i = 0;
  let found: boolean | null = null;
  for (;;) {
    if (text[i] === '}') return found ?? 'absent';
    const key = /^'([A-Za-z0-9_]+)': /u.exec(text.slice(i));
    if (key === null) return 'invalid';
    i += key[0].length;
    const end = valueEnd(text, i);
    if (end === null) return 'invalid';
    if (key[1] === 'LockedHint') {
      const v = /^<(true|false)>$/u.exec(text.slice(i, end));
      if (v === null) return 'invalid';
      found = v[1] === 'true';
    }
    i = end;
    if (text.startsWith(', ', i)) i += 2;
    else if (text[i] !== '}') return 'invalid';
  }
}

// valueEnd returns the index just past the value that starts at `from`
// (balanced brackets, quoted strings skipped whole), or null.
function valueEnd(text: string, from: number): number | null {
  const closer: Readonly<Record<string, string>> = { '<': '>', '(': ')', '[': ']', '{': '}' };
  const stack: string[] = [];
  for (let i = from; i < text.length; i++) {
    const c = text.charAt(i);
    if (c === "'" || c === '"') {
      for (i++; i < text.length && text.charAt(i) !== c; i++) if (text.charAt(i) === '\\') i++;
      if (i >= text.length) return null;
    } else if (c in closer) {
      stack.push(closer[c] ?? '');
    } else if (c === '>' || c === ')' || c === ']' || c === '}') {
      if (stack.pop() !== c) return null;
      if (stack.length === 0) return i + 1;
    } else if (stack.length === 0 && (c === ',' || c === '}')) {
      return i;
    }
  }
  return null;
}

// parseLine accepts only this session's object path (session) and the signals
// Lock, Unlock, PropertiesChanged of LockedHint, and the manager's
// PrepareForSleep. Another session's signals and other members are ignored;
// a line that is not a signal at all, or a signal of ours with arguments it
// does not have, is malformed (the caller counts those).
export function parseLine(line: string, session: string): ParsedLine {
  const text = line.trim();
  if (text === '' || PREAMBLE.test(text)) return { kind: 'ignored' };
  const m = SIGNAL.exec(text);
  if (m === null) return { kind: 'malformed' };
  const [, path, iface, member, args] = m;
  if (path === undefined || iface === undefined || member === undefined || args === undefined) return { kind: 'malformed' };
  if (path === MANAGER_PATH && iface === `${LOGIND}.Manager` && member === 'PrepareForSleep') {
    const a = SLEEP_ARGS.exec(args);
    return a === null ? { kind: 'malformed' } : { kind: 'event', event: { kind: 'sleep', sleeping: a[1] === 'true' } };
  }
  if (path !== session) return { kind: 'ignored' };
  if (iface === SESSION_IFACE && (member === 'Lock' || member === 'Unlock')) {
    return args === '' ? { kind: 'event', event: { kind: 'lock', locked: member === 'Lock' } } : { kind: 'malformed' };
  }
  if (iface === 'org.freedesktop.DBus.Properties' && member === 'PropertiesChanged') {
    if (!args.startsWith("'")) return { kind: 'malformed' };
    // Other properties of the session change all the time; the hint may
    // arrive with some of them in one dictionary.
    const head = `'${SESSION_IFACE}', {`;
    if (!args.startsWith(head)) return { kind: 'ignored' };
    const hint = lockedHintOf(args.slice(head.length));
    if (hint === 'invalid') return { kind: 'malformed' };
    return hint === 'absent' ? { kind: 'ignored' } : { kind: 'event', event: { kind: 'lock', locked: hint } };
  }
  return { kind: 'ignored' };
}

// parseSession reads `(objectpath '/org/freedesktop/login1/session/_32',)`.
export function parseSession(stdout: string): string | null {
  const m = /^\(objectpath '([^']*)',\)\s*$/u.exec(stdout);
  return m?.[1] !== undefined && SESSION_PATH.test(m[1]) ? m[1] : null;
}

// parseDisplaySession reads the user's Display property,
// `(<('5', objectpath '/org/freedesktop/login1/session/_35')>,)`: a session
// id and its path. The path must pass the same rule as parseSession's; the
// "no display session" answer (`('', objectpath '/')`) does not.
export function parseDisplaySession(stdout: string): string | null {
  const m = /^\(<\('[A-Za-z0-9_]+', objectpath '([^']*)'\)>,\)\s*$/u.exec(stdout);
  return m?.[1] !== undefined && SESSION_PATH.test(m[1]) ? m[1] : null;
}

// parseLockedHint reads `(<true>,)` or `(<false>,)`.
export function parseLockedHint(stdout: string): boolean | null {
  const m = /^\(<(true|false)>,\)\s*$/u.exec(stdout);
  return m === null ? null : m[1] === 'true';
}

export interface PowerMonitorLike {
  on(event: 'suspend' | 'resume', listener: () => void): unknown;
  removeListener(event: 'suspend' | 'resume', listener: () => void): unknown;
  getSystemIdleState(idleThreshold: number): string;
}

export interface WatcherDeps {
  readonly rules: Pick<Lifecycle, 'lockSignal' | 'observe' | 'suspend' | 'resume'>;
  readonly powerMonitor: PowerMonitorLike;
  readonly pid: number;
  readonly log: (line: string) => void;
  // Test seams; the defaults are the real ones.
  readonly findGdbus?: () => Promise<string | null>;
  readonly run?: (file: string, args: readonly string[], env: Readonly<Record<string, string>>) => Promise<RunResult | null>;
  readonly watch?: (file: string, args: readonly string[], env: Readonly<Record<string, string>>) => WatchChild;
  readonly backoffMs?: readonly number[];
  readonly idlePollMs?: number;
  // How long to wait for the monitor to say it is subscribed.
  readonly readyMs?: number;
}

async function defaultFindGdbus(): Promise<string | null> {
  for (const path of GDBUS_PATHS) {
    try {
      const st = await stat(path);
      if (st.isFile() && (st.mode & 0o111) !== 0) return path;
    } catch {
      // Try the next.
    }
  }
  return null;
}

// The child's environment: nothing of main's. gdbus finds the system bus at
// its default socket; the C locale keeps the output as the parser expects.
const CHILD_ENV: Readonly<Record<string, string>> = { LC_ALL: 'C' };

export class LinuxLifecycle {
  readonly #deps: WatcherDeps;
  readonly #backoff: readonly number[];
  #child: WatchChild | null = null;
  #restarts = 0;
  #timer: NodeJS.Timeout | undefined;
  #poll: NodeJS.Timeout | undefined;
  #stopped = true;
  #malformed = 0;
  #reportedLost = false;
  #upSince = 0;
  #sawLock = false;
  readonly #onSuspend = (): void => {
    void this.#deps.rules.suspend();
  };
  readonly #onResume = (): void => {
    this.#deps.rules.resume();
  };

  constructor(deps: WatcherDeps) {
    this.#deps = deps;
    this.#backoff = deps.backoffMs ?? RESTART_BACKOFF_MS;
  }

  // The count of malformed lines the parser has ignored.
  get malformed(): number {
    return this.#malformed;
  }

  // start begins watching: powerMonitor's suspend and resume, the idle-state
  // poll, and the logind monitor.
  async start(): Promise<void> {
    if (!this.#stopped) return;
    this.#stopped = false;
    this.#deps.powerMonitor.on('suspend', this.#onSuspend);
    this.#deps.powerMonitor.on('resume', this.#onResume);
    this.#poll = setInterval(() => {
      let locked = false;
      try {
        locked = this.#deps.powerMonitor.getSystemIdleState(1) === 'locked';
      } catch {
        // No idle state is no signal.
      }
      void this.#deps.rules.lockSignal('idle', locked);
    }, this.#deps.idlePollMs ?? IDLE_POLL_MS);
    await this.#attach();
  }

  // stop kills the monitor through its own ChildProcess and stops every timer.
  stop(): void {
    this.#stopped = true;
    clearTimeout(this.#timer);
    clearInterval(this.#poll);
    this.#deps.powerMonitor.removeListener('suspend', this.#onSuspend);
    this.#deps.powerMonitor.removeListener('resume', this.#onResume);
    const child = this.#child;
    this.#child = null;
    child?.kill('SIGTERM');
  }

  // #lockSeen: the monitor reported a lock change since it started (read
  // after awaits, where the flag may have changed).
  #lockSeen(): boolean {
    return this.#sawLock;
  }

  // #halted is read after awaits, where the flag may have changed.
  #halted(): boolean {
    return this.#stopped;
  }

  #lost(why: string): void {
    this.#deps.rules.observe('unavailable');
    if (!this.#reportedLost) {
      this.#reportedLost = true;
      this.#deps.log(`lifecycle: lock not observed: ${why}`);
    }
  }

  // #findSession: this process's own session, else the user's display
  // session (see the header). null when neither can be found.
  async #findSession(run: NonNullable<WatcherDeps['run']>, gdbus: string): Promise<string | null> {
    const found = await run(gdbus, ['call', '--system', '--dest', LOGIND, '--object-path', MANAGER_PATH, '--method', `${LOGIND}.Manager.GetSessionByPID`, String(this.#deps.pid)], CHILD_ENV);
    const own = found !== null && found.code === 0 ? parseSession(found.stdout) : null;
    if (own !== null || this.#halted()) return own;
    const display = await run(gdbus, ['call', '--system', '--dest', LOGIND, '--object-path', `${MANAGER_PATH}/user/self`, '--method', 'org.freedesktop.DBus.Properties.Get', `${LOGIND}.User`, 'Display'], CHILD_ENV);
    return display !== null && display.code === 0 ? parseDisplaySession(display.stdout) : null;
  }

  // #attach finds the session, starts the monitor, waits for the monitor to
  // say it is subscribed, then reads the current LockedHint (so no change
  // falls between the read and the subscription). The same at each restart.
  async #attach(): Promise<void> {
    const gdbus = await (this.#deps.findGdbus ?? defaultFindGdbus)();
    if (this.#halted()) return;
    if (gdbus === null) {
      this.#lost('gdbus is not installed');
      return;
    }
    const run = this.#deps.run ?? runFile;
    const session = await this.#findSession(run, gdbus);
    if (this.#halted()) return;
    if (session === null) {
      this.#lost('this process has no logind session and the user has no display session');
      return;
    }
    let child: WatchChild;
    try {
      child = (this.#deps.watch ?? spawnWatch)(gdbus, ['monitor', '--system', '--dest', LOGIND], CHILD_ENV);
    } catch {
      this.#retry('the monitor could not be started');
      return;
    }
    this.#child = child;
    this.#sawLock = false;
    // The hint is read only after the monitor has said it is subscribed, so
    // no lock falls between the read and the subscription.
    let subscribed: (ok: boolean) => void = () => undefined;
    const ready = new Promise<boolean>((resolve) => {
      subscribed = resolve;
    });
    let pending = '';
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (chunk: string) => {
      pending += chunk;
      // A line that never ends is not one: cut it off.
      if (pending.length > 64 * 1024) pending = '';
      let at = pending.indexOf('\n');
      while (at >= 0) {
        const text = pending.slice(0, at);
        if (READY.test(text.trim())) subscribed(true);
        this.#line(text, session);
        pending = pending.slice(at + 1);
        at = pending.indexOf('\n');
      }
    });
    child.stderr.resume();
    const ended = (): void => {
      if (this.#child !== child) return;
      this.#child = null;
      // Only a monitor that stayed up for a while earns its restarts back,
      // so one that dies at once every time is given up on.
      if (this.#upSince !== 0 && Date.now() - this.#upSince > STABLE_MS) this.#restarts = 0;
      this.#upSince = 0;
      subscribed(false);
      if (!this.#stopped) this.#retry('the monitor ended');
    };
    child.on('error', ended);
    child.on('close', ended);
    let readyTimer: NodeJS.Timeout | undefined;
    const slow = new Promise<boolean>((resolve) => {
      readyTimer = setTimeout(() => {
        resolve(false);
      }, this.#deps.readyMs ?? READY_MS);
    });
    const up = await Promise.race([ready, slow]);
    clearTimeout(readyTimer);
    if (this.#halted() || this.#child !== child) return;
    if (!up) {
      child.kill('SIGTERM');
      this.#child = null;
      this.#retry('the monitor did not say it was subscribed');
      return;
    }
    const hint = await run(gdbus, ['call', '--system', '--dest', LOGIND, '--object-path', session, '--method', 'org.freedesktop.DBus.Properties.Get', SESSION_IFACE, 'LockedHint'], CHILD_ENV);
    if (this.#halted() || this.#child !== child) return;
    const locked = hint !== null && hint.code === 0 ? parseLockedHint(hint.stdout) : null;
    if (locked === null) {
      child.kill('SIGTERM');
      this.#child = null;
      this.#retry('the session lock state could not be read');
      return;
    }
    this.#reportedLost = false;
    this.#upSince = Date.now();
    // The read came after the monitor started: a change the monitor has
    // already reported is newer than it.
    if (!this.#lockSeen()) void this.#deps.rules.lockSignal('logind', locked);
    this.#deps.rules.observe('available');
  }

  #line(line: string, session: string): void {
    const parsed = parseLine(line, session);
    if (parsed.kind === 'malformed') {
      this.#malformed++;
      return;
    }
    if (parsed.kind !== 'event') return;
    if (parsed.event.kind === 'lock') this.#sawLock = true;
    if (parsed.event.kind === 'lock') void this.#deps.rules.lockSignal('logind', parsed.event.locked);
    else if (parsed.event.sleeping) void this.#deps.rules.suspend();
    else this.#deps.rules.resume();
  }

  // #retry: the lock is not observed while the monitor is down; restart at
  // most MAX_RESTARTS times with backoff, then stay unobserved.
  #retry(why: string): void {
    this.#deps.rules.observe('unavailable');
    if (this.#restarts >= Math.min(MAX_RESTARTS, this.#backoff.length)) {
      this.#lost(`${why}, and it did not come back`);
      return;
    }
    const wait = this.#backoff[this.#restarts] ?? 0;
    this.#restarts++;
    this.#timer = setTimeout(() => {
      if (!this.#stopped) void this.#attach();
    }, wait);
  }
}
