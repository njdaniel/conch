// Starting conch-voice from Electron main (issue #948; linux-desktop.md §4.1,
// §5.3; issue #1785, windows-desktop.md §3.3, §3.7 for Windows): which
// executable may be run, the environment it gets, and the one way it is
// spawned.
//
// Input: the app's resources directory, main's own environment, the
// profile's configuration directory, the token, the nonce and the events
// socket's (Windows: pipe's) name. Output: the ChildProcess, or a typed
// refusal.
// Owns: the trusted-path checks on the bundled executable, the child's
// environment (built from empty by an allowlist, never filtered from main's),
// and the spawn options: stdio exactly ['pipe', 'pipe', 'pipe'], never
// detached, never a shell.
//
// What it keeps: the token and the nonce are only ever in the environment,
// never in argv; nothing here logs either. The environment is readable by
// same-user processes, which is outside the vault's at-rest guarantee
// (linux-desktop.md §4.1).
//
// Windows differs in three ways, each chosen by `platform` (default
// process.platform, a parameter so that the Windows rules are tested on
// Linux): the helper is conch-voice.exe and is checked by what Windows
// can say of a file (a regular file, not a link, inside the resources
// directory; the install directory's ACL is the trust, as for Electron's own
// binaries); the child's environment is built from empty by a
// case-insensitive allowlist (names are case-insensitive there, so a
// case-sensitive CONCH_ filter would let Conch_Token through); and the
// working directory is the system root, not /.
import {
  spawn as nodeSpawn,
  type ChildProcessByStdio,
  type ChildProcessWithoutNullStreams,
  type SpawnOptionsWithStdioTuple,
  type StdioNull,
  type StdioPipe,
} from 'node:child_process';
import { lstat, realpath } from 'node:fs/promises';
import { dirname, isAbsolute, join, sep } from 'node:path';
import type { Readable } from 'node:stream';

import {
  VOICE_CONTROL_PIPE_FLAG,
  VOICE_CONTROL_PIPE_NONCE_ENV,
  VOICE_CONTROL_SUBCOMMAND,
} from '../../../web/src/schema/generated.ts';

// HELPER_NAME is the bundled executable's file name in the resources
// directory (packaging, #774, puts it there).
export const HELPER_NAME = 'conch-voice';

// WINDOWS_HELPER_NAME is the same executable on Windows.
export const WINDOWS_HELPER_NAME = 'conch-voice.exe';

export function helperName(platform: NodeJS.Platform = process.platform): string {
  return platform === 'win32' ? WINDOWS_HELPER_NAME : HELPER_NAME;
}

const TOKEN_ENV = 'CONCH_TOKEN';
const CONFIG_DIR_ENV = 'CONCH_CONFIG_DIR';

// PASSED_THROUGH is every variable of main's environment the helper may see,
// by exact name: what it and its libraries need to find the user's home,
// PipeWire and the locale. None carries authority. Everything else, every
// CONCH_ variable, RUST_LOG, LD_PRELOAD and NODE_OPTIONS included, is absent
// by construction. The list is confirmed against a real session in #922 LE1.
export const PASSED_THROUGH: readonly string[] = [
  'HOME',
  'XDG_RUNTIME_DIR',
  'PIPEWIRE_RUNTIME_DIR',
  'PIPEWIRE_REMOTE',
  'LANG',
  'LC_ALL',
  'LC_CTYPE',
  'LC_MESSAGES',
];

// PASSED_THROUGH_WINDOWS is the same on Windows (windows-desktop.md §3.7),
// matched without regard to case: what the helper and its libraries need to
// find the system, the user's profile and the temporary directory. The list
// is confirmed at E1 by running the helper with exactly these. RUST_LOG and
// RUST_BACKTRACE are absent by construction.
export const PASSED_THROUGH_WINDOWS: readonly string[] = ['SYSTEMROOT', 'PATH', 'TEMP', 'TMP', 'USERPROFILE', 'APPDATA', 'LOCALAPPDATA', 'PROGRAMDATA', 'COMSPEC'];

// Why the helper cannot be run.
export type HelperRefusal = 'not-installed' | 'untrusted';

export class HelperError extends Error {
  constructor(
    readonly reason: HelperRefusal,
    readonly why: string,
  ) {
    super(`voice: the helper ${reason === 'not-installed' ? 'is not installed' : 'is not trusted'}: ${why}`);
  }
}

export interface FindDeps {
  // The uid the ownership checks compare against (a test stands in for
  // another user).
  readonly uid?: number;
  // The platform whose rules apply; process.platform by default.
  readonly platform?: NodeJS.Platform;
}

function codeOf(err: unknown): string {
  return typeof err === 'object' && err !== null && 'code' in err && typeof err.code === 'string' ? err.code : '';
}

