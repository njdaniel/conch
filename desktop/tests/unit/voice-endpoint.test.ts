// The owner's Linux events endpoint (issue #948, linux-desktop.md §5.2): the
// runtime-directory checks, conch/<profile-id>, the sun_path budget, binding
// a random name with a fresh one on a squat or EACCES, the stale sweep, and
// the attach. Every test runs in a private scratch directory; nothing here
// needs a network or a helper.
import { chmodSync, existsSync, linkSync, mkdirSync, readdirSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { connect, createServer, type Server, type Socket } from 'node:net';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { EndpointError, SUN_PATH_MAX_BYTES, listen, profileDir, runtimeDir, sweep, type Listening } from '../../main/voice/endpoint.ts';
import { fixture, readFixture, scratch, until, type Scratch } from './voice-support.ts';

let s: Scratch;
beforeEach(() => {
  s = scratch();
});
afterEach(() => {
  s.cleanup();
});

async function reason(p: Promise<unknown>): Promise<string> {
  try {
    await p;
    return 'resolved';
  } catch (err) {
    return err instanceof EndpointError ? err.reason : `other: ${String(err)}`;
  }
}

describe('the runtime directory', () => {
  it('is the verified XDG_RUNTIME_DIR', async () => {
    expect(await runtimeDir({ XDG_RUNTIME_DIR: s.run })).toBe(s.run);
  });

  it('unset is a refusal, never /tmp', async () => {
    expect(await reason(runtimeDir({}))).toBe('runtime-dir-unset');
    expect(await reason(runtimeDir({ XDG_RUNTIME_DIR: '' }))).toBe('runtime-dir-unset');
  });

  it('a relative path is a refusal', async () => {
    expect(await reason(runtimeDir({ XDG_RUNTIME_DIR: 'run' }))).toBe('runtime-dir-not-absolute');
  });

  it('a symlink is a refusal, and nothing is created behind it', async () => {
    const link = join(s.root, 'link');
    symlinkSync(s.run, link);
    expect(await reason(runtimeDir({ XDG_RUNTIME_DIR: link }))).toBe('runtime-dir-symlink');
    expect(readdirSync(s.run)).toEqual([]);
  });

  it("another user's is a refusal", async () => {
    expect(await reason(runtimeDir({ XDG_RUNTIME_DIR: s.run }, { uid: (process.getuid?.() ?? 0) + 1 }))).toBe('runtime-dir-not-owned');
    expect(readdirSync(s.run)).toEqual([]);
  });

  it('mode 0o750 is a refusal', async () => {
    chmodSync(s.run, 0o750);
    expect(await reason(runtimeDir({ XDG_RUNTIME_DIR: s.run }))).toBe('runtime-dir-not-private');
    expect(readdirSync(s.run)).toEqual([]);
  });

  it('a regular file is a refusal', async () => {
    const file = join(s.root, 'file');
    writeFileSync(file, '');
    expect(await reason(runtimeDir({ XDG_RUNTIME_DIR: file }))).toBe('runtime-dir-not-directory');
  });

  it('a missing one is a refusal', async () => {
    expect(await reason(runtimeDir({ XDG_RUNTIME_DIR: join(s.root, 'nope') }))).toBe('runtime-dir-missing');
    expect(existsSync(join(s.root, 'nope'))).toBe(false);
  });
});

describe('the profile directory', () => {
  it('is conch/<profile-id>, each level 0o700', async () => {
    const dir = await profileDir(s.run, 'p1');
    expect(dir).toBe(join(s.run, 'conch', 'p1'));
    expect(statSync(join(s.run, 'conch')).mode & 0o777).toBe(0o700);
    expect(statSync(dir).mode & 0o777).toBe(0o700);
  });

  it('refuses a profile id that is not one', async () => {
    expect(await reason(profileDir(s.run, '../x'))).toBe('profile-id-invalid');
  });

  it('refuses a conch directory that is a link or open to others', async () => {
    symlinkSync(s.config, join(s.run, 'conch'));
    expect(await reason(profileDir(s.run, 'p1'))).toBe('profile-dir-unsafe');
    expect(readdirSync(s.config)).toEqual([]);
  });
});

describe('the socket', () => {
  it('is bound under the profile directory with a random name, and its path is unlinked on close', async () => {
    const dir = await profileDir(s.run, 'p1');
    const l = await listen(dir);
    expect(l.path.startsWith(`${dir}/`)).toBe(true);
    expect(statSync(l.path).isSocket()).toBe(true);
    l.close();
    await until(() => !existsSync(l.path), 2000, 'the socket to go');
  });

  it('a path past 107 bytes is refused before anything listens', async () => {
    let p = s.run;
    while (Buffer.byteLength(join(p, 'conch', 'p1', 'e-xxxxxxxxxxxx.sock')) <= SUN_PATH_MAX_BYTES) {
      p = join(p, 'd'.repeat(10));
      mkdirSync(p, { mode: 0o700 });
    }
    const dir = await profileDir(p, 'p1');
    let listened = false;
    const got = await reason(listen(dir, { listen: () => { listened = true; return Promise.resolve(); } }));
    expect(got).toBe('socket-path-too-long');
    expect(listened).toBe(false);
  });

  it('a squatted name (EADDRINUSE) leads to a fresh one, and the squatter is left alone', async () => {
    const dir = await profileDir(s.run, 'p1');
    writeFileSync(join(dir, 'a.sock'), 'squatter');
    const names = ['a.sock', 'b.sock'];
    const logs: string[] = [];
    const l = await listen(dir, { name: () => names.shift() ?? 'c.sock', log: (x) => logs.push(x) });
    expect(l.path).toBe(join(dir, 'b.sock'));
    expect(logs.join('\n')).toContain('EADDRINUSE');
    expect(statSync(join(dir, 'a.sock')).isFile()).toBe(true);
    l.close();
  });

  it('an unwritable name (EACCES) leads to a fresh one', async () => {
    const dir = await profileDir(s.run, 'p1');
    const tried: string[] = [];
    const real = (server: Server, path: string): Promise<void> =>
      new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(path, () => {
          resolve();
        });
      });
    const l = await listen(dir, {
      name: () => (tried.length === 0 ? 'x.sock' : 'y.sock'),
      listen: (server, path) => {
        tried.push(path);
        if (tried.length === 1) return Promise.reject(Object.assign(new Error('denied'), { code: 'EACCES' }));
        return real(server, path);
      },
    });
    expect(tried).toEqual([join(dir, 'x.sock'), join(dir, 'y.sock')]);
    expect(l.path).toBe(join(dir, 'y.sock'));
    l.close();
  });
});

