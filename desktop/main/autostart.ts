// Launch at login on Linux (issue #1587, linux-desktop.md §9). Input: whether
// this is a packaged build, its executable's absolute path and the XDG config
// directory. Output: at most one file, $XDG_CONFIG_HOME/autostart/
// chat.conch.desktop.desktop (~/.config when the variable is unset), which
// carries a marker key.
//
// Electron's app.setLoginItemSettings is documented for macOS and Windows
// only, so main writes the standard XDG autostart entry itself. Rules:
//   - only a packaged build writes it; an unpackaged run refuses and says why;
//   - Exec= is the executable's path alone, with no argument: a start at login
//     opens the app as any launch does, text first, no helper, no voice;
//   - turning it off removes only a file that carries the marker, and a file
//     Conch did not write is never overwritten or removed.
import { randomBytes } from 'node:crypto';
import { lstat, mkdir, open, readFile, rename, rm } from 'node:fs/promises';
import { dirname, isAbsolute, join } from 'node:path';

// The app id of desktop/build/electron-builder.config.json.
export const APP_ID = 'chat.conch.desktop';
export const MARKER_KEY = 'X-Conch-Autostart';
const MARKER_LINE = `${MARKER_KEY}=true`;
const MAX_BYTES = 16 * 1024;

export type AutostartRefusal = 'unpackaged' | 'foreign-file' | 'failed';
export type AutostartResult = { readonly ok: true } | { readonly ok: false; readonly reason: AutostartRefusal };

export interface AutostartDeps {
  readonly packaged: boolean;
  // process.execPath: the packaged executable.
  readonly execPath: string;
  readonly env: Readonly<Record<string, string | undefined>>;
  readonly home: string;
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

type Existing = 'absent' | 'ours' | 'foreign';

export class Autostart {
  readonly #deps: AutostartDeps;
  readonly #file: string;

  constructor(deps: AutostartDeps) {
    this.#deps = deps;
    this.#file = autostartPath(deps.env, deps.home);
  }

  // Whether this build may write the entry at all.
  available(): boolean {
    return this.#deps.packaged && process.platform === 'linux' && execValue(this.#deps.execPath) !== null;
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
    if (!this.#deps.packaged) {
      this.#deps.log('conch-desktop: launch at login is only for an installed build; an unpackaged run does not write an autostart entry');
      return { ok: false, reason: 'unpackaged' };
    }
    const text = entryText(this.#deps.execPath);
    if (text === null || !this.available()) return { ok: false, reason: 'failed' };
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