function safeOwner(uid: number, own: number): boolean {
  return uid === own || uid === 0;
}

// findHelper returns the bundled executable to run: <resources>/conch-voice,
// an absolute path whose real location is inside the resources directory, a
// regular file (not a link), executable, owned by the user or root and not
// writable by group or others, in a directory with the same ownership and
// write rules. A missing file is not-installed; anything else that fails is
// untrusted, and nothing else is tried in its place.
// scripts/desktop-deb-lifecycle.sh check_helper mirrors these rules; change both.
export async function findHelper(resources: string, deps: FindDeps = {}): Promise<string> {
  if (!isAbsolute(resources) || resources.includes('\0')) throw new HelperError('untrusted', 'the resources directory is not an absolute path');
  if ((deps.platform ?? process.platform) === 'win32') return findHelperWindows(resources);
  const own = deps.uid ?? (typeof process.getuid === 'function' ? process.getuid() : -1);
  const candidate = join(resources, HELPER_NAME);
  let st;
  try {
    st = await lstat(candidate);
  } catch (err) {
    if (codeOf(err) === 'ENOENT' || codeOf(err) === 'ENOTDIR') throw new HelperError('not-installed', 'there is no voice helper in the app');
    throw new HelperError('untrusted', 'the voice helper cannot be read');
  }
  if (st.isSymbolicLink()) throw new HelperError('untrusted', 'the voice helper is a link');
  if (!st.isFile()) throw new HelperError('untrusted', 'the voice helper is not a regular file');
  if ((st.mode & 0o111) === 0) throw new HelperError('untrusted', 'the voice helper is not executable');
  if (!safeOwner(st.uid, own)) throw new HelperError('untrusted', 'the voice helper is not owned by you or by root');
  if ((st.mode & 0o022) !== 0) throw new HelperError('untrusted', 'the voice helper may be changed by other users');
  let real: string;
  let realResources: string;
  try {
    [real, realResources] = await Promise.all([realpath(candidate), realpath(resources)]);
  } catch {
    throw new HelperError('untrusted', 'the voice helper cannot be resolved');
  }
  if (!real.startsWith(realResources + sep)) throw new HelperError('untrusted', "the voice helper is not inside the app's resources");
  await safeAncestors(dirname(real), own);
  return real;
}

// findHelperWindows is findHelper on Windows: <resources>/conch-voice.exe, a
// regular file (not a symbolic link or junction, which lstat reports as a
// link) whose real location is inside the resources directory, compared
// without regard to case. Windows has no mode bits or uid to check, and no
// ACL check is possible from Node; the trust is the install directory's own
// ACL (Program Files, or the user's per-user install), the same trust as
// Electron's own binaries. A missing file is not-installed; anything else that
// fails is untrusted.
async function findHelperWindows(resources: string): Promise<string> {
  const candidate = join(resources, WINDOWS_HELPER_NAME);
  let st;
  try {
    st = await lstat(candidate);
  } catch (err) {
    if (codeOf(err) === 'ENOENT' || codeOf(err) === 'ENOTDIR') throw new HelperError('not-installed', 'there is no voice helper in the app');
    throw new HelperError('untrusted', 'the voice helper cannot be read');
  }
  if (st.isSymbolicLink()) throw new HelperError('untrusted', 'the voice helper is a link');
  if (!st.isFile()) throw new HelperError('untrusted', 'the voice helper is not a regular file');
  let real: string;
  let realResources: string;
  try {
    [real, realResources] = await Promise.all([realpath(candidate), realpath(resources)]);
  } catch {
    throw new HelperError('untrusted', 'the voice helper cannot be resolved');
  }
  if (!real.toLowerCase().startsWith((realResources + sep).toLowerCase())) throw new HelperError('untrusted', "the voice helper is not inside the app's resources");
  return real;
}

// safeAncestors checks every directory from dir up to /: each must be a
// directory owned by the user or root, and either closed to writes by group
// and others, or sticky and owned by root. Otherwise another user could
// rename a directory on the way and put another tree, or another
// conch-voice, where the checked one was between the check and the spawn.
//
// How this differs from internal/cli/voicehelper/find.go's trusted(): that
// walks the configured path one name at a time, following each link itself
// and checking every directory a link sits in or leads to, with no
// exception for sticky directories. Here the path is resolved by realpath
// first and the chain of real directories is walked; what is spawned is that
// real path, so a link on the way to it cannot be swapped afterwards to
// change what runs. And a root-owned sticky directory (/tmp, where an
// unpacked build or a test may live) is accepted: in it, other users may add
// entries but cannot rename or remove ours (unlink(2), "sticky bit").
async function safeAncestors(dir: string, own: number): Promise<void> {
  let at = dir;
  for (;;) {
    let st;
    try {
      st = await lstat(at);
    } catch {
      throw new HelperError('untrusted', 'a directory above the voice helper cannot be read');
    }
    const closed = (st.mode & 0o022) === 0;
    const rootSticky = (st.mode & 0o1000) !== 0 && st.uid === 0;
    if (!st.isDirectory() || !safeOwner(st.uid, own) || !(closed || rootSticky)) {
      throw new HelperError('untrusted', 'the voice helper is under a directory other users can change');
    }
    const up = dirname(at);
    if (up === at) return;
    at = up;
  }
}

