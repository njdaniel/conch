// MF7 (macos-desktop.md §3): use the Darwin rules with a fake TMPDIR on
// either Unix host. Only the listener cleanup test needs real Unix sockets.
import { chmodSync, existsSync, mkdirSync, readdirSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { connect } from 'node:net';
import { dirname, join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { listen, listenUnix, type EndpointDeps, type Listening } from '../../main/voice/endpoint.ts';
import { helperEnv, oneShotEnv } from '../../main/voice/spawn.ts';
import { FAKE_TOKEN, readFixture, scratch, type Scratch } from './voice-support.ts';

const mac = { platform: 'darwin' } as const;
const longId = 'p'.repeat(64);
const attached = readFixture<{ nonce: string }>('voice-control-pipe/attached.json');

describe('macOS endpoint', () => {
  let s: Scratch;
  beforeEach(() => { s = scratch(); });
  afterEach(() => { s.cleanup(); });

  function open(temp: string | undefined, deps: EndpointDeps = {}, profileId = longId) {
    return listenUnix({ TMPDIR: temp, XDG_RUNTIME_DIR: s.config }, profileId, { ...mac, ...deps });
  }

  it('uses <TMPDIR>/conch at 0o700 for a 64-byte profile id', async () => {
    const bind = vi.fn(() => Promise.resolve());
    const parent = join(s.run, 'conch');
    mkdirSync(parent, { mode: 0o500 });
    const endpoint = await open(`${s.run}/`, { listen: bind });
    try {
      expect(dirname(endpoint.path)).toBe(parent);
      expect(endpoint.path).toMatch(/\/e-[\w-]{12}\.sock$/);
      expect(Buffer.byteLength(endpoint.path)).toBeLessThanOrEqual(103);
      expect(bind).toHaveBeenCalledExactlyOnceWith(expect.anything(), endpoint.path);
      expect(statSync(parent).mode & 0o777).toBe(0o700);
      expect(readdirSync(parent)).toEqual([]);
      expect(readdirSync(s.config)).toEqual([]);
    } finally {
      endpoint.close();
    }
  });

  it.each([
    [undefined, 'runtime-dir-unset'],
    ['', 'runtime-dir-unset'],
    ['relative', 'runtime-dir-not-absolute'],
    ['/has\0nul', 'runtime-dir-not-absolute'],
  ])('refuses an unusable TMPDIR (%j) before bind', async (temp, reason) => {
    const bind = vi.fn(() => Promise.resolve());
    await expect(open(temp, { listen: bind })).rejects.toMatchObject({ reason });
    expect(bind).not.toHaveBeenCalled();
    expect(readdirSync(s.config)).toEqual([]);
  });

  it('checks TMPDIR ownership, permissions and file type before creating conch', async () => {
    const bind = vi.fn(() => Promise.resolve());
    const uid = statSync(s.run).uid;
    const file = join(s.root, 'file');
    const link = join(s.root, 'link');
    writeFileSync(file, '');
    symlinkSync(s.run, link);
    await expect(open(`${link}/`, { listen: bind })).rejects.toMatchObject({ reason: 'runtime-dir-symlink' });
    await expect(open(file, { listen: bind })).rejects.toMatchObject({ reason: 'runtime-dir-not-directory' });
    await expect(open(join(s.root, 'missing'), { listen: bind })).rejects.toMatchObject({ reason: 'runtime-dir-missing' });
    await expect(open(s.run, { listen: bind, uid: uid + 1 })).rejects.toMatchObject({ reason: 'runtime-dir-not-owned' });
    chmodSync(s.run, 0o750);
    await expect(open(s.run, { listen: bind })).rejects.toMatchObject({ reason: 'runtime-dir-not-private' });
    expect(bind).not.toHaveBeenCalled();
    expect(readdirSync(s.run)).toEqual([]);
  });

  it('refuses an invalid profile id or an unsafe shared parent', async () => {
    const bind = vi.fn(() => Promise.resolve());
    const parent = join(s.run, 'conch');
    await expect(open(s.run, { listen: bind }, '../other')).rejects.toMatchObject({ reason: 'profile-id-invalid' });
    expect(existsSync(parent)).toBe(false);
    symlinkSync(s.config, parent);
    await expect(open(s.run, { listen: bind })).rejects.toMatchObject({ reason: 'profile-dir-unsafe' });
    expect(bind).not.toHaveBeenCalled();
    expect(readdirSync(s.config)).toEqual([]);
  });

  it('with real Unix sockets, preserves another profile’s listener across stop and attach failure', async () => {
    const endpoints: Listening[] = [];
    try {
      const first = await open(s.run);
      endpoints.push(first);
      const second = await open(s.run, {}, 'second');
      endpoints.push(second);
      expect(second.path).not.toBe(first.path);
      expect(statSync(first.path).isSocket()).toBe(true);
      first.close();
      expect(existsSync(first.path)).toBe(false);

      const failed = await open(s.run, {}, 'failed');
      endpoints.push(failed);
      await expect(failed.attach(attached.nonce, Date.now())).rejects.toMatchObject({ reason: 'attach-timeout' });
      expect(existsSync(failed.path)).toBe(false);
      expect(statSync(second.path).isSocket()).toBe(true);

      const client = connect(second.path);
      client.on('error', () => undefined);
      try {
        client.write(`${JSON.stringify(attached)}\n`);
        const { socket } = await second.attach(attached.nonce, Date.now() + 2000);
        socket.destroy();
        expect(existsSync(second.path)).toBe(false);
      } finally {
        client.destroy();
      }
    } finally {
      for (const endpoint of endpoints) endpoint.close();
    }
  });
});

it.each([['darwin', 103], ['linux', 107]] as const)('%s accepts exactly %i UTF-8 path bytes and refuses one more before bind', async (platform, limit) => {
  const bind = vi.fn(() => Promise.resolve());
  const name = 'e-xxxxxxxxxxxx.sock';
  const multibyte = `/${'é'.repeat(30)}`;
  const dir = multibyte + 'a'.repeat(limit - Buffer.byteLength(join(multibyte, name)));
  const deps = { platform, name: () => name, listen: bind };
  const endpoint = await listen(dir, deps);
  try {
    expect(Buffer.byteLength(endpoint.path)).toBe(limit);
    await expect(listen(`${dir}a`, deps)).rejects.toMatchObject({ reason: 'socket-path-too-long' });
    expect(bind).toHaveBeenCalledExactlyOnceWith(expect.anything(), endpoint.path);
  } finally {
    endpoint.close();
  }
});

it('passes exactly the macOS allowlist to managed and one-shot helpers', () => {
  const allowed = {
    HOME: '/Users/someone', TMPDIR: '/var/folders/test/T/', LANG: 'en_US.UTF-8', LC_ALL: 'C', LC_CTYPE: 'C', LC_MESSAGES: 'C',
  };
  const parent = {
    ...allowed,
    tmpdir: '/other', XDG_RUNTIME_DIR: '/run/user/1000', PIPEWIRE_RUNTIME_DIR: '/run/user/1000', PIPEWIRE_REMOTE: 'pipewire-0',
    LC_TIME: 'C', PATH: '/usr/bin', DYLD_INSERT_LIBRARIES: '/tmp/test.dylib', DYLD_LIBRARY_PATH: '/tmp',
    LD_PRELOAD: '/tmp/test.so', NODE_OPTIONS: '--no-warnings', CONCH_TOKEN: 'parent-token', RUST_LOG: 'trace',
  };
  const configDir = '/Users/someone/Library/Application Support/Conch';
  const common = { ...allowed, CONCH_CONFIG_DIR: configDir };
  expect(oneShotEnv(parent, configDir, 'darwin')).toEqual(common);
  expect(helperEnv({ parent, configDir, token: FAKE_TOKEN, nonce: attached.nonce, ...mac })).toEqual({
    ...common, CONCH_TOKEN: FAKE_TOKEN, CONCH_VOICE_ATTACH: attached.nonce,
  });
});
