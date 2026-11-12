// The Electron owner of conch-voice on Windows (issue #1785;
// windows-desktop.md §3.3–§3.7): the named-pipe endpoint, the six owner rules
// of §3.4, the helper's path, the child's environment, owner loss and stop,
// and the configuration directory.
//
// Nothing here needs Windows. The pipe is a Node server: on Windows it is the
// real `\\.\pipe\conch-voice-<id>`; elsewhere a test seam binds the same name
// to a Unix socket in a private directory, and the supervisor runs with
// platform 'win32' against a fake child process. The tests that only a real
// pipe can answer (a real squat, the real name) run on the desktop-windows
// job. What only hardware answers (libuv's listener against tokio's client,
// ERROR_PIPE_BUSY, the DACL, cross-user access) is E2 (#903).
import { EventEmitter } from 'node:events';
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { connect, Server, type Socket } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { PassThrough } from 'node:stream';
import type { ChildProcessWithoutNullStreams } from 'node:child_process';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { EndpointError, PIPE_MAX_PENDING, PIPE_PREFIX, listenPipe, type EndpointDeps, type Listening } from '../../main/voice/endpoint.ts';
import {
  HelperError,
  PASSED_THROUGH_WINDOWS,
  WINDOWS_HELPER_NAME,
  findHelper,
  helperArgs,
  helperEnv,
  oneShotEnv,
  oneShotSpawnOptions,
  spawnOptions,
} from '../../main/voice/spawn.ts';
import { Supervisor, type StartRequest, type VoiceState } from '../../main/voice/supervisor.ts';
import { profileConfigDir } from '../../main/vault/files.ts';
import { FAKE_TOKEN, readFixture, transcript, until } from './voice-support.ts';

const ON_WINDOWS = process.platform === 'win32';

// A nonce of the attach shape: 43 characters of base64url, the last one of
// the 4 that carry no stray bits.
const NONCE = 'WIN_nonce_do_not_print_0123456789abcdefghiA';
const OTHER_NONCE = 'WIN_other_do_not_print_0123456789abcdefghiA';

let dir: string;
beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), 'conch-win-'));
});
afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

function attachLine(nonce: string): string {
  const base = readFixture<Record<string, unknown>>('voice-control-pipe/attached.json');
  return `${JSON.stringify({ ...base, nonce })}\n`;
}

// ------------------------------------------------------------- the harness

interface Harness {
  readonly deps: EndpointDeps;
  // The names listened on, in order.
  readonly names: string[];
  connect(name: string): Socket;
}

// pipes gives the endpoint a way to listen and a way to connect. On Windows it
// is the real pipe; elsewhere the pipe's name is bound to a Unix socket named
// by the id at its end, so that the same name is the same socket.
function pipes(extra: EndpointDeps = {}): Harness {
  const names: string[] = [];
  const pathOf = (name: string): string => join(dir, `${name.slice(-32)}.sock`);
  if (ON_WINDOWS) {
    return { deps: { ...extra, listen: (server, name) => bind(server, name, names) }, names, connect: (name) => connect(name) };
  }
  return { deps: { ...extra, listen: (server, name) => bind(server, pathOf(name), names, name) }, names, connect: (name) => connect(pathOf(name)) };
}

function bind(server: Server, path: string, names: string[], as = path): Promise<void> {
  return new Promise((resolve, reject) => {
    const onError = (err: Error): void => {
      reject(err);
    };
    server.once('error', onError);
    server.listen({ path, readableAll: false, writableAll: false }, () => {
      server.off('error', onError);
      names.push(as);
      resolve();
    });
  });
}

// A client: the helper's side of one connection, recording everything the
// owner sends it and when it is closed.
class Client {
  readonly received: Buffer[] = [];
  closed = false;
  readonly socket: Socket;
  readonly opened: Promise<void>;

  constructor(h: Harness, name: string) {
    this.socket = h.connect(name);
    this.socket.on('data', (c: Buffer) => this.received.push(c));
    this.socket.on('close', () => {
      this.closed = true;
    });
    this.socket.on('error', () => undefined);
    this.opened = new Promise((resolve) => {
      this.socket.once('connect', () => {
        resolve();
      });
    });
  }

  async send(text: string): Promise<void> {
    await this.opened;
    this.socket.write(text);
  }

  get bytesReceived(): number {
    return this.received.reduce((n, b) => n + b.length, 0);
  }
}

async function connectFails(h: Harness, name: string): Promise<boolean> {
  return new Promise((resolve) => {
    const s = h.connect(name);
    s.once('connect', () => {
      s.destroy();
      resolve(false);
    });
    s.once('error', () => {
      resolve(true);
    });
  });
}

async function outcome(p: Promise<unknown>): Promise<string> {
  try {
    await p;
    return 'attached';
  } catch (err) {
    return err instanceof EndpointError ? err.reason : `other: ${String(err)}`;
  }
}

const SOON = 400;

// ------------------------------------------------- §3.4 rule 1: the listener

