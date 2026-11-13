// Where a --user-data-dir may point on Windows (issue #1788). The environment,
// the switches and the realpath are injected, and the paths are judged with
// path.win32, so this holds on Linux as on Windows (vitest.windows.config.ts).
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { ALLOW_SHARED_SWITCH, dataRootVerdict, isInside, qualified, REFUSED_LINE, SHARED_LINE, USER_DATA_SWITCH, type DataRootVerdict } from '../../main/data-root.ts';

const ENV = {
  USERPROFILE: 'C:\\Users\\nick',
  APPDATA: 'C:\\Users\\nick\\AppData\\Roaming',
  LOCALAPPDATA: 'C:\\Users\\nick\\AppData\\Local',
};

function enoent(path: string): Error {
  return Object.assign(new Error(`ENOENT: no such file or directory, realpath '${path}'`), { code: 'ENOENT' });
}

// A file system of existing directories, matched case-insensitively, with
// links (junctions or symbolic links) from one path to another. realpath
// follows links at every level, as fs.realpathSync.native does, and gives the
// stored spelling.
function fakeRealpath(dirs: readonly string[], links: Readonly<Record<string, string>> = {}): (path: string) => string {
  const key = (p: string) => p.toLowerCase().replace(/\\+$/, '');
  const known = new Map(dirs.map((d) => [key(d), d]));
  const linked = new Map(Object.entries(links).map(([from, to]) => [key(from), to]));
  return (path) => {
    const parts = path.split('\\');
    let at = '';
    for (const [i, part] of parts.entries()) {
      at = i === 0 ? part : `${at}\\${part}`;
      const to = linked.get(key(at));
      if (to !== undefined) at = to;
      else if (i > 0 && !known.has(key(at))) throw enoent(path);
      else if (i > 0) at = known.get(key(at)) ?? at;
    }
    return at;
  };
}

const PROFILE_DIRS = ['C:\\Users', 'C:\\Users\\nick', 'C:\\Users\\nick\\AppData', 'C:\\Users\\nick\\AppData\\Roaming', 'C:\\Users\\nick\\AppData\\Local', 'C:\\Users\\other', 'C:\\Users\\nickx'];

interface Case {
  readonly name: string;
  readonly root: string;
  readonly want: DataRootVerdict;
  readonly platform?: NodeJS.Platform;
  readonly switches?: readonly string[];
  readonly env?: Readonly<Record<string, string | undefined>>;
  readonly realpath?: (path: string) => string;
}

