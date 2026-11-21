// The owner's half of the helper's Unix events endpoint (issue #948;
// linux-desktop.md §5.2, L-F2's owner rules; the helper's half is #937, the
// attach line #893): a verified runtime directory, a Unix socket bound under
// it, and the attach that binds the one connection to the spawn.
//
// On macOS (issue #1743; macos-desktop.md §3.3) the owner verifies the
// system per-user temp directory and binds in its shared conch directory.
// Each listener owns its random socket path, so the owner leaves every other
// entry alone.
//
// On Windows (issue #1785; windows-desktop.md §3.3–§3.5) the endpoint is a
// named pipe the owner creates under \\.\pipe\, listenPipe below: no
// directory, no sweep, a random 128-bit name, and the same attach with the
// per-connection first-line rules of §3.4.
//
// Input: the platform's runtime directory, the profile id, a nonce, and
// whatever arrives on the socket. Output: a refusal with its reason, or the
// attached connection (and any bytes that came after the attach line).
// Owns: the runtime-directory checks, the 0o700 directories, Linux's bounded
// stale sweep of conch/<profile-id>, the sun_path budget, binding a random
// name directly (a new name on EADDRINUSE or EACCES), the first-line deadline
// and cap, the nonce comparison with timingSafeEqual, and closing the
// listener on attach, on refusal and on timeout.
//
// What it keeps: it never writes on any connection; it uses lstat to reject
// links at the directory entries it verifies; it listens on an absolute path
// with no NUL; nothing it logs or returns holds the nonce. The attach is a
// spawn-to-pipe binding and defence in depth, not authentication:
// a process of the same user could unlink the socket and bind its own, which
// is out of scope by design (linux-desktop.md §5.2). Authority arrives only
// on the helper's standard input.
import type { MainLog } from '../diagnostics.ts';
import { randomBytes, timingSafeEqual } from 'node:crypto';
import { chmod, lstat, mkdir, readdir, unlink } from 'node:fs/promises';
import { createServer, type Server, type Socket } from 'node:net';
import { isAbsolute, join, resolve } from 'node:path';

import { VOICE_CONTROL_MAX_LINE_BYTES } from '../../../web/src/schema/generated.ts';
import { validProfileId } from '../vault/files.ts';
import { parseAttached } from './protocol.ts';
import { runDarwinTempUtility } from './spawn.ts';

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

// sun_path holds 108 bytes on Linux (unix(7)) and 104 on macOS (xnu's
// sys/un.h). Reserve one byte for the terminating NUL before libuv binds.
const SUN_PATH_MAX_BYTES = { linux: 107, darwin: 103 } as const;

// The sweep looks at no more entries than this: the directory is ours and
// holds a socket or two, so more is something else, and is left alone.
const SWEEP_MAX_ENTRIES = 64;

// How many fresh names a bind tries before it gives up.
const BIND_ATTEMPTS = 8;

const PRIVATE_DIR = 0o700;

export interface EndpointDeps {
  // The platform whose rules apply; process.platform by default.
  readonly platform?: NodeJS.Platform;
  // The uid the checks compare against (a test stands in for another user).
  readonly uid?: number;
  // Runs a macOS system utility and returns its stdout. Test seam for getconf and ls.
  readonly execFile?: typeof runDarwinTempUtility;
  // A fresh socket name, without its directory. Test seam for squats.
  readonly name?: () => string;
  // A fresh pipe id (the hex after the pipe prefix). Test seam for squats.
  readonly pipeId?: () => string;
  // Binds the server to path. Test seam for EACCES.
  readonly listen?: (server: Server, path: string) => Promise<void>;
  readonly log?: MainLog;
}

function ownUid(deps: EndpointDeps): number {
  if (deps.uid !== undefined) return deps.uid;
  if (typeof process.getuid !== 'function') throw new Error('voice: the Unix endpoint needs a POSIX uid');
  return process.getuid();
}

function codeOf(err: unknown): string {
  return typeof err === 'object' && err !== null && 'code' in err && typeof err.code === 'string' ? err.code : '';
}