export interface HelperEnvInput {
  // main's own environment, read only for the names in PASSED_THROUGH.
  readonly parent: NodeJS.ProcessEnv;
  // The profile's configuration directory: device settings never cross
  // accounts.
  readonly configDir: string;
  readonly token: string;
  readonly nonce: string;
  // The platform whose rules apply; process.platform by default.
  readonly platform?: NodeJS.Platform;
}

// WINDOWS_ABSOLUTE is an absolute Windows path: a drive and a separator, or a
// UNC prefix. A rooted path without a drive ("\\foo") is relative to the
// current drive and is refused.
const WINDOWS_ABSOLUTE = /^(?:[A-Za-z]:[\\/]|\\\\[^\\/?.])/;

function absoluteConfigDir(configDir: string, platform: NodeJS.Platform): boolean {
  if (configDir.includes('\0')) return false;
  return platform === 'win32' ? WINDOWS_ABSOLUTE.test(configDir) : isAbsolute(configDir);
}

// baseEnv is the allowlisted names main has and CONCH_CONFIG_DIR: what every
// run of the helper gets, whatever it is for. On Windows the names are
// matched without regard to case and set in the allowlist's own spelling, so
// that no two spellings of a name can both be present.
function baseEnv(parent: NodeJS.ProcessEnv, configDir: string, platform: NodeJS.Platform): Record<string, string> {
  const env: Record<string, string> = Object.create(null) as Record<string, string>;
  if (platform === 'win32') {
    for (const [name, value] of Object.entries(parent)) {
      const canonical = PASSED_THROUGH_WINDOWS.find((n) => n === name.toUpperCase());
      if (canonical === undefined || canonical in env) continue;
      if (value !== undefined && value !== '' && !value.includes('\0')) env[canonical] = value;
    }
  } else {
    for (const name of PASSED_THROUGH) {
      const value = parent[name];
      if (value !== undefined && value !== '' && !value.includes('\0')) env[name] = value;
    }
  }
  if (!absoluteConfigDir(configDir, platform)) throw new Error('voice: the configuration directory must be an absolute path');
  env[CONFIG_DIR_ENV] = configDir;
  return env;
}

// oneShotEnv is the environment of `devices` and `config set` (issue #1378):
// the same allowlist and the profile's CONCH_CONFIG_DIR, with no CONCH_TOKEN
// and no nonce, because neither command needs a login or an events endpoint.
export function oneShotEnv(parent: NodeJS.ProcessEnv, configDir: string, platform: NodeJS.Platform = process.platform): Record<string, string> {
  return baseEnv(parent, configDir, platform);
}

// helperEnv builds the child's environment from empty: the allowlisted
// names main has, then CONCH_CONFIG_DIR, CONCH_TOKEN (the start says
// credential env) and CONCH_VOICE_ATTACH, and nothing else. A value holding
// a NUL is left out rather than truncated.
export function helperEnv(input: HelperEnvInput): Record<string, string> {
  const env = baseEnv(input.parent, input.configDir, input.platform ?? process.platform);
  if (input.token.trim() === '' || input.token.includes('\0')) throw new Error('voice: no credential to hand to the helper');
  env[TOKEN_ENV] = input.token;
  env[VOICE_CONTROL_PIPE_NONCE_ENV] = input.nonce;
  return env;
}

// helperArgs is the command line: `managed --events-pipe <name>` and the
// caller's extra helper options. The name is a socket path on Linux and a
// pipe name on Windows; neither is a secret. None may hold the token or the
// nonce.
export function helperArgs(endpointName: string, extra: readonly string[], secrets: readonly string[]): string[] {
  const args = [VOICE_CONTROL_SUBCOMMAND, VOICE_CONTROL_PIPE_FLAG, endpointName, ...extra];
  for (const a of args) {
    if (a.includes('\0') || secrets.some((s) => s !== '' && a.includes(s))) throw new Error('voice: a secret may not be on the command line');
  }
  return args;
}

export type SpawnFn = (
  command: string,
  args: readonly string[],
  options: SpawnOptionsWithStdioTuple<StdioPipe, StdioPipe, StdioPipe>,
) => ChildProcessWithoutNullStreams;

