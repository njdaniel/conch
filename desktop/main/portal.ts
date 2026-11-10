// Whether this desktop can share a screen the one way the app allows
// (issue #1580, D4-03 of #772; linux-desktop.md §6.3, §8; Q-D4-4 on #1578):
// Chromium's PipeWire capturer, through the xdg-desktop-portal ScreenCast
// interface, whose own picker is the person's choice of source.
//
// Input: the process environment and one read-only D-Bus property, asked
// through GLib's gdbus at a fixed path, with argv only and no shell. Output:
// 'available', or why not. Owns: the last answer, which the display-capture
// rules read synchronously.
//
// Two things must both hold:
//  - the session is Wayland, by the very test Chromium's capturer makes
//    (libwebrtc's DesktopCapturer::IsRunningUnderWayland: XDG_SESSION_TYPE
//    begins "wayland" and WAYLAND_DISPLAY is set). On X11 Chromium does not
//    use PipeWire even with the feature on: it would list every screen and
//    window with no picker, which Q-D4-4 forbids, so Share is unavailable;
//  - the portal answers for org.freedesktop.portal.ScreenCast with a monitor
//    or a window source type (AvailableSourceTypes, a bitmask: 1 monitor,
//    2 window).
// Anything else, a missing gdbus included, is "unavailable": the app never
// guesses that a capture would go through a picker.
import { existsSync } from 'node:fs';

import { PORTAL_TIMEOUT_MS, runFile, type RunResult } from './voice/spawn.ts';

export { runFile };

export type ShareUnavailable = 'no-portal' | 'not-wayland';
export type ShareAvailability = 'available' | ShareUnavailable;

// Where GLib installs gdbus. Never looked up on PATH.
export const GDBUS_PATHS: readonly string[] = ['/usr/bin/gdbus', '/bin/gdbus'];


// The one call made: a property read, which changes nothing.
export const PORTAL_ARGS: readonly string[] = [
  'call',
  '--session',
  '--timeout',
  String(PORTAL_TIMEOUT_MS / 1000),
  '--dest',
  'org.freedesktop.portal.Desktop',
  '--object-path',
  '/org/freedesktop/portal/desktop',
  '--method',
  'org.freedesktop.DBus.Properties.Get',
  'org.freedesktop.portal.ScreenCast',
  'AvailableSourceTypes',
];

// The variables gdbus needs to find the session bus, and a fixed locale so its
// output is the one parsed. Nothing else of the environment is passed.
const PASSED = ['DBUS_SESSION_BUS_ADDRESS', 'XDG_RUNTIME_DIR'];

export type Env = Readonly<Record<string, string | undefined>>;

// underWayland is Chromium's test, so that main and the capturer agree.
export function underWayland(env: Env): boolean {
  return (env['XDG_SESSION_TYPE'] ?? '').startsWith('wayland') && env['WAYLAND_DISPLAY'] !== undefined;
}

// sourceTypes reads gdbus's printing of a uint32 variant, "(<uint32 3>,)",
// and nothing else.
export function sourceTypes(stdout: string): number | null {
  const m = /^\(<uint32 ([0-9]{1,10})>,\)\n?$/u.exec(stdout);
  if (m?.[1] === undefined) return null;
  const n = Number(m[1]);
  return n <= 0xffff_ffff ? n : null;
}

export interface PortalDeps {
  readonly platform: string;
  readonly env: Env;
  // Runs file with args and env, no shell, bounded in time and output; null
  // when it could not be run at all.
  readonly run: (file: string, args: readonly string[], env: Readonly<Record<string, string>>) => Promise<RunResult | null>;
  readonly executable: (path: string) => boolean;
  readonly log: (line: string) => void;
}

const WORDS: Record<ShareAvailability, string> = {
  available: 'screen share: the screen-cast portal answered',
  'no-portal': 'share unavailable: no screen-cast portal',
  'not-wayland': 'share unavailable: no screen-cast portal (not a Wayland session, so Chromium would not use one)',
};

export class Portal {
  readonly #deps: PortalDeps;
  #last: ShareAvailability | 'unknown' = 'unknown';

  constructor(deps: PortalDeps) {
    this.#deps = deps;
  }

  // last is the answer of the latest probe, or 'unknown' before the first:
  // only 'available' lets a capture through.
  last(): ShareAvailability | 'unknown' {
    return this.#last;
  }

  // probe asks again, and remembers and returns the answer. A change is one
  // log line.
  async probe(): Promise<ShareAvailability> {
    const answer = await this.#ask();
    if (answer !== this.#last) this.#deps.log(`conch-desktop: ${WORDS[answer]}`);
    this.#last = answer;
    return answer;
  }

  async #ask(): Promise<ShareAvailability> {
    const { platform, env } = this.#deps;
    if (platform !== 'linux') return 'no-portal';
    if (!underWayland(env)) return 'not-wayland';
    const gdbus = GDBUS_PATHS.find((p) => this.#deps.executable(p));
    if (gdbus === undefined) return 'no-portal';
    const passed: Record<string, string> = { LC_ALL: 'C' };
    for (const name of PASSED) {
      const value = env[name];
      if (value !== undefined) passed[name] = value;
    }
    let result: RunResult | null;
    try {
      result = await this.#deps.run(gdbus, PORTAL_ARGS, passed);
    } catch {
      result = null;
    }
    if (result?.code !== 0) return 'no-portal';
    const types = sourceTypes(result.stdout);
    return types !== null && (types & 3) !== 0 ? 'available' : 'no-portal';
  }
}

export function executable(path: string): boolean {
  return existsSync(path);
}
