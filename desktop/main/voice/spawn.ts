// Starting conch-voice from Electron main (issue #948; linux-desktop.md §4.1,
// §5.3): which executable may be run, the environment it gets, and the one
// way it is spawned.
//
// Input: the app's resources directory, main's own environment, the
// profile's configuration directory, the token, the nonce and the events
// socket's path. Output: the ChildProcess, or a typed refusal.
// Owns: the trusted-path checks on the bundled executable, the child's
// environment (built from empty by an allowlist, never filtered from main's),
// and the spawn options: stdio exactly ['pipe', 'pipe', 'pipe'], never
// detached, never a shell.
//
// What it keeps: the token and the nonce are only ever in the environment,
// never in argv; nothing here logs either. The environment is readable by
// same-user processes, which is outside the vault's at-rest guarantee
// (linux-desktop.md §4.1).
import { spawn as nodeSpawn, type ChildProcessWithoutNullStreams, type SpawnOptionsWithStdioTuple, type StdioPipe } from 'node:child_process';
import { lstat, realpath } from 'node:fs/promises';
import { dirname, isAbsolute, join, sep } from 'node:path';

import {
  VOICE_CONTROL_PIPE_FLAG,
  VOICE_CONTROL_PIPE_NONCE_ENV,
  VOICE_CONTROL_SUBCOMMAND,
} from '../../../web/src/schema/generated.ts';

// HELPER_NAME is the bundled executable's file name in the resources
// directory (packaging, #774, puts it there).
export const HELPER_NAME = 'conch-voice';

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
export async function findHelper(resources: string, deps: FindDeps = {}): Promise<string> {
  if (!isAbsolute(resources) || resources.includes('\0')) throw new HelperError('untrusted', 'the resources directory is not an absolute path');
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
  const dir = await lstat(dirname(real));
  if (!dir.isDirectory() || !safeOwner(dir.uid, own) || (dir.mode & 0o022) !== 0) {
    throw new HelperError('untrusted', 'the voice helper is in a directory other users can change');
  }
  return real;
}

export interface HelperEnvInput {
  // main's own environment, read only for the names in PASSED_THROUGH.
  readonly parent: NodeJS.ProcessEnv;
  // The profile's configuration directory: device settings never cross
  // accounts.
  readonly configDir: string;
  readonly token: string;
  readonly nonce: string;
}

// helperEnv builds the child's environment from empty: the allowlisted
// names main has, then CONCH_CONFIG_DIR, CONCH_TOKEN (the start says
// credential env) and CONCH_VOICE_ATTACH, and nothing else. A value holding
// a NUL is left out rather than truncated.
export function helperEnv(input: HelperEnvInput): Record<string, string> {
  const env: Record<string, string> = Object.create(null) as Record<string, string>;
  for (const name of PASSED_THROUGH) {
    const value = input.parent[name];
    if (value !== undefined && value !== '' && !value.includes('\0')) env[name] = value;
  }
  if (!isAbsolute(input.configDir) || input.configDir.includes('\0')) throw new Error('voice: the configuration directory must be an absolute path');
  if (input.token.trim() === '' || input.token.includes('\0')) throw new Error('voice: no credential to hand to the helper');
  env[CONFIG_DIR_ENV] = input.configDir;
  env[TOKEN_ENV] = input.token;
  env[VOICE_CONTROL_PIPE_NONCE_ENV] = input.nonce;
  return env;
}

// helperArgs is the command line: `managed --events-pipe <path>` and the
// caller's extra helper options. None may hold the token or the nonce.
export function helperArgs(socketPath: string, extra: readonly string[], secrets: readonly string[]): string[] {
  const args = [VOICE_CONTROL_SUBCOMMAND, VOICE_CONTROL_PIPE_FLAG, socketPath, ...extra];
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
export function spawnOptions(env: Record<string, string>): SpawnOptionsWithStdioTuple<StdioPipe, StdioPipe, StdioPipe> {
  return { stdio: ['pipe', 'pipe', 'pipe'], detached: false, shell: false, windowsHide: true, env, cwd: '/' };
}

export function spawnHelper(
  spawnFn: SpawnFn,
  executable: string,
  args: readonly string[],
  env: Record<string, string>,
): ChildProcessWithoutNullStreams {
  return spawnFn(executable, args, spawnOptions(env));
}

export const defaultSpawn: SpawnFn = (command, args, options) => nodeSpawn(command, args, options);