describe('rule 1: the listener', () => {
  it('is created without readableAll or writableAll, on a name under the pipe prefix', async () => {
    const calls: unknown[] = [];
    const spy = vi.spyOn(Server.prototype, 'listen').mockImplementation(function (this: Server, ...args: unknown[]) {
      calls.push(args[0]);
      const cb = args.find((a) => typeof a === 'function');
      if (typeof cb === 'function') queueMicrotask(() => { (cb as () => void)(); });
      return this;
    });
    try {
      const l = await listenPipe();
      l.close();
    } finally {
      spy.mockRestore();
    }
    expect(calls).toHaveLength(1);
    const opts = calls[0] as { path: string; readableAll?: boolean; writableAll?: boolean };
    expect(opts.readableAll).toBe(false);
    expect(opts.writableAll).toBe(false);
    expect(opts.path).toMatch(/^\\\\\.\\pipe\\conch-voice-[0-9a-f]{32}$/);
    expect(opts.path.startsWith(PIPE_PREFIX)).toBe(true);
  });

  it('takes a different 128-bit name every time', async () => {
    const h = pipes();
    const a = await listenPipe(h.deps);
    const b = await listenPipe(h.deps);
    try {
      expect(a.path).not.toBe(b.path);
      expect(h.names).toEqual([a.path, b.path]);
      for (const n of h.names) expect(n.slice(PIPE_PREFIX.length)).toMatch(/^[0-9a-f]{32}$/);
    } finally {
      a.close();
      b.close();
    }
  });

  const ID1 = 'a'.repeat(32);
  const ID2 = 'b'.repeat(32);
  it.each([
    ['EADDRINUSE', 'EADDRINUSE'],
    ['EACCES (FILE_FLAG_FIRST_PIPE_INSTANCE: ERROR_ACCESS_DENIED)', 'EACCES'],
  ])('a listen failure on the name (%s) picks a new random name', async (_what, code) => {
    const ids = [ID1, ID2];
    const tried: string[] = [];
    const h = pipes({
      pipeId: () => ids.shift() ?? ID2,
    });
    let first = true;
    const l = await listenPipe({
      ...h.deps,
      listen: (server, name) => {
        tried.push(name);
        if (first) {
          first = false;
          return Promise.reject(Object.assign(new Error('squat'), { code }));
        }
        return h.deps.listen?.(server, name) ?? Promise.resolve();
      },
    });
    try {
      expect(tried).toEqual([PIPE_PREFIX + ID1, PIPE_PREFIX + ID2]);
      expect(l.path).toBe(PIPE_PREFIX + ID2);
    } finally {
      l.close();
    }
  });

  it('gives up after a bounded number of squatted names, and on any other failure at once', async () => {
    let n = 0;
    const always = Object.assign(new Error('squat'), { code: 'EACCES' });
    const err1 = await outcome(
      listenPipe({
        pipeId: () => ID1,
        listen: () => {
          n++;
          return Promise.reject(always);
        },
      }),
    );
    expect(err1).toBe('socket-unbindable');
    expect(n).toBe(8);
    n = 0;
    const err2 = await outcome(
      listenPipe({
        listen: () => {
          n++;
          return Promise.reject(Object.assign(new Error('x'), { code: 'EMFILE' }));
        },
      }),
    );
    expect(err2).toBe('socket-unbindable');
    expect(n).toBe(1);
  });

  it('refuses a pipe id that is not 128 bits of hex', async () => {
    for (const id of ['', 'zz', 'A'.repeat(32), 'a'.repeat(31), `${'a'.repeat(31)}\\x`]) {
      expect(await outcome(listenPipe({ pipeId: () => id, listen: () => Promise.resolve() })), id).toBe('socket-unbindable');
    }
  });

  it.runIf(ON_WINDOWS)('a real squat on a real pipe name makes listen fail and a new name is picked', async () => {
    const taken = new Server();
    const first = await listenPipe({ pipeId: () => ID1 });
    taken.close();
    const ids = [ID1, ID2];
    const second = await listenPipe({ pipeId: () => ids.shift() ?? ID2 });
    try {
      expect(first.path).toBe(PIPE_PREFIX + ID1);
      expect(second.path).toBe(PIPE_PREFIX + ID2);
    } finally {
      first.close();
      second.close();
    }
  });
});

// ------------------------------------------------- §3.4 rules 2, 3 and 4

async function listening(h: Harness): Promise<Listening> {
  return listenPipe(h.deps);
}