describe('the stale sweep', () => {
  it('removes only socket entries of this directory, follows no link and never leaves it', async () => {
    const dir = await profileDir(s.run, 'p1');
    // A stale socket: bound, linked to a second name, and the first closed
    // (Node unlinks the name it bound).
    const live = join(s.root, 'live.sock');
    const server = createServer();
    await new Promise<void>((r) => server.listen(live, r));
    linkSync(live, join(dir, 'stale.sock'));
    // A socket outside the directory, reached by a link inside it.
    linkSync(live, join(s.root, 'outside.sock'));
    symlinkSync(join(s.root, 'outside.sock'), join(dir, 'link.sock'));
    // A socket in a subdirectory, a regular file and a directory.
    mkdirSync(join(dir, 'sub'), { mode: 0o700 });
    linkSync(live, join(dir, 'sub', 'deep.sock'));
    writeFileSync(join(dir, 'file.sock'), '');
    await new Promise<void>((r) => server.close(() => { r(); }));

    expect(await sweep(dir)).toBe(1);
    expect(readdirSync(dir).sort()).toEqual(['file.sock', 'link.sock', 'sub']);
    expect(statSync(join(s.root, 'outside.sock')).isSocket()).toBe(true);
    expect(statSync(join(dir, 'sub', 'deep.sock')).isSocket()).toBe(true);
  });
});

