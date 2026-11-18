// Is a tray host available? Linux uses the session bus (issue #1589, D5-03;
// linux-desktop.md §9, Q-D5-2); Windows uses the native tray (#1935).
// Input: a RunFile and platform. Output: on Linux, whether
// org.kde.StatusNotifierWatcher has an owner and reports a registered host
// (IsStatusNotifierHostRegistered):
// a watcher with no host draws no icon. Owns: the two gdbus command lines.
//
// On Windows, Electron creates the native notification-area icon: there is
// no bus probe. This only permits an opt-in attempt; TrayController must
// create the icon and the person must confirm visibility before close hides.
// Real notification-area visibility and Explorer recovery are F14 (#903).
//
// This is necessary and not sufficient for a working tray: Electron gives no
// "is the icon visible" answer, so the person also confirms they can see it
// (settings.ts). Detection fails closed: a gdbus that is missing, slow or
// answers anything but `(true,)` means no tray.
//
// The probe is argv only through runFile: an absolute path, no shell, no
// PATH search, nothing of the person's text in it.
import type { RunResult } from './voice/spawn.ts';

// RunFile is the one runner, voice/spawn.ts's runFile (its signature).
export type RunFile = (file: string, args: readonly string[], env: Readonly<Record<string, string>>) => Promise<RunResult | null>;

export const GDBUS = '/usr/bin/gdbus';
export const WATCHER_NAME = 'org.kde.StatusNotifierWatcher';

// What a session-bus query needs to find the bus, by exact name. The child
// gets these and nothing else (no HOME, no PATH: the file is absolute).
const BUS_ENV: readonly string[] = ['DBUS_SESSION_BUS_ADDRESS', 'XDG_RUNTIME_DIR'];

// busEnv picks the bus names out of source.
export function busEnv(source: Readonly<Record<string, string | undefined>> = process.env): Record<string, string> {
  const env: Record<string, string> = {};
  for (const name of BUS_ENV) {
    const value = source[name];
    if (value !== undefined) env[name] = value;
  }
  return env;
}

export const WATCHER_ARGV: readonly string[] = [
  'call',
  '--session',
  '--dest',
  'org.freedesktop.DBus',
  '--object-path',
  '/org/freedesktop/DBus',
  '--method',
  'org.freedesktop.DBus.NameHasOwner',
  WATCHER_NAME,
];

// The watcher's IsStatusNotifierHostRegistered property, read through the
// standard Properties interface.
export const HOST_ARGV: readonly string[] = [
  'call',
  '--session',
  '--dest',
  WATCHER_NAME,
  '--object-path',
  '/StatusNotifierWatcher',
  '--method',
  'org.freedesktop.DBus.Properties.Get',
  WATCHER_NAME,
  'IsStatusNotifierHostRegistered',
];

// parseHostRegistered reads gdbus's answer to the property Get: `(<true>,)` or
// `(<false>,)`. Anything else is null.
export function parseHostRegistered(stdout: string): boolean | null {
  const text = stdout.trim();
  if (text === '(<true>,)') return true;
  if (text === '(<false>,)') return false;
  return null;
}

// parseHasOwner reads gdbus's answer to NameHasOwner: `(true,)` or
// `(false,)`. Anything else is null.
export function parseHasOwner(stdout: string): boolean | null {
  const text = stdout.trim();
  if (text === '(true,)') return true;
  if (text === '(false,)') return false;
  return null;
}

// watcherPresent: true only when the bus answered that the watcher has an owner.
export async function watcherPresent(run: RunFile, env: Readonly<Record<string, string>> = busEnv()): Promise<boolean> {
  const result = await run(GDBUS, WATCHER_ARGV, env);
  return result !== null && result.code === 0 && parseHasOwner(result.stdout) === true;
}

// trayHostPresent: on Linux, the watcher has an owner and a registered host;
// on Windows, permit native icon creation and the visibility confirmation.
export async function trayHostPresent(run: RunFile, env: Readonly<Record<string, string>> = busEnv(), platform: NodeJS.Platform = process.platform): Promise<boolean> {
  if (platform === 'win32') return true;
  if (!(await watcherPresent(run, env))) return false;
  const result = await run(GDBUS, HOST_ARGV, env);
  return result !== null && result.code === 0 && parseHostRegistered(result.stdout) === true;
}