// spawnOptions are the only options the helper is ever started with: three
// pipes (libuv's socketpairs; no fourth descriptor, which the helper would
// refuse), not detached (no setsid: the helper is in main's session and sees
// main as its parent), no shell, the environment built above, and / as the
// working directory so that nothing resolves against main's.
//
// On Windows the working directory is the system root (a directory ordinary
// users cannot write to) and the pipes are libuv's named pipes.
export function spawnOptions(env: Record<string, string>, platform: NodeJS.Platform = process.platform): SpawnOptionsWithStdioTuple<StdioPipe, StdioPipe, StdioPipe> {
  return { stdio: ['pipe', 'pipe', 'pipe'], detached: false, shell: false, windowsHide: true, env, cwd: workingDir(env, platform) };
}

// workingDir is / (Linux, macOS) or, on Windows, the system root from the
// environment being passed, which must be a drive-absolute path: there is no
// guessed C:\Windows, and a spawn without one fails.
function workingDir(env: Record<string, string>, platform: NodeJS.Platform): string {
  if (platform !== 'win32') return '/';
  const root = env['SYSTEMROOT'];
  if (root === undefined || !/^[A-Za-z]:[\\/]/.test(root)) throw new Error('voice: no usable SYSTEMROOT for the helper');
  return root;
}

// OneShotChild is a one-shot run's process: no standard input to write to.
export type OneShotChild = ChildProcessByStdio<null, Readable, Readable>;

export type OneShotSpawnFn = (command: string, args: readonly string[], options: SpawnOptionsWithStdioTuple<StdioNull, StdioPipe, StdioPipe>) => OneShotChild;

// oneShotSpawnOptions: as spawnOptions, but standard input is /dev/null
// ('ignore'), so that the run can wait for nothing from main.
export function oneShotSpawnOptions(env: Record<string, string>, platform: NodeJS.Platform = process.platform): SpawnOptionsWithStdioTuple<StdioNull, StdioPipe, StdioPipe> {
  return { stdio: ['ignore', 'pipe', 'pipe'], detached: false, shell: false, windowsHide: true, env, cwd: workingDir(env, platform) };
}

export const defaultOneShotSpawn: OneShotSpawnFn = (command, args, options) => nodeSpawn(command, args, options);

export function spawnHelper(
  spawnFn: SpawnFn,
  executable: string,
  args: readonly string[],
  env: Record<string, string>,
  platform: NodeJS.Platform = process.platform,
): ChildProcessWithoutNullStreams {
  return spawnFn(executable, args, spawnOptions(env, platform));
}

export const defaultSpawn: SpawnFn = (command, args, options) => nodeSpawn(command, args, options);

// How long the screen-cast portal may take to answer (portal.ts, #1580): it is
// D-Bus activated, so the first ask may start it.
export const PORTAL_TIMEOUT_MS = 3_000;
const PORTAL_MAX_OUTPUT = 4096;

export interface RunResult {
  readonly code: number | null;
  readonly stdout: string;
}

// runFile runs one read-only program for portal.ts: file with args and env,
// never a shell, bounded in time and in output, stderr dropped. It resolves
// with the exit code and stdout, or null when the program could not be run.
// It lives here because spawn.ts is the one place that reaches child_process.
export function runFile(file: string, args: readonly string[], env: Readonly<Record<string, string>>): Promise<RunResult | null> {
  return new Promise((resolve) => {
    let out = '';
    let settled = false;
    const done = (value: RunResult | null): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve(value);
    };
    let child: ChildProcessByStdio<null, Readable, Readable>;
    try {
      child = nodeSpawn(file, [...args], { env: { ...env }, stdio: ['ignore', 'pipe', 'pipe'] });
    } catch {
      resolve(null);
      return;
    }
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      done(null);
    }, PORTAL_TIMEOUT_MS + 500);
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (chunk: string) => {
      out += chunk;
      if (out.length > PORTAL_MAX_OUTPUT) {
        child.kill('SIGKILL');
        done(null);
      }
    });
    child.stderr.resume();
    child.on('error', () => {
      done(null);
    });
    child.on('close', (code) => {
      done({ code, stdout: out });
    });
  });
}

// WatchChild is a long-lived read-only child: no standard input, stdout read
// line by line, stderr dropped.
export type WatchChild = OneShotChild;

// spawnWatch starts a long-lived read-only program (the logind monitor of
// issue #1588): argv only, no shell, not detached, standard input /dev/null.
// The caller owns the ChildProcess and kills it by that handle, never by
// name.
export function spawnWatch(file: string, args: readonly string[], env: Readonly<Record<string, string>>): WatchChild {
  return nodeSpawn(file, [...args], { env: { ...env }, stdio: ['ignore', 'pipe', 'pipe'], detached: false, shell: false, windowsHide: true, cwd: '/' });
}