async function lstatOrNull(path: string) {
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
  st: NonNullable<Awaited<ReturnType<typeof lstatOrNull>>>,
  uid: number,
): 'runtime-dir-symlink' | 'runtime-dir-not-directory' | 'runtime-dir-not-owned' | 'runtime-dir-not-private' | null {
  if (st.isSymbolicLink()) return 'runtime-dir-symlink';
  if (!st.isDirectory()) return 'runtime-dir-not-directory';
  if (st.uid !== uid) return 'runtime-dir-not-owned';
  if ((st.mode & 0o077) !== 0) return 'runtime-dir-not-private';
  return null;
}

async function darwinTempDir(deps: EndpointDeps): Promise<string> {
  try {
    return (await (deps.execFile ?? runDarwinTempUtility)('/usr/bin/getconf', ['DARWIN_USER_TEMP_DIR'])).trimEnd();
  } catch {
    throw new EndpointError('runtime-dir-unset');
  }
}

async function requireNoDarwinAcl(dir: string, deps: EndpointDeps, reason: EndpointRefusal): Promise<void> {
  try {
    const output = await (deps.execFile ?? runDarwinTempUtility)('/bin/ls', ['-lde', dir]);
    // An ACL can be marked '+' or listed below the mode line when '@' marks
    // extended attributes. Only a single directory line can pass.
    const [listing, ...extra] = output.replace(/\n$/, '').split('\n');
    if (extra.length === 0 && /^d[rwxstST-]{9}@?[ \t]/.test(listing ?? '')) return;
  } catch {
    // Failure to inspect the ACL is a refusal too.
  }
  throw new EndpointError(reason);
}

// runtimeDir verifies $XDG_RUNTIME_DIR on Linux and uses the system-provided
// DARWIN_USER_TEMP_DIR on macOS. A failed check stops startup.
export async function runtimeDir(env: NodeJS.ProcessEnv, deps: EndpointDeps = {}): Promise<string> {
  const mac = (deps.platform ?? process.platform) === 'darwin';
  const value = mac ? await darwinTempDir(deps) : env['XDG_RUNTIME_DIR'];
  if (value === undefined || value === '') throw new EndpointError('runtime-dir-unset');
  if (!isAbsolute(value) || value.includes('\0')) throw new EndpointError('runtime-dir-not-absolute');
  // macOS normally includes a trailing slash. Check the same path join will
  // use for the socket; lstat with a trailing slash can follow a final link.
  const dir = mac ? resolve(value) : value;
  const st = await lstatOrNull(dir);
  if (st === null) throw new EndpointError('runtime-dir-missing');
  const refusal = privateDirRefusal(st, ownUid(deps));
  if (refusal !== null) throw new EndpointError(refusal);
  if (mac) {
    if ((st.mode & 0o7777) !== PRIVATE_DIR) throw new EndpointError('runtime-dir-not-private');
    await requireNoDarwinAcl(dir, deps, 'runtime-dir-not-private');
  }
  return dir;
}

// ensurePrivateDir makes dir 0o700 if it is absent and checks it as the
// runtime directory is checked; a directory of ours that is private but not
// exactly 0o700 (the helper wants exactly that of the socket's parent) is set
// to it.
async function ensurePrivateDir(dir: string, uid: number, darwin?: EndpointDeps): Promise<void> {
  try {
    await mkdir(dir, { mode: PRIVATE_DIR });
  } catch (err) {
    if (codeOf(err) !== 'EEXIST') throw new EndpointError('profile-dir-unsafe');
  }
  const st = await lstatOrNull(dir);
  if (st === null || privateDirRefusal(st, uid) !== null) throw new EndpointError('profile-dir-unsafe');
  if (darwin !== undefined) await requireNoDarwinAcl(dir, darwin, 'profile-dir-unsafe');
  if ((st.mode & (darwin === undefined ? 0o777 : 0o7777)) !== PRIVATE_DIR) await chmod(dir, PRIVATE_DIR);
}

