// The owner's half of the helper's Linux events endpoint (issue #948;
// linux-desktop.md §5.2, L-F2's owner rules; the helper's half is #937, the
// attach line #893): a verified runtime directory, a Unix socket bound under
// it, and the attach that binds the one connection to the spawn.
//
// Input: the environment's XDG_RUNTIME_DIR, the profile id, a nonce, and
// whatever arrives on the socket. Output: a refusal with its reason, or the
// attached connection (and any bytes that came after the attach line).
// Owns: the runtime-directory checks, conch/<profile-id> (created 0o700 and
// checked the same way), the bounded stale sweep of that one directory, the
// sun_path budget, binding a random name directly (a new name on EADDRINUSE
// or EACCES), the first-line deadline and cap, the nonce comparison with
// timingSafeEqual, and closing the listener on attach, on refusal and on
// timeout.
//
// What it keeps: it never writes on any connection; it never follows a link
// (every check is lstat); it never listens on an abstract name (the path is
// absolute and holds no NUL); nothing it logs or returns holds the nonce. The
// attach is a spawn-to-pipe binding and defence in depth, not authentication:
// a process of the same user could unlink the socket and bind its own, which
// is out of scope by design (linux-desktop.md §5.2). Authority arrives only
// on the helper's standard input.
import { randomBytes, timingSafeEqual } from 'node:crypto';
import { chmod, lstat, mkdir, readdir, unlink } from 'node:fs/promises';
import { createServer, type Server, type Socket } from 'node:net';
import { isAbsolute, join } from 'node:path';

import { VOICE_CONTROL_MAX_LINE_BYTES } from '../../../web/src/schema/generated.ts';
import { validProfileId } from '../vault/files.ts';
import { parseAttached } from './protocol.ts';

// Why voice cannot start here. Each is a fixed word for the UI and the log,
// never a path or a value.
export type EndpointRefusal =
  | 'runtime-dir-unset'
  | 'runtime-dir-not-absolute'
  | 'runtime-dir-missing'
  | 'runtime-dir-symlink'
  | 'runtime-dir-not-directory'
  | 'runtime-dir-not-owned'
  | 'runtime-dir-not-private'
  | 'profile-id-invalid'
  | 'profile-dir-unsafe'
  | 'socket-path-too-long'
  | 'socket-unbindable';

// Why an attach failed: the start fails, the listener is closed.
export type AttachFailure = 'attach-timeout' | 'attach-refused' | 'attach-too-long' | 'attach-second-connection' | 'attach-ended';

export class EndpointError extends Error {
  constructor(readonly reason: EndpointRefusal | AttachFailure) {
    super(`voice: ${reason}`);
  }
}

// sun_path is 108 bytes, its last a NUL (unix(7)); libuv refuses a name of
// 108 bytes or more, and so does Rust's connect.
export const SUN_PATH_MAX_BYTES = 107;

// The sweep looks at no more entries than this: the directory is ours and
// holds a socket or two, so more is something else, and is left alone.
const SWEEP_MAX_ENTRIES = 64;

// How many fresh names a bind tries before it gives up.
const BIND_ATTEMPTS = 8;

const PRIVATE_DIR = 0o700;

export interface EndpointDeps {
  // The uid the checks compare against (a test stands in for another user).
  readonly uid?: number;
  // A fresh socket name, without its directory. Test seam for squats.
  readonly name?: () => string;
  // Binds the server to path. Test seam for EACCES.
  readonly listen?: (server: Server, path: string) => Promise<void>;
  readonly log?: (line: string) => void;
}

function ownUid(deps: EndpointDeps): number {
  if (deps.uid !== undefined) return deps.uid;
  if (typeof process.getuid !== 'function') throw new Error('voice: the Linux endpoint needs a POSIX uid');
  return process.getuid();
}

function codeOf(err: unknown): string {
  return typeof err === 'object' && err !== null && 'code' in err && typeof err.code === 'string' ? err.code : '';
}

async function lstatOrNull(path: string): Promise<Awaited<ReturnType<typeof lstat>> | null> {
  try {
    return await lstat(path);
  } catch (err) {
    if (codeOf(err) === 'ENOENT') return null;
    throw err;
  }
}

// privateDirRefusal is the check every directory on the way to the socket
// passes: a directory (not a link to one), owned by this user, closed to
// group and others.
function privateDirRefusal(
  st: Awaited<ReturnType<typeof lstat>>,
  uid: number,
): 'runtime-dir-symlink' | 'runtime-dir-not-directory' | 'runtime-dir-not-owned' | 'runtime-dir-not-private' | null {
  if (st.isSymbolicLink()) return 'runtime-dir-symlink';
  if (!st.isDirectory()) return 'runtime-dir-not-directory';
  if (st.uid !== uid) return 'runtime-dir-not-owned';
  if ((Number(st.mode) & 0o077) !== 0) return 'runtime-dir-not-private';
  return null;
}