describe('rule 2: the first line', () => {
  const GOOD = (): string => attachLine(NONCE);
  const long = 'x'.repeat(5000);
  // Each bad first line closes its own connection; the attach is still
  // waiting, and a good connection after it attaches.
  it.each([
    ['a nonce that is not the owner’s', attachLine(OTHER_NONCE)],
    ['an attach with an unknown kind', GOOD().replace('"attached"', '"hello"')],
    ['not JSON', 'not json at all\n'],
    ['a different schema', GOOD().replace('conch.voice_control_pipe.v1', 'conch.voice_control.v1')],
    ['a nonce that is too short', attachLine('short')],
    ['a line past the 4096-byte cap, with no line feed', long],
    ['a line past the cap, with a line feed', `${long}\n`],
  ])('%s: the connection is closed and the attach goes on', async (_what, line) => {
    const h = pipes();
    const l = await listening(h);
    const deadline = Date.now() + 5000;
    const attached = l.attach(NONCE, deadline);
    const bad = new Client(h, l.path);
    await bad.send(line);
    await until(() => bad.closed, 3000, 'the bad connection to be closed');
    const good = new Client(h, l.path);
    await good.send(GOOD());
    const got = await attached;
    expect(got.rest.length).toBe(0);
    got.socket.destroy();
  });

  it('a connection that sends nothing does not hold the attach: another connection attaches', async () => {
    const h = pipes();
    const l = await listening(h);
    const attached = l.attach(NONCE, Date.now() + 5000);
    const silent = new Client(h, l.path);
    await silent.opened;
    const good = new Client(h, l.path);
    await good.send(attachLine(NONCE));
    const got = await attached;
    expect(got.socket).toBeDefined();
    expect(silent.closed).toBe(false);
    got.socket.destroy();
    // …but the listener is closed, and with it the waiting connection.
    await until(() => silent.closed, 3000, 'the waiting connection to be closed');
  });

  it('bytes after the attach line (the hello) are returned, not lost', async () => {
    const h = pipes();
    const l = await listening(h);
    const attached = l.attach(NONCE, Date.now() + 5000);
    const c = new Client(h, l.path);
    await c.send(`${attachLine(NONCE)}{"hello":1}\n`);
    const got = await attached;
    expect(got.rest.toString('utf8')).toBe('{"hello":1}\n');
    got.socket.destroy();
  });

  it('the deadline is the one the caller gives: no first line by then is attach-timeout, and the listener is closed', async () => {
    const h = pipes();
    const l = await listening(h);
    const attached = l.attach(NONCE, Date.now() + 150);
    const silent = new Client(h, l.path);
    await silent.opened;
    expect(await outcome(attached)).toBe('attach-timeout');
    await until(() => silent.closed, 3000, 'the waiting connection to be closed');
    expect(await connectFails(h, l.path)).toBe(true);
  });

  it('a deadline already past fails at once', async () => {
    const h = pipes();
    const l = await listening(h);
    expect(await outcome(l.attach(NONCE, Date.now() - 1))).toBe('attach-timeout');
  });

  it('more than the bound of waiting connections: the excess is closed at once', async () => {
    const h = pipes();
    const l = await listening(h);
    const attached = l.attach(NONCE, Date.now() + 5000);
    const clients: Client[] = [];
    for (let i = 0; i < PIPE_MAX_PENDING + 4; i++) clients.push(new Client(h, l.path));
    await until(() => clients.filter((c) => c.closed).length >= 4, 3000, 'the excess connections to be closed');
    expect(clients.filter((c) => c.closed).length).toBe(4);
    // The waiting ones are served: a good first line from one of them wins.
    const winner = clients.find((c) => !c.closed);
    await winner?.send(attachLine(NONCE));
    (await attached).socket.destroy();
  });
});

describe('rule 3: the owner never writes', () => {
  it.each([
    ['a good attach', attachLine(NONCE), 'attached'],
    ['a wrong nonce, then the deadline', attachLine(OTHER_NONCE), 'attach-timeout'],
    ['an overlong line, then the deadline', 'y'.repeat(5000), 'attach-timeout'],
    ['no line at all', '', 'attach-timeout'],
  ])('%s: not a byte reaches the connection', async (_what, line, want) => {
    const h = pipes();
    const l = await listening(h);
    const attached = l.attach(NONCE, Date.now() + SOON);
    const c = new Client(h, l.path);
    if (line !== '') await c.send(line);
    expect(await outcome(attached)).toBe(want);
    await new Promise((r) => setTimeout(r, 50));
    expect(c.bytesReceived).toBe(0);
    c.socket.destroy();
  });
});

describe('rule 4: the listener is closed on attach or at the deadline', () => {
  it('after the attach no further connection is accepted, and the others are closed', async () => {
    const h = pipes();
    const l = await listening(h);
    const attached = l.attach(NONCE, Date.now() + 5000);
    const waiting = new Client(h, l.path);
    await waiting.opened;
    const good = new Client(h, l.path);
    await good.send(attachLine(NONCE));
    const got = await attached;
    await until(() => waiting.closed, 3000, 'the other connection to be closed');
    expect(await connectFails(h, l.path)).toBe(true);
    got.socket.destroy();
  });

  it('a close() closes the listener and the waiting connections, and is idempotent', async () => {
    const h = pipes();
    const l = await listening(h);
    const c = new Client(h, l.path);
    await c.opened;
    l.close();
    l.close();
    await until(() => c.closed, 3000, 'the connection to be closed');
    expect(await connectFails(h, l.path)).toBe(true);
    expect(await outcome(l.attach(NONCE, Date.now() + 100))).toBe('attach-ended');
  });
});

