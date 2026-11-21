// MF7 (macos-desktop.md §3): use the Darwin rules with fake system utilities
// on either Unix host. Only the listener cleanup test needs real Unix sockets.
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
  beforeEach(() => { s = scratch('/tmp'); });
  afterEach(() => { s.cleanup(); });

  const noAcl = 'drwx------  2 user staff  64 Nov 20 12:00';

  function systemUtilities(temp: string | undefined, list = (dir: string) => `${noAcl} ${dir}\n`) {
    return vi.fn((file: string, args: string[]) => {
      if (file === '/usr/bin/getconf') {
        expect(args).toEqual(['DARWIN_USER_TEMP_DIR']);
        if (temp === undefined) return Promise.reject(new Error('getconf unavailable'));
        return Promise.resolve(`${temp}\n`);
      }
      expect(file).toBe('/bin/ls');
      expect(args).toEqual(['-lde', expect.any(String)]);
      return Promise.resolve().then(() => list(args[1] ?? ''));
    });
  }

  function open(temp: string | undefined, deps: EndpointDeps = {}, profileId = longId) {
    return listenUnix({ TMPDIR: s.config, XDG_RUNTIME_DIR: s.config }, profileId, {
      ...mac, execFile: systemUtilities(temp), ...deps,
    });
  }

  it('uses the system temp directory for a 64-byte profile id and ignores TMPDIR', async () => {
    const bind = vi.fn(() => Promise.resolve());
    const parent = join(s.run, 'conch');
    mkdirSync(parent, { mode: 0o500 });
    const execFile = systemUtilities(`${s.run}/`);
    const endpoint = await open(`${s.run}/`, { execFile, listen: bind });
    try {
      expect(dirname(endpoint.path)).toBe(parent);
      expect(endpoint.path).toMatch(/\/e-[\w-]{12}\.sock$/);
      expect(Buffer.byteLength(endpoint.path)).toBeLessThanOrEqual(103);
      expect(bind).toHaveBeenCalledExactlyOnceWith(expect.anything(), endpoint.path);
      expect(execFile.mock.calls).toEqual([
        ['/usr/bin/getconf', ['DARWIN_USER_TEMP_DIR']],
        ['/bin/ls', ['-lde', s.run]],
        ['/bin/ls', ['-lde', parent]],
      ]);
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
  ])('refuses an unusable getconf result (%j) before bind', async (temp, reason) => {
    const bind = vi.fn(() => Promise.resolve());
    await expect(open(temp, { listen: bind })).rejects.toMatchObject({ reason });
    expect(bind).not.toHaveBeenCalled();
    expect(readdirSync(s.config)).toEqual([]);
  });

  it('checks system temp ownership, exact mode and file type before creating conch', async () => {
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
    chmodSync(s.run, 0o500);
    await expect(open(s.run, { listen: bind })).rejects.toMatchObject({ reason: 'runtime-dir-not-private' });
    expect(bind).not.toHaveBeenCalled();
    expect(readdirSync(s.run)).toEqual([]);
  });

  it.each([
    ['temp', 'runtime-dir-not-private'],
    ['conch', 'profile-dir-unsafe'],
  ])('refuses an ACL or failed ACL inspection on the %s directory', async (target, reason) => {
    const bind = vi.fn(() => Promise.resolve());
    const conch = join(s.run, 'conch');
    const checkedDir = target === 'temp' ? s.run : conch;
    const listings = [
      `drwx------+  2 user staff  64 Nov 20 12:00 ${checkedDir}\n`,
      `drwx------@  2 user staff  64 Nov 20 12:00 ${checkedDir}\n 0: user:other inherited allow search,file_inherit\n`,
      '',
      null,
    ];
    for (const listing of listings) {
      const execFile = systemUtilities(s.run, (dir) => {
        if (dir !== checkedDir) return `${noAcl} ${dir}\n`;
        if (listing === null) throw new Error('ls unavailable');
        return listing;
      });
      await expect(open(s.run, { execFile, listen: bind })).rejects.toMatchObject({ reason });
      expect(execFile).toHaveBeenLastCalledWith('/bin/ls', ['-lde', checkedDir]);
    }
    expect(bind).not.toHaveBeenCalled();
    if (target === 'temp') expect(existsSync(conch)).toBe(false);
  });

  it('accepts an extended-attributes marker with no ACL lines', async () => {
    const bind = vi.fn(() => Promise.resolve());
    const execFile = systemUtilities(s.run, (dir) => `drwx------@  2 user staff  64 Nov 20 12:00 ${dir}\n`);
    const endpoint = await open(s.run, { execFile, listen: bind });
    try {
      expect(bind).toHaveBeenCalledExactlyOnceWith(expect.anything(), endpoint.path);
    } finally {
      endpoint.close();
    }
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