// runtimeDir verifies $XDG_RUNTIME_DIR, never trusting the variable: unset,
// relative, missing, a link, not a directory, another user's, or open to
// group or others is a refusal, and nothing is created. There is no fallback
// to /tmp.
export async function runtimeDir(env: NodeJS.ProcessEnv, deps: EndpointDeps = {}): Promise<string> {
  const dir = env['XDG_RUNTIME_DIR'];
  if (dir === undefined || dir === '') throw new EndpointError('runtime-dir-unset');
  if (!isAbsolute(dir) || dir.includes('\0')) throw new EndpointError('runtime-dir-not-absolute');
  const st = await lstatOrNull(dir);
  if (st === null) throw new EndpointError('runtime-dir-missing');
  const refusal = privateDirRefusal(st, ownUid(deps));
  if (refusal !== null) throw new EndpointError(refusal);
  return dir;
}

// ensurePrivateDir makes dir 0o700 if it is absent and checks it as the
// runtime directory is checked; a directory of ours that is private but not
// exactly 0o700 (the helper wants exactly that of the socket's parent) is set
// to it.
async function ensurePrivateDir(dir: string, uid: number): Promise<void> {
  try {
    await mkdir(dir, { mode: PRIVATE_DIR });
  } catch (err) {
    if (codeOf(err) !== 'EEXIST') throw new EndpointError('profile-dir-unsafe');
  }
  const st = await lstatOrNull(dir);
  if (st === null || privateDirRefusal(st, uid) !== null) throw new EndpointError('profile-dir-unsafe');
  if ((Number(st.mode) & 0o777) !== PRIVATE_DIR) await chmod(dir, PRIVATE_DIR);
}

// profileDir is <runtime>/conch/<profile-id>, each level created 0o700 and
// checked.
export async function profileDir(runtime: string, profileId: string, deps: EndpointDeps = {}): Promise<string> {
  if (!validProfileId(profileId)) throw new EndpointError('profile-id-invalid');
  const uid = ownUid(deps);
  const conch = join(runtime, 'conch');
  await ensurePrivateDir(conch, uid);
  const dir = join(conch, profileId);
  await ensurePrivateDir(dir, uid);
  return dir;
}

// sweep removes stale sockets from the profile's own directory: it lstats
// each entry (never following a link), unlinks only sockets, never descends
// and never touches another directory. It looks at a bounded number of
// entries. A socket of a live helper of this profile cannot be there: one
// helper per profile, and the listener is closed (and its path unlinked) on
// attach.
export async function sweep(dir: string, log: (line: string) => void = () => undefined): Promise<number> {
  const names = await readdir(dir);
  let removed = 0;
  for (const name of names.slice(0, SWEEP_MAX_ENTRIES)) {
    const path = join(dir, name);
    const st = await lstatOrNull(path);
    if (st?.isSocket() === true) {
      try {
        await unlink(path);
        removed++;
      } catch (err) {
        if (codeOf(err) !== 'ENOENT') throw err;
      }
    }
  }
  if (removed > 0) log(`voice: removed ${String(removed)} stale socket(s) from the profile's runtime directory`);
  return removed;
}

function randomName(): string {
  return `e-${randomBytes(9).toString('base64url')}.sock`;
}

function socketPath(dir: string, name: string): string {
  const path = join(dir, name);
  if (!isAbsolute(path) || path.includes('\0') || !path.startsWith('/')) {
    // Never an abstract name: the path is a file in our directory.
    throw new EndpointError('socket-unbindable');
  }
  if (Buffer.byteLength(path, 'utf8') > SUN_PATH_MAX_BYTES) throw new EndpointError('socket-path-too-long');
  return path;
}

function defaultListen(server: Server, path: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const onError = (err: Error): void => {
      reject(err);
    };
    server.once('error', onError);
    // readableAll and writableAll stay false: the 0o700 directory is the
    // boundary, not a wider socket mode.
    server.listen({ path, readableAll: false, writableAll: false }, () => {
      server.off('error', onError);
      resolve();
    });
  });
}

// Listening is a bound endpoint waiting for its one connection.
export interface Listening {
  readonly path: string;
  // attach waits, until deadline (ms since the epoch), for the helper's
  // connection and its attach line, and settles once. On any failure the
  // listener is closed and every connection destroyed.
  attach(nonce: string, deadline: number): Promise<Attached>;
  // close closes the listener (Node unlinks the path) and any connection
  // still waiting. Idempotent.
  close(): void;
}