// ----------------------------------------------------- the helper and its env

describe('the helper on Windows', () => {
  async function refusal(p: Promise<unknown>): Promise<string> {
    try {
      await p;
      return 'found';
    } catch (err) {
      return err instanceof HelperError ? err.reason : `other: ${String(err)}`;
    }
  }
  const win = { platform: 'win32' } as const;

  it('is conch-voice.exe inside the resources directory', async () => {
    const res = join(dir, 'resources');
    mkdirSync(res);
    writeFileSync(join(res, WINDOWS_HELPER_NAME), 'MZ');
    expect(await findHelper(res, win)).toBe(join(res, WINDOWS_HELPER_NAME));
  });

  it('a missing file is not-installed, and a Unix-named one is no substitute', async () => {
    const res = join(dir, 'resources');
    mkdirSync(res);
    expect(await refusal(findHelper(res, win))).toBe('not-installed');
    writeFileSync(join(res, 'conch-voice'), 'x');
    expect(await refusal(findHelper(res, win))).toBe('not-installed');
  });

  it('a directory, and a relative resources path, are untrusted', async () => {
    const res = join(dir, 'resources');
    mkdirSync(join(res, WINDOWS_HELPER_NAME), { recursive: true });
    expect(await refusal(findHelper(res, win))).toBe('untrusted');
    expect(await refusal(findHelper('resources', win))).toBe('untrusted');
  });

  it('a link is untrusted', async (ctx) => {
    const res = join(dir, 'resources');
    mkdirSync(res);
    const real = join(dir, 'elsewhere.exe');
    writeFileSync(real, 'MZ');
    try {
      symlinkSync(real, join(res, WINDOWS_HELPER_NAME));
    } catch {
      // Windows needs a privilege or developer mode to make one.
      ctx.skip();
      return;
    }
    expect(await refusal(findHelper(res, win))).toBe('untrusted');
  });

  it('has no mode or owner rule: Windows has neither', async () => {
    const res = join(dir, 'resources');
    mkdirSync(res);
    writeFileSync(join(res, WINDOWS_HELPER_NAME), 'MZ', { mode: 0o666 });
    expect(await findHelper(res, { platform: 'win32', uid: 12345 })).toBe(join(res, WINDOWS_HELPER_NAME));
  });
});

describe('the helper’s environment on Windows', () => {
  const configDir = 'C:\\Users\\Someone\\AppData\\Roaming\\Conch\\profiles\\p1';
  const names = (env: Record<string, string>): string[] => Object.keys(env).sort();

  it('is built from empty by a case-insensitive allowlist, in the allowlist’s spelling', () => {
    const parent = {
      Path: 'C:\\Windows\\System32',
      SystemRoot: 'C:\\Windows',
      temp: 'C:\\Temp',
      TMP: 'C:\\Temp',
      UserProfile: 'C:\\Users\\Someone',
      AppData: 'C:\\Users\\Someone\\AppData\\Roaming',
      LocalAppData: 'C:\\Users\\Someone\\AppData\\Local',
      ProgramData: 'C:\\ProgramData',
      ComSpec: 'C:\\Windows\\System32\\cmd.exe',
      // None of these reaches the helper, whatever the case.
      Conch_Token: 'leak',
      CONCH_SERVER: 'https://elsewhere.example',
      conch_voice_attach: 'leak',
      Conch_Config_Dir: 'C:\\elsewhere',
      RUST_LOG: 'trace',
      rust_backtrace: '1',
      NODE_OPTIONS: '--require x',
      HOME: 'C:\\Users\\Someone',
    };
    const env = helperEnv({ parent, configDir, token: FAKE_TOKEN, nonce: NONCE, platform: 'win32' });
    expect(names(env)).toEqual([...PASSED_THROUGH_WINDOWS, 'CONCH_CONFIG_DIR', 'CONCH_TOKEN', 'CONCH_VOICE_ATTACH'].sort());
    expect(env['PATH']).toBe('C:\\Windows\\System32');
    expect(env['SYSTEMROOT']).toBe('C:\\Windows');
    expect(env['TEMP']).toBe('C:\\Temp');
    expect(env['CONCH_CONFIG_DIR']).toBe(configDir);
    expect(env['CONCH_TOKEN']).toBe(FAKE_TOKEN);
    expect(env['CONCH_VOICE_ATTACH']).toBe(NONCE);
    expect(JSON.stringify(env)).not.toContain('leak');
    expect(JSON.stringify(env)).not.toContain('elsewhere');
  });

  it('a name in two spellings is set once, and an empty or NUL-holding value is left out', () => {
    const parent = { PATH: 'a', Path: 'b', TEMP: '', TMP: 'x\0y' };
    const env = helperEnv({ parent, configDir, token: FAKE_TOKEN, nonce: NONCE, platform: 'win32' });
    expect(env['PATH']).toBe('a');
    expect(env).not.toHaveProperty('Path');
    expect(env).not.toHaveProperty('TEMP');
    expect(env).not.toHaveProperty('TMP');
  });

  it.each([
    ['a relative path', 'profiles\\p1'],
    ['a path rooted on the current drive', '\\profiles\\p1'],
    ['a Unix-style root', '/profiles/p1'],
    ['a drive-relative path', 'C:profiles'],
    ['an empty path', ''],
    ['a path with a NUL', 'C:\\a\0b'],
  ])('refuses %s for CONCH_CONFIG_DIR', (_what, bad) => {
    expect(() => helperEnv({ parent: {}, configDir: bad, token: FAKE_TOKEN, nonce: NONCE, platform: 'win32' })).toThrow(/absolute/);
  });

  it.each(['C:\\Users\\x', 'D:/Users/x', '\\\\server\\share\\x'])('takes %s', (ok) => {
    expect(helperEnv({ parent: {}, configDir: ok, token: FAKE_TOKEN, nonce: NONCE, platform: 'win32' })['CONCH_CONFIG_DIR']).toBe(ok);
  });

  it('a one-shot run gets the same allowlist and the config dir, and no token or nonce', () => {
    const env = oneShotEnv({ Path: 'p', Conch_Token: 'leak', RUST_LOG: 'x' }, configDir, 'win32');
    expect(names(env)).toEqual(['CONCH_CONFIG_DIR', 'PATH']);
  });

  it('the command line is --events-pipe <name>, never a secret', () => {
    const name = `${PIPE_PREFIX}${'a'.repeat(32)}`;
    expect(helperArgs(name, [], [FAKE_TOKEN, NONCE])).toEqual(['managed', '--events-pipe', name]);
    expect(() => helperArgs(name, [`--x=${NONCE}`], [FAKE_TOKEN, NONCE])).toThrow(/secret/);
  });

  it('is spawned hidden, not detached, with three pipes and the system root as its directory', () => {
    const env = helperEnv({ parent: { SystemRoot: 'C:\\Windows' }, configDir, token: FAKE_TOKEN, nonce: NONCE, platform: 'win32' });
    expect(spawnOptions(env, 'win32')).toEqual({ stdio: ['pipe', 'pipe', 'pipe'], detached: false, shell: false, windowsHide: true, env, cwd: 'C:\\Windows' });
    expect(oneShotSpawnOptions(env, 'win32').stdio).toEqual(['ignore', 'pipe', 'pipe']);
    expect(spawnOptions({}, 'win32').cwd).toBe('C:\\Windows');
    // Linux and macOS: unchanged.
    expect(spawnOptions(env, 'linux').cwd).toBe('/');
    expect(spawnOptions(env, 'darwin').cwd).toBe('/');
  });
});

