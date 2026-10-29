import { mkdtemp, readdir, rm, stat, symlink, writeFile, mkdir, readFile } from 'node:fs/promises';
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

  it('tightens a loose directory that already exists', async () => {
    await mkdir(join(root, 'profiles', 'alpha'), { recursive: true, mode: 0o777 });
    await writeCredential(root, 'alpha', Buffer.from('x'));
    expect(await mode(root)).toBe(0o700);
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
    await mkdir(join(root, 'profiles', 'alpha'), { recursive: true });
    const target = join(base, 'target');
    await writeFile(target, 'secret elsewhere');
    await symlink(target, join(root, 'profiles', 'alpha', 'credential.bin'));
    expect(await readCredential(root, 'alpha')).toEqual({ kind: 'corrupt' });
  });
});
