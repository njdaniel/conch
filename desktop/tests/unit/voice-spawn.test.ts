// Which conch-voice may be run, the environment it gets and how it is
// spawned (issue #948; linux-desktop.md §4.1, §5.3).
import { ChildProcess } from 'node:child_process';
import { chmodSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { PassThrough } from 'node:stream';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { HELPER_NAME, HelperError, PASSED_THROUGH, findHelper, helperArgs, helperEnv, runDarwinTempUtility, spawnOptions, type OneShotChild, type OneShotSpawnFn } from '../../main/voice/spawn.ts';
import { FAKE_TOKEN, scratch, type Scratch } from './voice-support.ts';

let s: Scratch;
beforeEach(() => {
  s = scratch();
});
afterEach(() => {
  s.cleanup();
});

async function refusal(p: Promise<unknown>): Promise<string> {
  try {
    await p;
    return 'resolved';
  } catch (err) {
    return err instanceof HelperError ? err.reason : `other: ${String(err)}`;
  }
}

function install(mode = 0o755): string {
  const path = join(s.resources, HELPER_NAME);
  writeFileSync(path, '#!/bin/sh\nexit 0\n');
  chmodSync(path, mode);
  return path;
}

describe('the bundled executable', () => {
  it('is <resources>/conch-voice when it is a private regular executable', async () => {
    const path = install();
    expect(await findHelper(s.resources)).toBe(path);
  });

  it('missing is "not installed"', async () => {
    expect(await refusal(findHelper(s.resources))).toBe('not-installed');
  });

  it.each([
    ['group-writable', 0o775],
    ['writable by others', 0o757],
    ['not executable', 0o644],
  ])('%s is refused', async (_name, mode) => {
    install(mode);
    expect(await refusal(findHelper(s.resources))).toBe('untrusted');
  });

  it('a link is refused, even to a good file inside', async () => {
    const real = join(s.resources, 'real-voice');
    writeFileSync(real, '#!/bin/sh\n');
    chmodSync(real, 0o755);
    symlinkSync(real, join(s.resources, HELPER_NAME));
    expect(await refusal(findHelper(s.resources))).toBe('untrusted');
  });

  it('a directory is refused', async () => {
    mkdirSync(join(s.resources, HELPER_NAME));
    expect(await refusal(findHelper(s.resources))).toBe('untrusted');
  });

  it("a file of another user's (not root) is refused", async () => {
    install();
    expect(await refusal(findHelper(s.resources, { uid: (process.getuid?.() ?? 0) + 1 }))).toBe('untrusted');
  });

  it('a relative resources path is refused', async () => {
    expect(await refusal(findHelper('resources'))).toBe('untrusted');
  });

  it('a group-writable directory anywhere above it is refused', async () => {
    install();
    chmodSync(s.root, 0o775);
    expect(await refusal(findHelper(s.resources))).toBe('untrusted');
  });

  it.each([
    ['darwin', 'root', 80, 0o775, true],
    ['darwin', 'root', 0, 0o775, false],
    ['darwin', 'root', 20, 0o775, false],
    ['darwin', 'user', 80, 0o775, false],
    ['darwin', 'root', 80, 0o777, false],
    ['linux', 'root', 80, 0o775, false],
    ['linux', 'root', 0, 0o775, false],
  ] as const)('%s checks a %s-owned ancestor with gid %i, mode %i (trusted: %s)', async (platform, owner, gid, mode, trusted) => {
    const executable = realpathSync(install());
    const ancestor = realpathSync(s.root);
    const uid = process.getuid?.() || 501;
    const found = findHelper(s.resources, {
      platform,
      uid,
      ancestorStat: (dir) => Promise.resolve({
        ...(dir === ancestor ? { uid: owner === 'root' ? 0 : uid, gid, mode } : { uid: 0, gid: 0, mode: 0o755 }),
        isDirectory: () => true,
      }),
    });
    if (trusted) {
      await expect(found).resolves.toBe(executable);
    } else {
      await expect(found).rejects.toMatchObject({ reason: 'untrusted', trustCause: 'folder-writable' });
    }
  });

  it('a sticky directory above it that is not root-owned is refused', async () => {
    install();
    chmodSync(s.root, 0o1777);
    expect(await refusal(findHelper(s.resources))).toBe('untrusted');
  });

  it('a root-owned sticky directory above it (/tmp) is accepted', async () => {
    const tmp = lstatSync(realpathSync('/tmp'));
    expect(tmp.uid === 0 && (tmp.mode & 0o1002) === 0o1002, '/tmp is root-owned, sticky and world-writable').toBe(true);
    const base = mkdtempSync('/tmp/conch-voice-sticky-');
    try {
      const resources = join(base, 'resources');
      mkdirSync(resources, { mode: 0o755 });
      chmodSync(resources, 0o755);
      const path = join(resources, HELPER_NAME);
      writeFileSync(path, '#!/bin/sh\nexit 0\n');
      chmodSync(path, 0o755);
      expect(await findHelper(resources)).toBe(path);
    } finally {
      rmSync(base, { recursive: true, force: true });
    }
  });

  it('a resources directory others can write is refused', async () => {
    install();
    chmodSync(s.resources, 0o777);
    expect(await refusal(findHelper(s.resources))).toBe('untrusted');
  });
});

describe('the environment', () => {
  const nonce = 'FAKE_nonce_do_not_print_0123456789abcdefghA';
  const parent = {
    HOME: '/home/someone',
    XDG_RUNTIME_DIR: '/run/user/1000',
    PATH: '/usr/bin',
    CONCH_SERVER: 'https://elsewhere.example',
    CONCH_TOKEN: 'conch_parent_token',
    CONCH_VOICE_HELPER: '/tmp/other',
    RUST_LOG: 'trace',
    RUST_BACKTRACE: '1',
    LD_PRELOAD: '/tmp/evil.so',
    NODE_OPTIONS: '--require /tmp/evil.js',
    ELECTRON_RUN_AS_NODE: '1',
    DBUS_SESSION_BUS_ADDRESS: 'unix:path=/run/user/1000/bus',
  };

  it('is built from empty: the allowlist, the config directory, the token and the nonce, and nothing else', () => {
    const env = helperEnv({ parent, configDir: '/home/someone/.config/conch/p1', token: FAKE_TOKEN, nonce });
    expect(env).toEqual({
      HOME: '/home/someone',
      XDG_RUNTIME_DIR: '/run/user/1000',
      CONCH_CONFIG_DIR: '/home/someone/.config/conch/p1',
      CONCH_TOKEN: FAKE_TOKEN,
      CONCH_VOICE_ATTACH: nonce,
    });
    for (const name of Object.keys(env)) expect([...PASSED_THROUGH, 'CONCH_CONFIG_DIR', 'CONCH_TOKEN', 'CONCH_VOICE_ATTACH']).toContain(name);
  });

  it('refuses a relative configuration directory and an empty token', () => {
    expect(() => helperEnv({ parent, configDir: 'conf', token: FAKE_TOKEN, nonce })).toThrow(/absolute/);
    expect(() => helperEnv({ parent, configDir: '/c', token: ' ', nonce })).toThrow(/credential/);
  });

  it.each([
    ['/usr/bin/getconf', ['DARWIN_USER_TEMP_DIR']],
    ['/bin/ls', ['-lde', '/var/folders/example/T']],
  ] as const)('%s gets only LC_ALL=C', async (file, args) => {
    const stdout = new PassThrough();
    const stderr = new PassThrough();
    const stdio: OneShotChild['stdio'] = [null, stdout, stderr, null, null];
    const child = Object.assign(new ChildProcess(), { stdin: null, stdout, stderr, stdio });
    const spawn = vi.fn<OneShotSpawnFn>(() => child);
    const result = runDarwinTempUtility(file, [...args], spawn);
    stdout.end('output\n');
    child.emit('close', 0, null);

    expect(spawn).toHaveBeenCalledExactlyOnceWith(file, args, { env: { LC_ALL: 'C' }, stdio: ['ignore', 'pipe', 'pipe'], shell: false });
    await expect(result).resolves.toBe('output\n');
  });
});

describe('the command line and the spawn options', () => {
  it('is managed --events-pipe <path> and never holds a secret', () => {
    expect(helperArgs('/run/user/1000/conch/p1/e.sock', ['--mic', 'none'], [FAKE_TOKEN])).toEqual(['managed', '--events-pipe', '/run/user/1000/conch/p1/e.sock', '--mic', 'none']);
    expect(() => helperArgs('/s', [`--x=${FAKE_TOKEN}`], [FAKE_TOKEN])).toThrow(/secret/);
  });

  it('are three pipes, not detached, no shell', () => {
    const env = { A: 'b' };
    expect(spawnOptions(env)).toEqual({ stdio: ['pipe', 'pipe', 'pipe'], detached: false, shell: false, windowsHide: true, env, cwd: '/' });
  });

  it('no module under main/voice signals a process but through its ChildProcess, or spawns but through spawn.ts', () => {
    for (const file of ['supervisor.ts', 'endpoint.ts', 'protocol.ts', 'spawn.ts']) {
      const src = readFileSync(join(import.meta.dirname, '../../main/voice', file), 'utf8');
      expect(src, file).not.toMatch(/process\.kill\(|pkill|killall/);
      expect(src, file).not.toMatch(/\b(execFile|execSync|execFileSync|spawnSync|fork)\(/);
      // Only spawn.ts reaches child_process for more than its types.
      if (file !== 'spawn.ts') expect(src, file).not.toMatch(/^import \{[^}]*\} from 'node:child_process'/m);
    }
  });
});