// ------------------------------------------------ the configuration directory

describe('CONCH_CONFIG_DIR', () => {
  it('is the profile’s own folder under userData, and never another profile’s', () => {
    const root = join(dir, 'AppData', 'Roaming', 'Conch');
    const a = profileConfigDir(root, 'p1');
    const b = profileConfigDir(root, 'p2');
    expect(a).toBe(join(root, 'profiles', 'p1'));
    expect(b).toBe(join(root, 'profiles', 'p2'));
    expect(a.startsWith(root)).toBe(true);
    expect(a).not.toBe(b);
    expect(a.startsWith(b)).toBe(false);
    for (const bad of ['', '..', '../p2', 'p1/../p2', 'p1\\..\\p2', 'a b']) expect(() => profileConfigDir(root, bad), bad).toThrow(/invalid profile id/);
  });
});

// ------------------------------------------------- the supervisor on Windows

// FakeChild is the helper's process as the supervisor sees it: the three
// pipes, a pid, and kill(), which it records.
class FakeChild extends EventEmitter {
  readonly stdin = new PassThrough();
  readonly stdout = new PassThrough();
  readonly stderr = new PassThrough();
  readonly pid = 4242;
  readonly kills: (string | number | undefined)[] = [];
  readonly written: string[] = [];
  exited = false;
  // How the process answers: it leaves when its standard input ends, unless
  // it is told to hang, or only when it is killed.
  constructor(readonly behaviour: 'cooperative' | 'hung' = 'cooperative') {
    super();
    this.stdin.on('data', (c: Buffer) => {
      this.written.push(c.toString('utf8'));
    });
    this.stdin.on('end', () => {
      if (this.behaviour === 'cooperative') this.exit(0, null);
    });
  }

  kill(signal?: string | number): boolean {
    this.kills.push(signal);
    // Windows reports a forcibly ended process by an exit code, not a signal.
    this.exit(1, null);
    return true;
  }

  exit(code: number | null, signal: string | null): void {
    if (this.exited) return;
    this.exited = true;
    this.emit('exit', code, signal);
  }

  asChild(): ChildProcessWithoutNullStreams {
    return this as unknown as ChildProcessWithoutNullStreams;
  }
}

const request: StartRequest = {
  profileId: 'p1',
  configDir: 'C:\\Users\\Someone\\AppData\\Roaming\\Conch\\profiles\\p1',
  token: FAKE_TOKEN,
  server: 'https://conch.example.org',
  channel: 'general',
  principalId: 7,
};

