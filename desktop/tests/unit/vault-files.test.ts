import { chmod, mkdtemp, readdir, rm, stat, symlink, writeFile, mkdir, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { readCredential, writeCredential } from '../../main/vault/files.ts';

let base: string;
let root: string;

beforeEach(async () => {
  base = await mkdtemp(join(tmpdir(), 'conch-vault-files-'));
  root = join(base, 'data');
});

afterEach(async () => {
  await rm(base, { recursive: true, force: true });
});

const mode = async (p: string): Promise<number> => (await stat(p)).mode & 0o777;

describe('modes', () => {
  it('creates 0700 directories and a 0600 file', async () => {
    await writeCredential(root, 'alpha', Buffer.from('x'));
    expect(await mode(root)).toBe(0o700);
    expect(await mode(join(root, 'profiles'))).toBe(0o700);
    expect(await mode(join(root, 'profiles', 'alpha'))).toBe(0o700);
    expect(await mode(join(root, 'profiles', 'alpha', 'credential.bin'))).toBe(0o600);
  });

  it('still ends 0700/0600 under a permissive umask', async () => {
    const old = process.umask(0);
    try {
      await writeCredential(root, 'alpha', Buffer.from('x'));
    } finally {
      process.umask(old);
    }
    expect(await mode(root)).toBe(0o700);
    expect(await mode(join(root, 'profiles'))).toBe(0o700);
    expect(await mode(join(root, 'profiles', 'alpha'))).toBe(0o700);
    expect(await mode(join(root, 'profiles', 'alpha', 'credential.bin'))).toBe(0o600);
  });

  it('tightens loose directories inside a private root', async () => {
    await mkdir(root, { mode: 0o700 });
    await mkdir(join(root, 'profiles', 'alpha'), { recursive: true });
    await chmod(join(root, 'profiles'), 0o777);
    await chmod(join(root, 'profiles', 'alpha'), 0o777);
    await writeCredential(root, 'alpha', Buffer.from('x'));
    expect(await mode(join(root, 'profiles'))).toBe(0o700);
    expect(await mode(join(root, 'profiles', 'alpha'))).toBe(0o700);
  });
});

describe('atomic replacement', () => {
  it('leaves no temp file after a write', async () => {
    await writeCredential(root, 'alpha', Buffer.from('one'));
    await writeCredential(root, 'alpha', Buffer.from('two'));
    expect(await readdir(join(root, 'profiles', 'alpha'))).toEqual(['credential.bin']);
    expect(await readFile(join(root, 'profiles', 'alpha', 'credential.bin'), 'utf8')).toBe('two');
  });

  it('a failed write leaves the old file whole and no temp file', async () => {
    await writeCredential(root, 'alpha', Buffer.from('old'));
    // A non-bytes value makes the write fail after the temp file exists.
    const bad = 42 as unknown as Uint8Array;
    await expect(writeCredential(root, 'alpha', bad)).rejects.toThrow();
    expect(await readdir(join(root, 'profiles', 'alpha'))).toEqual(['credential.bin']);
    expect(await readFile(join(root, 'profiles', 'alpha', 'credential.bin'), 'utf8')).toBe('old');
  });

  it('the file is never visible half-written: it is renamed into place', async () => {
    // The final name appears only by rename, so a reader sees old or new.
    await writeCredential(root, 'alpha', Buffer.from('old'));
    const before = (await stat(join(root, 'profiles', 'alpha', 'credential.bin'))).ino;
    await writeCredential(root, 'alpha', Buffer.from('new'));
    const after = (await stat(join(root, 'profiles', 'alpha', 'credential.bin'))).ino;
    expect(after).not.toBe(before);
  });
});

describe('refusals', () => {
  it('refuses a symlinked profile directory', async () => {
    await mkdir(join(root, 'profiles'), { recursive: true });
    const elsewhere = join(base, 'elsewhere');
    await mkdir(elsewhere);
    await symlink(elsewhere, join(root, 'profiles', 'alpha'));
    await expect(writeCredential(root, 'alpha', Buffer.from('x'))).rejects.toThrow();
    expect(await readdir(elsewhere)).toEqual([]);
  });

  it('reads a symlinked credential file as corrupt, not through the link', async () => {
    await mkdir(root, { mode: 0o700 });
    await mkdir(join(root, 'profiles', 'alpha'), { recursive: true });
    const target = join(base, 'target');
    await writeFile(target, 'secret elsewhere');
    await symlink(target, join(root, 'profiles', 'alpha', 'credential.bin'));
    expect(await readCredential(root, 'alpha')).toEqual({ kind: 'corrupt' });
  });

  it('refuses a pre-existing root that group or others can reach', async () => {
    await mkdir(root, { mode: 0o755 });
    await chmod(root, 0o755);
    await expect(writeCredential(root, 'alpha', Buffer.from('x'))).rejects.toThrow();
    expect(await readdir(root)).toEqual([]);
    expect(await readCredential(root, 'alpha')).toEqual({ kind: 'unsafe-root' });
  });

  it('refuses a symlinked root', async () => {
    const real = join(base, 'real');
    await mkdir(real, { mode: 0o700 });
    await symlink(real, root);
    await expect(writeCredential(root, 'alpha', Buffer.from('x'))).rejects.toThrow();
    expect(await readdir(real)).toEqual([]);
  });

  it('reads through a symlinked profile directory as corrupt', async () => {
    await mkdir(join(root, 'profiles'), { recursive: true, mode: 0o700 });
    const elsewhere = join(base, 'elsewhere');
    await mkdir(elsewhere);
    await writeFile(join(elsewhere, 'credential.bin'), 'planted');
    await symlink(elsewhere, join(root, 'profiles', 'alpha'));
    expect(await readCredential(root, 'alpha')).toEqual({ kind: 'corrupt' });
  });

  it('an absent root reads as absent', async () => {
    expect(await readCredential(root, 'alpha')).toEqual({ kind: 'absent' });
  });
});

// Issue #2217's review, N4: the file is opened without following a symlink,
// checked again through its handle, and read through that handle, bounded.
describe('reading through the opened handle', () => {
  const file = (): string => join(root, 'profiles', 'alpha', 'credential.bin');

  it('a file swapped for a symlink between the check and the open is not read', async () => {
    await writeCredential(root, 'alpha', Buffer.from('v10ours'));
    const outside = join(base, 'outside.bin');
    await writeFile(outside, 'v10not-ours', { mode: 0o600 });
    const read = await readCredential(root, 'alpha', {
      beforeOpen: async () => {
        await rm(file());
        await symlink(outside, file());
      },
    });
    expect(read).toEqual({ kind: 'corrupt' });
    expect(await readFile(outside, 'utf8')).toBe('v10not-ours');
  });

  it('a file swapped for one open to others between the check and the open is not read', async () => {
    await writeCredential(root, 'alpha', Buffer.from('v10ours'));
    const read = await readCredential(root, 'alpha', {
      beforeOpen: async () => {
        await rm(file());
        await writeFile(file(), 'v10planted', { mode: 0o644 });
        await chmod(file(), 0o644);
      },
    });
    expect(read).toEqual({ kind: 'corrupt' });
  });

  it('a file that grows past the limit between the check and the read is not read', async () => {
    await writeCredential(root, 'alpha', Buffer.from('v10ours'));
    const read = await readCredential(root, 'alpha', {
      beforeOpen: async () => {
        await writeFile(file(), Buffer.alloc(70 * 1024, 0x41));
      },
    });
    expect(read).toEqual({ kind: 'corrupt' });
  });

  it('a file deleted between the check and the open is absent', async () => {
    await writeCredential(root, 'alpha', Buffer.from('v10ours'));
    expect(await readCredential(root, 'alpha', { beforeOpen: () => rm(file()) })).toEqual({ kind: 'absent' });
  });

  it('a file open to group or others is not read, even when nothing changes', async () => {
    await writeCredential(root, 'alpha', Buffer.from('v10ours'));
    await chmod(file(), 0o640);
    expect(await readCredential(root, 'alpha')).toEqual({ kind: 'corrupt' });
    await chmod(file(), 0o600);
    expect(await readCredential(root, 'alpha')).toEqual({ kind: 'bytes', data: Buffer.from('v10ours') });
  });
});

describe('directory fsync', () => {
  it('a failing directory fsync does not fail a write that has renamed', async () => {
    await writeCredential(root, 'alpha', Buffer.from('x'), {
      syncDir: () => Promise.reject(new Error('EPERM')),
    });
    expect(await readFile(join(root, 'profiles', 'alpha', 'credential.bin'), 'utf8')).toBe('x');
  });
});
