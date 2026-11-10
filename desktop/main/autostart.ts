// Launch at login on Linux (issue #1587, linux-desktop.md §9). Input: whether
// this is a packaged build, its executable's absolute path and the XDG config
// directory. Output: at most one file, $XDG_CONFIG_HOME/autostart/
// chat.conch.desktop.desktop (~/.config when the variable is unset), which
// carries a marker key.
//
// Electron's app.setLoginItemSettings is documented for macOS and Windows
// only, so main writes the standard XDG autostart entry itself. Rules:
//   - only a packaged build writes it; an unpackaged run refuses and says why;
//   - the executable must sit where only the user or the system can change it:
//     its real path is a regular file and no ancestor is writable by group or
//     others or owned by someone else than root or the user (a `dir` build can
//     be unpacked anywhere, /tmp included, and a path another user can
//     recreate after a reboot would run as the user at login). Unlike the data
//     root's rule (instance.ts) there is no sticky-bit allowance: sticky does
//     not stop the directory going away;
//   - Exec= is the executable's path alone, with no argument: a start at login
//     opens the app as any launch does, text first, no helper, no voice;
//   - turning it off removes only a file that carries the marker, and a file
//     Conch did not write is never overwritten or removed.
import { randomBytes } from 'node:crypto';
import { lstat, mkdir, open, readFile, realpath, rename, rm, stat } from 'node:fs/promises';
import { dirname, isAbsolute, join } from 'node:path';

// The app id of desktop/build/electron-builder.config.json.
export const APP_ID = 'chat.conch.desktop';
export const MARKER_KEY = 'X-Conch-Autostart';
const MARKER_LINE = `${MARKER_KEY}=true`;
const MAX_BYTES = 16 * 1024;

export type AutostartRefusal = 'unpackaged' | 'unsafe-location' | 'foreign-file' | 'failed';
export type AutostartResult = { readonly ok: true } | { readonly ok: false; readonly reason: AutostartRefusal };

export interface AutostartDeps {
  readonly packaged: boolean;
  // process.execPath: the packaged executable.
  readonly execPath: string;
  readonly env: Readonly<Record<string, string | undefined>>;
  readonly home: string;
  // Test seam, see trustedTarget.
  readonly trustedAbove?: string;
  readonly log: (line: string) => void;
}

export function autostartPath(env: Readonly<Record<string, string | undefined>>, home: string): string {
  const configured = env['XDG_CONFIG_HOME'];
  // The XDG rule: a relative or empty value is ignored.
  const base = configured !== undefined && isAbsolute(configured) ? configured : join(home, '.config');
  return join(base, 'autostart', `${APP_ID}.desktop`);
}