const V2 = 'voice-control-v2/transcript-negotiation-v2-owner.json';

function helperLines(from: number, to: number): string {
  return transcript(V2)
    .slice(from, to)
    .map((s) => `${JSON.stringify(s.message)}\n`)
    .join('');
}

interface Rig {
  readonly sup: Supervisor;
  readonly h: Harness;
  readonly logs: string[];
  readonly states: VoiceState[];
  readonly spawns: { command: string; args: readonly string[]; env: Record<string, string>; cwd: string | undefined; child: FakeChild }[];
  child(): FakeChild;
}

function rig(opts: { behaviour?: 'cooperative' | 'hung'; nonce?: () => string; timings?: { helloTimeoutMs?: number; killAfterMs?: number; termAfterMs?: number }; extra?: EndpointDeps } = {}): Rig {
  const resources = join(dir, 'resources');
  mkdirSync(resources, { recursive: true });
  writeFileSync(join(resources, WINDOWS_HELPER_NAME), 'MZ');
  const h = pipes(opts.extra);
  const logs: string[] = [];
  const states: VoiceState[] = [];
  const spawns: Rig['spawns'] = [];
  const sup = new Supervisor({
    resources,
    env: { SystemRoot: 'C:\\Windows', Path: 'C:\\Windows\\System32', Conch_Token: 'leak', RUST_LOG: 'trace' },
    log: (l) => logs.push(l),
    platform: 'win32',
    ...(opts.nonce === undefined ? {} : { nonce: opts.nonce }),
    endpoint: h.deps,
    spawn: (command, args, options) => {
      const child = new FakeChild(opts.behaviour);
      spawns.push({ command, args, env: options.env as Record<string, string>, cwd: options.cwd as string | undefined, child });
      return child.asChild();
    },
    timings: { helloTimeoutMs: 5000, ackTimeoutMs: 5000, termAfterMs: 150, killAfterMs: 400, ...opts.timings },
  });
  sup.subscribe((st) => states.push(st));
  return {
    sup,
    h,
    logs,
    states,
    spawns,
    child() {
      const s = spawns.at(-1);
      if (s === undefined) throw new Error('nothing spawned');
      return s.child;
    },
  };
}

// helperJoins plays the helper up to a running session: connect to the pipe
// the owner named, the attach line (with the nonce from its environment), the
// hello, then the owner's start and the helper's answers.
async function helperJoins(r: Rig, generation = 1): Promise<Client> {
  await until(() => r.spawns.length >= generation, 5000, 'the spawn');
  const sp = r.spawns[generation - 1];
  if (sp === undefined) throw new Error('no spawn');
  const name = sp.args[2] ?? '';
  const nonce = sp.env['CONCH_VOICE_ATTACH'] ?? '';
  const c = new Client(r.h, name);
  await c.send(attachLine(nonce) + helperLines(0, 1));
  await until(() => sp.child.written.length > 0, 5000, 'the start command');
  await c.send(helperLines(2, 6));
  return c;
}

