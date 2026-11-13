// Where a --user-data-dir may point on Windows (issue #1788; windows-desktop.md
// §2.7). Input: the platform, Chromium's switches, the data root Electron
// resolved (app.getPath('userData')), the environment and a realpath. Output:
// a verdict, and at most one line on the log. It owns the rule only; it
// creates, reads and changes nothing.
//
// On Linux and macOS, prepareRoot (instance.ts) judges the root by its mode
// bits and its ancestors'. Windows has no mode bits: the root is private only
// because it sits in a directory whose ACL is private, which %USERPROFILE%,
// %APPDATA% and %LOCALAPPDATA% are under their default DACL (the user, SYSTEM
// and Administrators). A directory made under a drive root (C:\conch-data)
// inherits Modify for Authenticated Users, so another local account could
// write profiles\<id>\voice.toml there for the helper, or the app's other
// files. Node has no ACL API without a native module, so the check is by
// path: a root given with --user-data-dir must be inside one of those three
// directories, after junctions and symbolic links are resolved, or the app
// does not start. --allow-shared-user-data-dir is the deliberate way past it,
// and says what it risks. Without --user-data-dir the root is Electron's
// %APPDATA%\conch-desktop, and nothing is checked.
//
// It is a guard against a mistaken launch, not against an attacker running as
// this user (who can pass the override flag too), and it does not prove the
// ACL: a profile directory whose ACL someone has widened passes.
import { realpathSync } from 'node:fs';
import { win32 } from 'node:path';

export const USER_DATA_SWITCH = 'user-data-dir';
export const ALLOW_SHARED_SWITCH = 'allow-shared-user-data-dir';

// The directories a root may be inside, by environment variable.
export const PRIVATE_BASES = ['USERPROFILE', 'APPDATA', 'LOCALAPPDATA'] as const;

export const REFUSED_LINE =
  'conch-desktop: refusing to start: the --user-data-dir data root must be under your user profile (%USERPROFILE%, %APPDATA% or %LOCALAPPDATA%); ' +
  `--${ALLOW_SHARED_SWITCH} overrides this`;

export const SHARED_LINE =
  `conch-desktop: --${ALLOW_SHARED_SWITCH}: the data root is outside your user profile, ` +
  "so other local accounts may be able to change this app's files, the voice helper's settings among them";

// unchecked: not Windows, or no --user-data-dir. inside: the root is under a
// private base. allowed: it is not, and the override was given. refused: it is
// not, and the app must exit.
export type DataRootVerdict = 'unchecked' | 'inside' | 'allowed' | 'refused';

export interface DataRootInputs {
  readonly platform: NodeJS.Platform;
  readonly hasSwitch: (name: string) => boolean;
  // app.getPath('userData'): what Electron made of --user-data-dir.
  readonly root: string;
  readonly env: Readonly<Record<string, string | undefined>>;
  // fs.realpathSync.native unless a test says otherwise: it resolves
  // junctions and symbolic links and throws (ENOENT) for a missing path.
  readonly realpath?: (path: string) => string;
}

export function dataRootVerdict(inputs: DataRootInputs, log: (line: string) => void): DataRootVerdict {
  if (inputs.platform !== 'win32' || !inputs.hasSwitch(USER_DATA_SWITCH)) return 'unchecked';
  if (underPrivateBase(inputs.root, inputs.env, inputs.realpath ?? realpathSync.native)) return 'inside';
  if (inputs.hasSwitch(ALLOW_SHARED_SWITCH)) {
    log(SHARED_LINE);
    return 'allowed';
  }
  log(REFUSED_LINE);
  return 'refused';
}

// underPrivateBase: whether root, made real, is inside one of the bases, each
// taken as written and as made real (a redirected %APPDATA% is a junction to
// where the files really are). A root that is not fully qualified, or cannot
// be resolved, is not inside anything; a base that is empty or not fully
// qualified is ignored.
export function underPrivateBase(root: string, env: Readonly<Record<string, string | undefined>>, realpath: (path: string) => string): boolean {
  const real = qualified(root) ? resolved(win32.normalize(root), realpath) : null;
  if (real === null) return false;
  return PRIVATE_BASES.some((name) => {
    const base = env[name];
    if (base === undefined || !qualified(base)) return false;
    const plain = win32.normalize(base);
    const realBase = resolved(plain, realpath);
    return isInside(real, plain) || (realBase !== null && isInside(real, realBase));
  });
}

// qualified: a drive-letter path (C:\...) or a UNC one (\\server\share\...).
// Not C:foo (relative to that drive's current directory), not \foo (relative
// to the current drive) and not a bare relative path: none of them names one
// place.
export function qualified(path: string): boolean {
  return /^[A-Za-z]:[\\/]/.test(path) || /^[\\/]{2}[^\\/]/.test(path);
}

// resolved: the path with every junction and symbolic link resolved. A path
// that does not exist yet (a first launch) is its nearest existing ancestor,
// resolved, with the rest added back. Any failure but ENOENT is null.
function resolved(path: string, realpath: (path: string) => string): string | null {
  const rest: string[] = [];
  for (let dir = path; ; ) {
    try {
      return win32.join(stripVerbatim(realpath(dir)), ...rest);
    } catch (error) {
      if (!(error instanceof Error) || (error as NodeJS.ErrnoException).code !== 'ENOENT') return null;
      const parent = win32.dirname(dir);
      if (parent === dir) return path;
      rest.unshift(win32.basename(dir));
      dir = parent;
    }
  }
}

// GetFinalPathNameByHandleW gives \\?\C:\... and \\?\UNC\server\...; libuv
// strips that, and this does again in case it does not.
function stripVerbatim(path: string): string {
  if (/^\\\\\?\\UNC\\/i.test(path)) return `\\\\${path.slice(8)}`;
  if (path.startsWith('\\\\?\\')) return path.slice(4);
  return path;
}

// isInside: child is parent or below it, compared segment by segment and
// case-insensitively, as Windows compares names: C:\Users\nickx is not inside
// C:\Users\nick. Both are normalized, fully qualified paths.
export function isInside(child: string, parent: string): boolean {
  const c = segments(child);
  const p = segments(parent);
  return p.length > 0 && c.length >= p.length && p.every((s, i) => fold(s) === fold(c[i] ?? ''));
}

function segments(path: string): string[] {
  const parts = win32.normalize(path).split('\\');
  if (parts.length > 1 && parts[parts.length - 1] === '') parts.pop();
  return parts;
}

// fold upper-cases one character at a time, keeping a character whose upper
// case is longer (ß is SS): NTFS's upcase table maps each character to one.
function fold(name: string): string {
  return Array.from(name, (ch) => {
    const up = ch.toUpperCase();
    return up.length === ch.length ? up : ch;
  }).join('');
}