// An Exec argument, quoted as the Desktop Entry specification asks: in double
// quotes, with `"`, backtick, `$` and `\` escaped, and `%` doubled. A path
// with a control character is refused (null).
export function execValue(path: string): string | null {
  if (!isAbsolute(path) || /[\u0000-\u001f\u007f]/u.test(path)) return null;
  const escaped = path.replace(/[\\"`$]/gu, (c) => `\\${c}`).replace(/%/gu, '%%');
  // The specification wants the backslash doubled once more inside a string.
  return `"${escaped.replace(/\\/gu, '\\\\')}"`;
}

export function entryText(execPath: string): string | null {
  const exec = execValue(execPath);
  if (exec === null) return null;
  return ['[Desktop Entry]', 'Type=Application', 'Name=Conch', 'Comment=Open Conch when you log in', `Exec=${exec}`, 'Terminal=false', MARKER_LINE, ''].join('\n');
}

// Whether a file's text is one this module wrote: the marker as a line of its
// own.
export function carriesMarker(text: string): boolean {
  return text.split('\n').some((line) => line.trim() === MARKER_LINE);
}

function isCode(err: unknown, code: string): boolean {
  return typeof err === 'object' && err !== null && (err as { code?: unknown }).code === code;
}

// trustedTarget: the real path of the executable, when it is a regular file
// that only root or the current user owns and can write, and every ancestor is
// a directory with the same rule; null otherwise. The entry names this real
// path, never a symlink to it: a link in a directory others can change could be
// swapped after the check.
//
// above is a test seam: ancestors from that real directory upward are not
// judged (a test tree lives under a sticky, world-writable /tmp). Production
// never passes it.
export async function trustedTarget(execPath: string, above?: string): Promise<string | null> {
  try {
    const real = await realpath(execPath);
    if (execValue(real) === null) return null;
    const uid = typeof process.getuid === 'function' ? process.getuid() : null;
    const owned = (st: { mode: number; uid: number }): boolean => (st.mode & 0o022) === 0 && (st.uid === 0 || st.uid === uid);
    const file = await stat(real);
    if (!file.isFile() || !owned(file)) return null;
    const stopAt = above === undefined ? undefined : await realpath(above);
    for (let dir = dirname(real); dir !== stopAt; dir = dirname(dir)) {
      const st = await stat(dir);
      if (!st.isDirectory() || !owned(st)) return null;
      if (dirname(dir) === dir) break;
    }
    return real;
  } catch {
    return null;
  }
}

type Existing = 'absent' | 'ours' | 'foreign';

export class Autostart {
  readonly #deps: AutostartDeps;
  readonly #file: string;

  constructor(deps: AutostartDeps) {
    this.#deps = deps;
    this.#file = autostartPath(deps.env, deps.home);
  }

  // Why this build may not write the entry, or null when it may.
  async availability(): Promise<'unpackaged' | 'unsafe-location' | null> {
    if (!this.#deps.packaged) return 'unpackaged';
    if (process.platform !== 'linux' || execValue(this.#deps.execPath) === null) return 'unsafe-location';
    return (await trustedTarget(this.#deps.execPath, this.#deps.trustedAbove)) === null ? 'unsafe-location' : null;
  }

  async #existing(): Promise<Existing> {
    try {
      const st = await lstat(this.#file);
      if (!st.isFile() || st.size > MAX_BYTES) return 'foreign';
      return carriesMarker(await readFile(this.#file, 'utf8')) ? 'ours' : 'foreign';
    } catch (err) {
      if (isCode(err, 'ENOENT')) return 'absent';
      throw err;
    }
  }

  async enable(): Promise<AutostartResult> {
    const why = await this.availability();
    if (why === 'unpackaged') {
      this.#deps.log('conch-desktop: launch at login is only for an installed build; an unpackaged run does not write an autostart entry');
      return { ok: false, reason: 'unpackaged' };
    }
    if (why !== null) {
      this.#deps.log('conch-desktop: launch at login needs the app installed where only you or the system can change it; no autostart entry was written');
      return { ok: false, reason: 'unsafe-location' };
    }
    // The real path, checked again here, so the entry names what was judged.
    const real = await trustedTarget(this.#deps.execPath, this.#deps.trustedAbove);
    const text = real === null ? null : entryText(real);
    if (text === null) return { ok: false, reason: 'failed' };
    try {
      if ((await this.#existing()) === 'foreign') {
        this.#deps.log(`conch-desktop: ${this.#file} exists and was not written by Conch; it is left alone`);
        return { ok: false, reason: 'foreign-file' };
      }
      await mkdir(dirname(this.#file), { recursive: true });
      const tmp = `${this.#file}.${randomBytes(8).toString('hex')}.tmp`;
      try {
        const fh = await open(tmp, 'wx', 0o600);
        try {
          await fh.writeFile(text, 'utf8');
          await fh.sync();
        } finally {
          await fh.close();
        }
        await rename(tmp, this.#file);
      } catch (err) {
        await rm(tmp, { force: true });
        throw err;
      }
      return { ok: true };
    } catch {
      this.#deps.log('conch-desktop: the autostart entry could not be written');
      return { ok: false, reason: 'failed' };
    }
  }

  async disable(): Promise<AutostartResult> {
    try {
      switch (await this.#existing()) {
        case 'absent':
          return { ok: true };
        case 'foreign':
          this.#deps.log(`conch-desktop: ${this.#file} was not written by Conch; it is left alone`);
          return { ok: false, reason: 'foreign-file' };
        case 'ours':
          await rm(this.#file, { force: true });
          return { ok: true };
      }
    } catch {
      this.#deps.log('conch-desktop: the autostart entry could not be removed');
      return { ok: false, reason: 'failed' };
    }
  }
}