describe('the supervisor on Windows', () => {
  it('spawns conch-voice.exe with --events-pipe <random pipe name>, and the allowlisted environment', async () => {
    const r = rig();
    const started = r.sup.start(request);
    const c = await helperJoins(r);
    const st = await started;
    expect(st.status).toBe('running');
    const sp = r.spawns[0];
    expect(sp?.command.endsWith(WINDOWS_HELPER_NAME)).toBe(true);
    expect(sp?.args.slice(0, 2)).toEqual(['managed', '--events-pipe']);
    expect(sp?.args[2]).toMatch(/^\\\\\.\\pipe\\conch-voice-[0-9a-f]{32}$/);
    expect(sp?.cwd).toBe('C:\\Windows');
    const env = sp?.env ?? {};
    expect(Object.keys(env).sort()).toEqual(['CONCH_CONFIG_DIR', 'CONCH_TOKEN', 'CONCH_VOICE_ATTACH', 'PATH', 'SYSTEMROOT']);
    expect(env['CONCH_CONFIG_DIR']).toBe(request.configDir);
    expect(env['CONCH_TOKEN']).toBe(FAKE_TOKEN);
    expect(env['CONCH_VOICE_ATTACH']).toMatch(/^[A-Za-z0-9_-]{43}$/);
    // The nonce and the token are only in the environment.
    expect((sp?.args ?? []).join(' ')).not.toContain(env['CONCH_VOICE_ATTACH'] ?? '?');
    expect((sp?.args ?? []).join(' ')).not.toContain(FAKE_TOKEN);
    // Rule 3, across the whole session: nothing was written to the helper's
    // end of the events pipe.
    expect(c.bytesReceived).toBe(0);
    await r.sup.stop();
    expect(c.bytesReceived).toBe(0);
    c.socket.destroy();
  });

  it('the first line within the hello deadline from the spawn: no attach, failed attach-timeout, helper stopped', async () => {
    const r = rig({ timings: { helloTimeoutMs: 200 } });
    const st = await r.sup.start(request);
    expect(st).toEqual({ status: 'failed', generation: 1, reason: 'attach-timeout' });
    await until(() => r.child().exited, 5000, 'the helper to be reaped');
    expect(r.child().stdin.writableEnded).toBe(true);
    expect(r.child().written).toEqual([]);
  });

  it('a wrong nonce from a connection is closed, and the helper’s own connection still attaches', async () => {
    const r = rig();
    const started = r.sup.start(request);
    await until(() => r.spawns.length === 1, 5000, 'the spawn');
    const name = r.spawns[0]?.args[2] ?? '';
    const intruder = new Client(r.h, name);
    await intruder.send(attachLine(OTHER_NONCE));
    await until(() => intruder.closed, 3000, 'the intruder to be closed');
    expect(intruder.bytesReceived).toBe(0);
    const c = await helperJoins(r);
    expect((await started).status).toBe('running');
    await r.sup.stop();
    c.socket.destroy();
  });

  it('rule 4: after the attach the pipe accepts no more connections', async () => {
    const r = rig();
    const started = r.sup.start(request);
    const c = await helperJoins(r);
    await started;
    expect(await connectFails(r.h, r.spawns[0]?.args[2] ?? '')).toBe(true);
    await r.sup.stop();
    c.socket.destroy();
  });

  it('rule 5: the nonce and the token are in no log, state or diagnostic, before or after the attach, nor is its shape', async () => {
    const r = rig();
    const started = r.sup.start(request);
    await until(() => r.spawns.length === 1, 5000, 'the spawn');
    const sp = r.spawns[0];
    const nonce = sp?.env['CONCH_VOICE_ATTACH'] ?? '';
    // Before the attach: a careless helper prints both.
    sp?.child.stderr.write(`before token=${FAKE_TOKEN} nonce=${nonce}\n`);
    const c = await helperJoins(r);
    await started;
    // After the attach: the nonce is no longer kept, and its shape is replaced.
    sp?.child.stderr.write(`after nonce=${nonce} other=${OTHER_NONCE} token=${FAKE_TOKEN}\n`);
    await until(() => r.sup.diagnostics().length === 2, 5000, 'the stderr lines');
    await r.sup.stop();
    const seen = [JSON.stringify(r.states), JSON.stringify(r.logs), JSON.stringify(r.sup.diagnostics()), JSON.stringify(r.sup.state)].join('\n');
    expect(r.sup.diagnostics().join('\n')).toContain('before token=<redacted> nonce=<redacted>');
    expect(r.sup.diagnostics().join('\n')).toContain('after nonce=<redacted> other=<redacted> token=<redacted>');
    expect(seen).not.toContain(nonce);
    expect(seen).not.toContain(OTHER_NONCE);
    expect(seen).not.toContain(FAKE_TOKEN);
    c.socket.destroy();
  });

  it('rule 6: the nonce is not kept after the attach, and a new one is made for every spawn', async () => {
    const r = rig();
    const started = r.sup.start(request);
    const c1 = await helperJoins(r, 1);
    await started;
    const first = r.spawns[0]?.env['CONCH_VOICE_ATTACH'] ?? '';
    expect(first).toMatch(/^[A-Za-z0-9_-]{43}$/);
    // Nothing the run holds, but the process handle (which is the test's
    // fake) and the connection, has the nonce in it.
    const current = (r.sup as unknown as { current: unknown })['current'];
    expect(holds(current, first, new Set(['child', 'socket', 'listening']))).toBe(false);
    expect(holds(current, FAKE_TOKEN, new Set(['child', 'socket', 'listening']))).toBe(true);
    await r.sup.stop();
    c1.socket.destroy();

    const again = r.sup.start(request);
    const c2 = await helperJoins(r, 2);
    await again;
    const second = r.spawns[1]?.env['CONCH_VOICE_ATTACH'] ?? '';
    expect(second).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(second).not.toBe(first);
    expect(r.spawns[1]?.args[2]).not.toBe(r.spawns[0]?.args[2]);
    await r.sup.stop();
    c2.socket.destroy();
  });

  it('a nonce the helper does not know is never accepted: the old one, replayed to the new pipe, fails', async () => {
    let n = 0;
    const r = rig({ nonce: () => (n++ === 0 ? NONCE : OTHER_NONCE), timings: { helloTimeoutMs: 400 } });
    const first = r.sup.start(request);
    const c1 = await helperJoins(r, 1);
    await first;
    await r.sup.stop();
    c1.socket.destroy();
    const second = r.sup.start(request);
    await until(() => r.spawns.length === 2, 5000, 'the second spawn');
    const replay = new Client(r.h, r.spawns[1]?.args[2] ?? '');
    await replay.send(attachLine(NONCE));
    await until(() => replay.closed, 3000, 'the replay to be closed');
    expect(await second).toEqual({ status: 'failed', generation: 2, reason: 'attach-timeout' });
  });
});

