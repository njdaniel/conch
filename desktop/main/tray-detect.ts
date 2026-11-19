// Is a tray host available? Linux uses the session bus (issue #1589, D5-03;
// linux-desktop.md §9, Q-D5-2); Windows uses the native tray (#1935).
// Input: a RunFile, environment and platform. Output: live host evidence.
// On Linux, org.kde.StatusNotifierWatcher must have an owner and report a
// registered host (IsStatusNotifierHostRegistered): a watcher with no host
// draws no icon. Owns: the gdbus and tasklist command lines and their parsing.
//
// On Windows, icon creation is an opt-in attempt, independent of this live
// host check. tasklist identifies this process's session, then asks whether
// explorer.exe is running there. This is a heuristic, not proof that an icon
// is visible; visibility confirmation is still required. F14 hardware testing
// (#903) checks the heuristic and Explorer recovery.
//
// Detection fails closed on errors, timeouts or unknown output. RunFile bounds
// each query's time (3.5 seconds) and output. All probes use an absolute path
// and argv, never a shell or PATH search.
import { win32 } from 'node:path';

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

// Only SystemRoot is needed by tasklist; never pass credentials or PATH.
function windowsEnv(): Record<string, string> {
  const root = Object.entries(process.env).find(([name]) => name.toUpperCase() === 'SYSTEMROOT')?.[1];
  return root === undefined ? {} : { SystemRoot: root };
}

interface Task {
  readonly image: string;
  readonly pid: string;
  readonly session: string;
}

// /FO CSV /NH has five quoted columns, independent of localized headings.
// Reject the whole answer on unexpected rows (including localized "no tasks"
// messages), rather than searching arbitrary output for explorer.exe.
function tasks(result: RunResult | null): Task[] | null {
  if (result === null || result.code !== 0 || result.stdout.trim() === '') return null;
  const rows: Task[] = [];
  for (const line of result.stdout.trim().split(/\r?\n/u)) {
    const match = /^"([^"\r\n]+)","([1-9][0-9]*)","[^"\r\n]+","([0-9]+)","[^"\r\n]+"$/u.exec(line);
    if (match === null) return null;
    rows.push({ image: match[1] ?? '', pid: match[2] ?? '', session: match[3] ?? '' });
  }
  return rows;
}

async function windowsHostPresent(run: RunFile, env: Readonly<Record<string, string>>): Promise<boolean> {
  const root = env['SystemRoot'];
  if (root === undefined || !/^[A-Za-z]:[\\/]/u.test(root) || root.includes('\0')) return false;
  const file = win32.join(root, 'System32', 'tasklist.exe');
  try {
    const own = tasks(await run(file, ['/FI', `PID eq ${String(process.pid)}`, '/FO', 'CSV', '/NH'], env));
    if (own?.length !== 1 || own[0]?.pid !== String(process.pid)) return false;
    const session = own[0].session;
    const hosts = tasks(await run(file, ['/FI', 'IMAGENAME eq explorer.exe', '/FI', `SESSION eq ${session}`, '/FO', 'CSV', '/NH'], env));
    return hosts !== null && hosts.length > 0 && hosts.every((host) => host.image.toLowerCase() === 'explorer.exe' && host.session === session);
  } catch {
    return false;
  }
}

// trayHostPresent supplies live evidence for periodic and close-time checks;
// permission to attempt native icon creation is not evidence of a live host.
export async function trayHostPresent(run: RunFile, env?: Readonly<Record<string, string>>, platform: NodeJS.Platform = process.platform): Promise<boolean> {
  if (platform === 'win32') return windowsHostPresent(run, env ?? windowsEnv());
  const bus = env ?? busEnv();
  if (!(await watcherPresent(run, bus))) return false;
  const result = await run(GDBUS, HOST_ARGV, bus);
  return result !== null && result.code === 0 && parseHostRegistered(result.stdout) === true;
}