const CASES: readonly Case[] = [
  // Inside one of the three.
  { name: 'inside %USERPROFILE%', root: 'C:\\Users\\nick\\conch-test', want: 'inside' },
  { name: '%USERPROFILE% itself', root: 'C:\\Users\\nick', want: 'inside' },
  { name: 'inside %APPDATA%', root: 'C:\\Users\\nick\\AppData\\Roaming\\conch-desktop', want: 'inside' },
  { name: 'inside %LOCALAPPDATA%', root: 'C:\\Users\\nick\\AppData\\Local\\conch\\test', want: 'inside' },
  { name: 'inside %APPDATA% when the profile variable is missing', root: 'C:\\Users\\nick\\AppData\\Roaming\\x', env: { APPDATA: ENV.APPDATA }, want: 'inside' },
  { name: 'forward slashes', root: 'C:/Users/nick/AppData/Local/conch', want: 'inside' },
  { name: 'a trailing separator on the base', root: 'C:\\Users\\nick\\conch', env: { USERPROFILE: 'C:\\Users\\nick\\' }, want: 'inside' },
  { name: '.. that stays inside', root: 'C:\\Users\\nick\\AppData\\..\\conch', want: 'inside' },

  // Case differences: Windows names are compared case-insensitively.
  { name: 'a root in another case', root: 'c:\\USERS\\Nick\\appdata\\local\\conch', want: 'inside' },
  { name: 'a base in another case', root: 'C:\\Users\\nick\\conch', env: { USERPROFILE: 'c:\\users\\NICK' }, want: 'inside' },

  // Outside.
  { name: 'a drive root', root: 'C:\\conch-data', want: 'refused' },
  { name: 'the drive itself', root: 'C:\\', want: 'refused' },
  { name: 'another drive', root: 'D:\\Users\\nick\\conch', want: 'refused' },
  { name: "another user's profile", root: 'C:\\Users\\other\\AppData\\Roaming\\conch', want: 'refused' },
  { name: 'the Users directory', root: 'C:\\Users\\conch', want: 'refused' },
  { name: 'the prefix trap', root: 'C:\\Users\\nickx', want: 'refused' },
  { name: 'the prefix trap, below', root: 'C:\\Users\\nickx\\conch', want: 'refused' },
  { name: 'the prefix trap on %APPDATA%', root: 'C:\\Users\\nick\\AppData\\RoamingX\\conch', env: { APPDATA: ENV.APPDATA }, want: 'refused' },
  { name: '.. that leaves the profile', root: 'C:\\Users\\nick\\..\\other\\conch', want: 'refused' },
  { name: 'a UNC share', root: '\\\\server\\share\\conch', want: 'refused' },
  { name: 'ProgramData', root: 'C:\\ProgramData\\conch', want: 'refused' },

  // Relative paths name no one place.
  { name: 'a relative path', root: 'conch-data', want: 'refused' },
  { name: 'a relative path below the profile name', root: 'Users\\nick\\conch', want: 'refused' },
  { name: 'a drive-relative path', root: 'C:conch-data', want: 'refused' },
  { name: 'a root-relative path', root: '\\Users\\nick\\conch', want: 'refused' },

  // Bases that cannot be used.
  { name: 'no profile variables at all', root: 'C:\\Users\\nick\\conch', env: {}, want: 'refused' },
  { name: 'empty profile variables', root: 'C:\\Users\\nick\\conch', env: { USERPROFILE: '', APPDATA: '', LOCALAPPDATA: '' }, want: 'refused' },
  { name: 'a relative %USERPROFILE%', root: 'C:\\Users\\nick\\conch', env: { USERPROFILE: 'nick' }, want: 'refused' },
  { name: 'a drive-root %USERPROFILE%', root: 'C:\\conch-data', env: { USERPROFILE: 'C:' }, want: 'refused' },

  // Links are resolved before the comparison.
  {
    name: 'a junction inside the profile that leads to a drive root',
    root: 'C:\\Users\\nick\\AppData\\Local\\conch',
    realpath: fakeRealpath([...PROFILE_DIRS, 'C:\\conch-data'], { 'C:\\Users\\nick\\AppData\\Local\\conch': 'C:\\conch-data' }),
    want: 'refused',
  },
  {
    name: 'a junction above a root that does not exist yet',
    root: 'C:\\Users\\nick\\shared\\conch\\new',
    realpath: fakeRealpath([...PROFILE_DIRS, 'C:\\conch-data'], { 'C:\\Users\\nick\\shared': 'C:\\conch-data' }),
    want: 'refused',
  },
  {
    name: 'a link from a drive root into the profile',
    root: 'C:\\conch-link',
    realpath: fakeRealpath([...PROFILE_DIRS, 'C:\\Users\\nick\\AppData\\Local\\conch'], { 'C:\\conch-link': 'C:\\Users\\nick\\AppData\\Local\\conch' }),
    want: 'inside',
  },
  {
    name: 'a redirected %APPDATA% (a junction to where the files are)',
    root: 'C:\\Users\\nick\\AppData\\Roaming\\conch-desktop',
    env: { APPDATA: ENV.APPDATA },
    realpath: fakeRealpath(['C:\\Users', 'C:\\Users\\nick', 'C:\\Users\\nick\\AppData', 'D:\\Redirected', 'D:\\Redirected\\nick', 'D:\\Redirected\\nick\\conch-desktop'], {
      'C:\\Users\\nick\\AppData\\Roaming': 'D:\\Redirected\\nick',
    }),
    want: 'inside',
  },
  {
    name: 'a resolved root in the verbatim form',
    root: 'C:\\Users\\nick\\conch',
    realpath: (p) => `\\\\?\\${p}`,
    want: 'inside',
  },
  {
    name: 'a resolved root in the verbatim UNC form, outside',
    root: 'C:\\Users\\nick\\conch',
    env: { USERPROFILE: 'C:\\Users\\nick' },
    realpath: (p) => (p.toLowerCase().startsWith('c:\\users\\nick\\conch') ? '\\\\?\\UNC\\server\\share\\conch' : p),
    want: 'refused',
  },
  {
    name: 'a root that cannot be resolved (not ENOENT)',
    root: 'C:\\Users\\nick\\conch',
    realpath: (p) => {
      throw Object.assign(new Error(`EACCES: ${p}`), { code: 'EACCES' });
    },
    want: 'refused',
  },

  // The override.
  { name: 'a drive root with the override', root: 'C:\\conch-data', switches: [USER_DATA_SWITCH, ALLOW_SHARED_SWITCH], want: 'allowed' },
  { name: 'a relative path with the override', root: 'conch-data', switches: [USER_DATA_SWITCH, ALLOW_SHARED_SWITCH], want: 'allowed' },
  { name: 'inside the profile with the override', root: 'C:\\Users\\nick\\conch', switches: [USER_DATA_SWITCH, ALLOW_SHARED_SWITCH], want: 'inside' },

  // No --user-data-dir: Electron's own %APPDATA%\conch-desktop, not checked.
  { name: 'no --user-data-dir', root: 'C:\\conch-data', switches: [], want: 'unchecked' },
  { name: 'only the override', root: 'C:\\conch-data', switches: [ALLOW_SHARED_SWITCH], want: 'unchecked' },

  // Linux and macOS are unchanged: prepareRoot judges the root there.
  { name: 'Linux', platform: 'linux', root: '/tmp/conch-data', want: 'unchecked' },
  { name: 'Linux, a Windows-looking root', platform: 'linux', root: 'C:\\conch-data', want: 'unchecked' },
  { name: 'macOS', platform: 'darwin', root: '/Users/Shared/conch', want: 'unchecked' },
];