export interface Attached {
  readonly socket: Socket;
  // Bytes that came after the attach line's line feed: the hello, maybe more.
  readonly rest: Buffer;
}

// listen binds a fresh random name in dir, checking the sun_path budget
// before every bind; on EADDRINUSE (a squat) or EACCES it tries a new name.
export async function listen(dir: string, deps: EndpointDeps = {}): Promise<Listening> {
  const nextName = deps.name ?? randomName;
  const bind = deps.listen ?? defaultListen;
  for (let attempt = 0; attempt < BIND_ATTEMPTS; attempt++) {
    const path = socketPath(dir, nextName());
    const server = createServer({ pauseOnConnect: true, allowHalfOpen: false });
    try {
      await bind(server, path);
    } catch (err) {
      server.close();
      const code = codeOf(err);
      if (code === 'EADDRINUSE' || code === 'EACCES') {
        deps.log?.(`voice: the socket name was taken (${code}); trying another`);
        continue;
      }
      throw new EndpointError('socket-unbindable');
    }
    return listening(server, path);
  }
  throw new EndpointError('socket-unbindable');
}

function listening(server: Server, path: string): Listening {
  // Every connection that arrived, in order. The first is the candidate; a
  // second before the attach is settled fails it.
  const connections: Socket[] = [];
  let closed = false;
  let attachCalled = false;
  // fail ends a pending attach; set while one is pending.
  let fail: ((reason: AttachFailure) => void) | null = null;
  let onFirst: ((s: Socket) => void) | null = null;

  const close = (): void => {
    if (closed) return;
    closed = true;
    // Node unlinks the socket's path when the server closes.
    server.close();
    for (const s of connections) s.destroy();
    fail?.('attach-ended');
  };

  // Errors end the attempt through attach's own handlers; these keep a stray
  // one from being thrown.
  server.on('error', () => undefined);
  server.on('connection', (s: Socket) => {
    s.on('error', () => undefined);
    if (closed) {
      s.destroy();
      return;
    }
    connections.push(s);
    if (connections.length > 1) {
      fail?.('attach-second-connection');
      return;
    }
    onFirst?.(s);
  });

  return {
    path,
    close,
    attach(nonce: string, deadline: number): Promise<Attached> {
      if (attachCalled || closed) return Promise.reject(new EndpointError('attach-ended'));
      attachCalled = true;
      const want = Buffer.from(nonce, 'utf8');
      return new Promise<Attached>((resolve, reject) => {
        let settled = false;
        const timer = setTimeout(() => {
          fail?.('attach-timeout');
        }, Math.max(0, deadline - Date.now()));
        fail = (reason: AttachFailure): void => {
          if (settled) return;
          settled = true;
          fail = null;
          clearTimeout(timer);
          close();
          reject(new EndpointError(reason));
        };
        const succeed = (s: Socket, rest: Buffer): void => {
          if (settled) return;
          settled = true;
          fail = null;
          clearTimeout(timer);
          s.pause();
          // The attached connection is the caller's now; every other one, and
          // the listener, goes. After attach no further connection is
          // accepted.
          connections.splice(connections.indexOf(s), 1);
          close();
          resolve({ socket: s, rest });
        };
        const read = (s: Socket): void => {
          const chunks: Buffer[] = [];
          let size = 0;
          const onEnd = (): void => {
            fail?.('attach-ended');
          };
          const onData = (chunk: Buffer): void => {
            const lf = chunk.indexOf(0x0a);
            const take = lf < 0 ? chunk : chunk.subarray(0, lf);
            size += take.length;
            if (size > VOICE_CONTROL_MAX_LINE_BYTES) {
              s.off('data', onData);
              fail?.('attach-too-long');
              return;
            }
            chunks.push(Buffer.from(take));
            if (lf < 0) return;
            s.off('data', onData);
            s.off('end', onEnd);
            s.off('close', onEnd);
            const parsed = parseAttached(Buffer.concat(chunks));
            if (!parsed.ok) {
              fail?.('attach-refused');
              return;
            }
            const got = Buffer.from(parsed.attached.nonce, 'utf8');
            // Both are 43 bytes once parsed; timingSafeEqual throws on a
            // length mismatch, so the length is compared first.
            if (got.length !== want.length || !timingSafeEqual(got, want)) {
              fail?.('attach-refused');
              return;
            }
            succeed(s, Buffer.from(chunk.subarray(lf + 1)));
          };
          s.on('data', onData);
          s.once('end', onEnd);
          s.once('close', onEnd);
          s.resume();
        };
        if (connections.length > 1) {
          fail('attach-second-connection');
          return;
        }
        const first = connections[0];
        if (first !== undefined) read(first);
        else onFirst = read;
      });
    },
  };
}