describe('the attach', () => {
  const nonce = readFixture<{ fake_nonces: string[] }>('voice-control-pipe/sensitive.json').fake_nonces[0] ?? '';
  const other = readFixture<{ fake_nonces: string[] }>('voice-control-pipe/sensitive.json').fake_nonces[1] ?? '';
  const attachedLine = (n: string): string => `${JSON.stringify({ ...readFixture<object>('voice-control-pipe/attached.json'), nonce: n })}\n`;
  const hello = `${JSON.stringify(readFixture('voice-control/event-hello.json'))}\n`;

  let l: Listening;
  const clients: { socket: Socket; received: number }[] = [];
  beforeEach(async () => {
    l = await listen(await profileDir(s.run, 'p1'));
  });
  afterEach(() => {
    l.close();
    for (const c of clients) c.socket.destroy();
    clients.length = 0;
  });

  function client(): Promise<{ socket: Socket; received: number }> {
    return new Promise((resolve, reject) => {
      const c = { socket: connect(l.path), received: 0 };
      c.socket.on('data', (b: Buffer) => {
        c.received += b.length;
      });
      c.socket.once('connect', () => {
        clients.push(c);
        resolve(c);
      });
      c.socket.once('error', reject);
    });
  }

  async function closedListener(): Promise<void> {
    await until(() => !existsSync(l.path), 2000, 'the listener to close');
    expect(await reason(new Promise((resolve, reject) => {
      const c = connect(l.path, () => { resolve(c); });
      c.on('error', reject);
    }))).toMatch(/other: .*(ENOENT|ECONNREFUSED)/);
  }

  it('takes the first line with the right nonce and hands over what follows', async () => {
    const c = await client();
    c.socket.write(attachedLine(nonce) + hello);
    const a = await l.attach(nonce, Date.now() + 2000);
    const rest: Buffer[] = [Buffer.from(a.rest)];
    a.socket.on('data', (b: Buffer) => rest.push(b));
    a.socket.resume();
    await until(() => Buffer.concat(rest).toString() === hello, 2000, 'the hello');
    await closedListener();
    expect(c.received).toBe(0);
    a.socket.destroy();
  });

  it('a wrong nonce closes the listener and fails', async () => {
    const c = await client();
    c.socket.write(attachedLine(other));
    expect(await reason(l.attach(nonce, Date.now() + 2000))).toBe('attach-refused');
    await closedListener();
    expect(c.received).toBe(0);
  });

  it('a line past 4096 bytes closes the listener and fails, without waiting for its end', async () => {
    const c = await client();
    c.socket.write('x'.repeat(5000));
    expect(await reason(l.attach(nonce, Date.now() + 2000))).toBe('attach-too-long');
    await closedListener();
    expect(c.received).toBe(0);
  });

  it('a hello first (no attach line) fails', async () => {
    const c = await client();
    c.socket.write(hello);
    expect(await reason(l.attach(nonce, Date.now() + 2000))).toBe('attach-refused');
    expect(c.received).toBe(0);
  });

  it('no line before the deadline closes the listener and fails', async () => {
    const c = await client();
    expect(await reason(l.attach(nonce, Date.now() + 200))).toBe('attach-timeout');
    await closedListener();
    expect(c.received).toBe(0);
  });

  it('no connection before the deadline fails', async () => {
    expect(await reason(l.attach(nonce, Date.now() + 200))).toBe('attach-timeout');
    await closedListener();
  });

  it('a second connection closes the listener and fails, even with the right nonce on the first', async () => {
    const first = await client();
    const pending = reason(l.attach(nonce, Date.now() + 2000));
    const second = await client();
    expect(await pending).toBe('attach-second-connection');
    first.socket.write(attachedLine(nonce));
    await closedListener();
    expect(first.received + second.received).toBe(0);
  });

  it('the nonce is in no log line and no error', async () => {
    const logs: string[] = [];
    const ll = await listen(await profileDir(s.run, 'p2'), { log: (x) => logs.push(x) });
    const c = connect(ll.path);
    c.on('error', () => undefined);
    c.write(attachedLine(other));
    let message = '';
    try {
      await ll.attach(nonce, Date.now() + 2000);
    } catch (err) {
      message = err instanceof Error ? `${err.message} ${err.stack ?? ''}` : '';
    }
    c.destroy();
    ll.close();
    expect(message).toContain('attach-refused');
    for (const n of [nonce, other]) {
      expect(message).not.toContain(n);
      expect(logs.join('\n')).not.toContain(n);
    }
  });

  it('the attach fixture names a nonce this test uses', () => {
    expect(readFixture<{ nonce: string }>('voice-control-pipe/attached.json').nonce).toBe(nonce);
    expect(existsSync(fixture('voice-control-pipe/attached.json'))).toBe(true);
  });
});