// profileDir is <runtime>/conch/<profile-id> on Linux and <runtime>/conch
// on macOS, where keeping the profile id could exceed the socket path budget.
// Each level is created 0o700 and checked.
export async function profileDir(runtime: string, profileId: string, deps: EndpointDeps = {}): Promise<string> {
  if (!validProfileId(profileId)) throw new EndpointError('profile-id-invalid');
  const uid = ownUid(deps);
  const conch = join(runtime, 'conch');
  if ((deps.platform ?? process.platform) === 'darwin') {
    await ensurePrivateDir(conch, uid, deps);
    return conch;
  }
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
export async function sweep(dir: string, log: MainLog = () => undefined): Promise<number> {
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
  if (removed > 0) log(`voice: removed ${String(removed)} stale socket(s) from the profile's runtime directory`, { ring: true });
  return removed;
}

function randomName(): string {
  return `e-${randomBytes(9).toString('base64url')}.sock`;
}

function socketPath(dir: string, name: string, maxBytes: number): string {
  const path = join(dir, name);
  if (!isAbsolute(path) || path.includes('\0') || !path.startsWith('/')) {
    // Never an abstract name: the path is a file in our directory.
    throw new EndpointError('socket-unbindable');
  }
  if (Buffer.byteLength(path, 'utf8') > maxBytes) throw new EndpointError('socket-path-too-long');
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

// PIPE_PREFIX is where every one of the owner's pipes is; the name is that
// and 128 random bits in hex. It is passed in argv and is no secret
// (windows-desktop.md §3.3): it carries no authority.
export const PIPE_PREFIX = '\\\\.\\pipe\\conch-voice-';
const PIPE_ID_BYTES = 16;
const PIPE_ID_RE = /^[0-9a-f]{32}$/;

// The most connections a pipe holds while waiting for the attach; past it the
// oldest one that has delivered no byte is closed (see listening). Bounded by
// the first-line deadline as well.
export const PIPE_MAX_PENDING = 256;

function randomPipeId(): string {
  return randomBytes(PIPE_ID_BYTES).toString('hex');
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

// listenUnix verifies the socket's directories at each start. Linux sweeps
// its per-profile directory. On macOS the directory is shared by all the
// running instances, so Node's listener close handles only its own socket.
// macOS supplies the temp path under system-controlled ancestors. Only this
// user can write the checked temp and conch directories, so another account
// cannot swap a directory or socket between our checks, bind and cleanup.
export async function listenUnix(env: NodeJS.ProcessEnv, profileId: string, deps: EndpointDeps = {}): Promise<Listening> {
  const runtime = await runtimeDir(env, deps);
  const dir = await profileDir(runtime, profileId, deps);
  if ((deps.platform ?? process.platform) !== 'darwin') await sweep(dir, deps.log);
  return listen(dir, deps);
}

// listen binds a fresh random name in dir, checking the sun_path budget
// before every bind; on EADDRINUSE (a squat) or EACCES it tries a new name.
export async function listen(dir: string, deps: EndpointDeps = {}): Promise<Listening> {
  const nextName = deps.name ?? randomName;
  const bind = deps.listen ?? defaultListen;
  const maxBytes = SUN_PATH_MAX_BYTES[(deps.platform ?? process.platform) === 'darwin' ? 'darwin' : 'linux'];
  for (let attempt = 0; attempt < BIND_ATTEMPTS; attempt++) {
    const path = socketPath(dir, nextName(), maxBytes);
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

// listenPipe creates the owner's named pipe: \\.\pipe\conch-voice-<128 random
// bits> through Node's net (libuv), with no readableAll or writableAll (the
// default security descriptor is the cross-user boundary, §3.5). A listen
// failure on the name (a squat: FILE_FLAG_FIRST_PIPE_INSTANCE makes it
// EACCES, a busy name EADDRINUSE) picks a new random name.
export async function listenPipe(deps: EndpointDeps = {}): Promise<Listening> {
  const nextId = deps.pipeId ?? randomPipeId;
  const bind = deps.listen ?? defaultListen;
  for (let attempt = 0; attempt < BIND_ATTEMPTS; attempt++) {
    const id = nextId();
    if (!PIPE_ID_RE.test(id)) throw new EndpointError('socket-unbindable');
    const name = PIPE_PREFIX + id;
    const server = createServer({ pauseOnConnect: true, allowHalfOpen: false });
    try {
      await bind(server, name);
    } catch (err) {
      server.close();
      const code = codeOf(err);
      if (code === 'EADDRINUSE' || code === 'EACCES') {
        deps.log?.(`voice: the pipe name was taken (${code}); trying another`);
        continue;
      }
      throw new EndpointError('socket-unbindable');
    }
    return listening(server, name, true);
  }
  throw new EndpointError('socket-unbindable');
}

// listening is the attach of one bound endpoint. On a Unix socket in the
// private directory, where only the helper can connect, the first connection
// is the candidate and any failure, or a second connection, fails the attach.
// On a pipe (pipe = true), which other users can open read-only and same-user
// code can flood (§3.5), every connection has its own first line and its own
// failure: a bad one is closed and the wait goes on to the deadline, and a
// connection past PIPE_MAX_PENDING closes the oldest waiting one that has
// delivered no byte (§3.4 rule 2). What holds, and what does not: a cross-user
// client can never deliver a byte (§3.5), so the helper, once it has written
// any, is never the one closed; but a flood of PIPE_MAX_PENDING or more silent
// opens between the helper's open and its first byte can still close it. That
// fails closed and is the attach flood §3.5 names as a residual.
function listening(server: Server, path: string, pipe = false): Listening {
  // Every connection that arrived, in order. On a socket the first is the
  // candidate and a second before the attach is settled fails it; on a pipe
  // each is read for itself.
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
  // drop closes one pipe connection that failed its first line; the attach
  // goes on waiting for another.
  const drop = (s: Socket): void => {
    const i = connections.indexOf(s);
    if (i >= 0) connections.splice(i, 1);
    s.removeAllListeners('data');
    s.destroy();
  };
  // spoke is the connections that have delivered at least one byte.
  const spoke = new WeakSet<Socket>();
  server.on('connection', (s: Socket) => {
    s.on('error', () => undefined);
    if (closed) {
      s.destroy();
      return;
    }
    // At the bound the oldest connection that has delivered no byte goes
    // (the oldest of all if every one has), not the newcomer.
    if (pipe && connections.length >= PIPE_MAX_PENDING) {
      const victim = connections.find((c) => !spoke.has(c)) ?? connections[0];
      if (victim !== undefined) drop(victim);
    }
    connections.push(s);
    if (!pipe && connections.length > 1) {
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
          want.fill(0);
          onFirst = null;
          fail = null;
          clearTimeout(timer);
          close();
          reject(new EndpointError(reason));
        };
        const succeed = (s: Socket, rest: Buffer): void => {
          if (settled) return;
          settled = true;
          want.fill(0);
          onFirst = null;
          fail = null;
          clearTimeout(timer);
          s.pause();
          // The attached connection is the caller's now; every other one, and
          // the listener, goes. After attach no further connection is
          // accepted.
          const at = connections.indexOf(s);
          if (at >= 0) connections.splice(at, 1);
          close();
          resolve({ socket: s, rest });
        };
        // refused ends the attempt on a socket and only this connection on a
        // pipe.
        const refused = (s: Socket, reason: AttachFailure): void => {
          if (pipe) drop(s);
          else fail?.(reason);
        };
        const read = (s: Socket): void => {
          const chunks: Buffer[] = [];
          let size = 0;
          const onEnd = (): void => {
            refused(s, 'attach-ended');
          };
          const onData = (chunk: Buffer): void => {
            spoke.add(s);
            const lf = chunk.indexOf(0x0a);
            const take = lf < 0 ? chunk : chunk.subarray(0, lf);
            size += take.length;
            if (size > VOICE_CONTROL_MAX_LINE_BYTES) {
              s.off('data', onData);
              refused(s, 'attach-too-long');
              return;
            }
            chunks.push(Buffer.from(take));
            if (lf < 0) return;
            s.off('data', onData);
            s.off('end', onEnd);
            s.off('close', onEnd);
            const line = Buffer.concat(chunks);
            const parsed = parseAttached(line);
            // The line and the copies of it are wiped once compared: the
            // nonce is spent, or this was not it.
            const wipe = (...bufs: Buffer[]): void => {
              for (const b of [line, ...chunks, ...bufs]) b.fill(0);
            };
            if (!parsed.ok) {
              wipe();
              refused(s, 'attach-refused');
              return;
            }
            const got = Buffer.from(parsed.attached.nonce, 'utf8');
            // Both are 43 bytes once parsed; timingSafeEqual throws on a
            // length mismatch, so the length is compared first.
            const same = got.length === want.length && timingSafeEqual(got, want);
            wipe(got);
            if (!same) {
              refused(s, 'attach-refused');
              return;
            }
            succeed(s, Buffer.from(chunk.subarray(lf + 1)));
          };
          s.on('data', onData);
          s.once('end', onEnd);
          s.once('close', onEnd);
          s.resume();
        };
        if (pipe) {
          // Every connection that is already here, and every one to come.
          onFirst = read;
          for (const c of [...connections]) read(c);
          return;
        }
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
