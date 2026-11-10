// Is a tray host on the session bus? (issue #1589, D5-03; linux-desktop.md §9,
// Q-D5-2). Input: a RunFile. Output: whether org.kde.StatusNotifierWatcher
// has an owner. Owns: the one gdbus command line.
//
// This is necessary and not sufficient for a working tray: Electron gives no
// "is the icon visible" answer, so the person also confirms they can see it
// (settings.ts). Detection fails closed: a gdbus that is missing, slow or
// answers anything but `(true,)` means no tray.
//
// The probe is argv only through runFile: an absolute path, no shell, no
// PATH search, nothing of the person's text in it.
import type { RunFile } from './voice/spawn.ts';

export const GDBUS = '/usr/bin/gdbus';
export const WATCHER_NAME = 'org.kde.StatusNotifierWatcher';
export const DETECT_TIMEOUT_MS = 3_000;

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

// parseHasOwner reads gdbus's answer to NameHasOwner: `(true,)` or
// `(false,)`. Anything else is null.
export function parseHasOwner(stdout: string): boolean | null {
  const text = stdout.trim();
  if (text === '(true,)') return true;
  if (text === '(false,)') return false;
  return null;
}

// watcherPresent: true only when the bus answered that the watcher has an owner.
export async function watcherPresent(run: RunFile): Promise<boolean> {
  const result = await run(GDBUS, WATCHER_ARGV, DETECT_TIMEOUT_MS);
  return result.ok && parseHasOwner(result.stdout) === true;
}