describe('dataRootVerdict', () => {
  it.each(CASES)('$name: $want', (c) => {
    const lines: string[] = [];
    const switches = c.switches ?? [USER_DATA_SWITCH];
    const asked: string[] = [];
    const verdict = dataRootVerdict(
      {
        platform: c.platform ?? 'win32',
        hasSwitch: (name) => {
          asked.push(name);
          return switches.includes(name);
        },
        root: c.root,
        env: c.env ?? ENV,
        // With no links, every path resolves to itself, as a path that exists
        // and holds no link does.
        realpath: c.realpath ?? ((p) => p),
      },
      (line) => lines.push(line),
    );
    expect(verdict).toBe(c.want);
    expect(lines).toEqual(c.want === 'refused' ? [REFUSED_LINE] : c.want === 'allowed' ? [SHARED_LINE] : []);
    // Off Windows nothing is asked, not even the switches.
    if ((c.platform ?? 'win32') !== 'win32') expect(asked).toEqual([]);
  });

  it('runs on a root that does not exist yet, through its nearest existing ancestor', () => {
    const verdict = dataRootVerdict(
      { platform: 'win32', hasSwitch: (n) => n === USER_DATA_SWITCH, root: 'C:\\Users\\nick\\AppData\\Local\\conch\\a\\b', env: ENV, realpath: fakeRealpath(PROFILE_DIRS) },
      () => undefined,
    );
    expect(verdict).toBe('inside');
  });

  it('names the rule and the override when it refuses, and the risk when overridden', () => {
    expect(REFUSED_LINE).toContain('must be under your user profile');
    expect(REFUSED_LINE).toContain(`--${ALLOW_SHARED_SWITCH}`);
    expect(SHARED_LINE).toContain('other local accounts may be able to change');
    expect(SHARED_LINE).toContain("the voice helper's settings");
    // One line each.
    expect(REFUSED_LINE).not.toContain('\n');
    expect(SHARED_LINE).not.toContain('\n');
  });
});

describe('isInside', () => {
  it.each<[string, string, boolean]>([
    ['C:\\Users\\nick', 'C:\\Users\\nick', true],
    ['C:\\Users\\nick\\a', 'C:\\Users\\nick', true],
    ['C:\\Users\\nick\\', 'C:\\Users\\nick', true],
    ['C:\\Users\\nickx', 'C:\\Users\\nick', false],
    ['C:\\Users\\nic', 'C:\\Users\\nick', false],
    ['C:\\Users', 'C:\\Users\\nick', false],
    ['c:\\users\\NICK\\a', 'C:\\Users\\nick', true],
    ['D:\\Users\\nick', 'C:\\Users\\nick', false],
    ['\\\\server\\share\\nick\\a', '\\\\server\\share\\nick', true],
    ['\\\\server\\share\\nickx', '\\\\server\\share\\nick', false],
    // ß upper-cases to two letters; NTFS keeps it one, and distinct from SS.
    ['C:\\Users\\SS', 'C:\\Users\\ß', false],
    ['C:\\Users\\Ä\\a', 'C:\\Users\\ä', true],
  ])('%s inside %s: %s', (child, parent, want) => {
    expect(isInside(child, parent)).toBe(want);
  });
});

describe('qualified', () => {
  it.each<[string, boolean]>([
    ['C:\\Users\\nick', true],
    ['c:/Users/nick', true],
    ['\\\\server\\share', true],
    ['C:', false],
    ['C:nick', false],
    ['\\Users\\nick', false],
    ['nick', false],
    ['', false],
  ])('%j: %s', (path, want) => {
    expect(qualified(path)).toBe(want);
  });
});

describe('the call in app.ts', () => {
  const code = readFileSync(join(import.meta.dirname, '..', '..', 'main', 'app.ts'), 'utf8').replace(/^\s*\/\/.*$/gm, '');

  it('comes before the single-instance lock, the first write to the root, and exits on a refusal', () => {
    const at = code.indexOf('dataRootVerdict(');
    expect(at).toBeGreaterThanOrEqual(0);
    expect(code.indexOf('acquire(')).toBeGreaterThan(at);
    expect(code.indexOf('prepareRoot(')).toBeGreaterThan(at);
    expect(code.slice(at, code.indexOf('acquire('))).toMatch(/=== 'refused'\) \{\s*app\.exit\(1\);\s*return;/);
    expect(code.split('dataRootVerdict(').length - 1).toBe(1);
  });
});