// holds looks for a string in the values reachable from root: own properties,
// arrays, maps and sets, to a bounded depth, skipping the named keys.
function holds(root: unknown, needle: string, skip: ReadonlySet<string>): boolean {
  const seen = new Set<unknown>();
  const walk = (v: unknown, depth: number): boolean => {
    if (typeof v === 'string') return v.includes(needle);
    if (typeof v !== 'object' || v === null || depth > 8 || seen.has(v)) return false;
    seen.add(v);
    if (v instanceof Map) return [...v.entries()].some(([k, x]) => walk(k, depth + 1) || walk(x, depth + 1));
    if (v instanceof Set || Array.isArray(v)) return [...(v as Iterable<unknown>)].some((x) => walk(x, depth + 1));
    return Object.entries(v).some(([k, x]) => !skip.has(k) && walk(x, depth + 1));
  };
  return walk(root, 0);
}

describe('owner loss on Windows (§3.6)', () => {
  async function running(r: Rig): Promise<Client> {
    const started = r.sup.start(request);
    const c = await helperJoins(r);
    expect((await started).status).toBe('running');
    return c;
  }

  it.each([
    ['the events connection ends', (c: Client): void => {
      c.socket.end();
    }, 'events-ended'],
    ['the events connection is destroyed', (c: Client): void => {
      c.socket.destroy();
    }, 'events-ended'],
    ['a line that is not an event arrives', (c: Client): void => {
        c.socket.write('this is not json\n');
      }, 'malformed'],
    ['a line past the cap arrives', (c: Client): void => {
        c.socket.write(`${'z'.repeat(5000)}\n`);
      }, 'malformed'],
  ])('%s: lost (%s), and the helper is stopped and reaped', async (_what, act, reason) => {
    const r = rig();
    const c = await running(r);
    const child = r.child();
    act(c);
    await until(() => r.sup.state.status === 'lost', 5000, 'the loss');
    expect(r.sup.state).toEqual({ status: 'lost', generation: 1, reason });
    await until(() => child.exited, 5000, 'the helper to be reaped');
    // Standard input was closed: that is how the helper is told to leave.
    expect(child.stdin.writableEnded).toBe(true);
    // The loss is the one state, and it stays.
    expect(r.states.filter((s) => s.status === 'lost')).toHaveLength(1);
    expect(await connectFails(r.h, r.spawns[0]?.args[2] ?? '')).toBe(true);
    c.socket.destroy();
  });

  it('the helper’s own exit is a loss, exited', async () => {
    const r = rig();
    const c = await running(r);
    r.child().exit(1, null);
    await until(() => r.sup.state.status === 'lost', 5000, 'the loss');
    expect(r.sup.state).toEqual({ status: 'lost', generation: 1, reason: 'exited' });
    c.socket.destroy();
  });

  it('no SIGTERM step on Windows: a helper that hangs is killed once, with kill() and no signal, at the kill time', async () => {
    const r = rig({ behaviour: 'hung' });
    const c = await running(r);
    const child = r.child();
    const t0 = Date.now();
    await r.sup.stop();
    expect(child.kills).toEqual([undefined]);
    expect(Date.now() - t0).toBeGreaterThanOrEqual(350);
    expect(r.sup.state).toEqual({ status: 'idle' });
    expect(r.logs.some((l) => l.includes('SIGTERM'))).toBe(false);
    expect(r.logs.some((l) => l.includes('reaped'))).toBe(true);
    c.socket.destroy();
  });

  it('a cooperative helper leaves on the end of standard input: no kill at all, the pipe closed', async () => {
    const r = rig();
    const c = await running(r);
    const child = r.child();
    await r.sup.stop();
    expect(child.kills).toEqual([]);
    expect(child.stdin.writableEnded).toBe(true);
    expect(await connectFails(r.h, r.spawns[0]?.args[2] ?? '')).toBe(true);
    // The stop said leave first, on standard input only.
    expect(child.written.join('')).toContain('"leave"');
    c.socket.destroy();
  });

  it('a stop before any attach closes the pipe and reaps the helper', async () => {
    const r = rig();
    const started = r.sup.start(request);
    await until(() => r.spawns.length === 1, 5000, 'the spawn');
    const name = r.spawns[0]?.args[2] ?? '';
    await r.sup.stop();
    await started;
    expect(r.child().exited).toBe(true);
    expect(await connectFails(r.h, name)).toBe(true);
    expect(r.sup.state).toEqual({ status: 'idle' });
  });

  it('an invalid profile id is refused before a pipe is made or anything is spawned', async () => {
    const r = rig();
    const st = await r.sup.start({ ...request, profileId: '../evil' });
    expect(st).toEqual({ status: 'unavailable', reason: 'profile-id-invalid' });
    expect(r.spawns).toHaveLength(0);
    expect(r.h.names).toEqual([]);
  });

  it('no helper installed: unavailable, nothing listened on', async () => {
    const r = rig();
    rmSync(join(dir, 'resources', WINDOWS_HELPER_NAME));
    expect(await r.sup.start(request)).toEqual({ status: 'unavailable', reason: 'not-installed' });
    expect(r.h.names).toEqual([]);
  });
});
