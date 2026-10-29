import { mkdtemp, readFile, readdir, rm, stat, writeFile, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { createVault, type Vault, type VaultStorage } from '../../main/vault/vault.ts';

const SECRET = 'cnch_SECRET_CANARY_7f3a9c';
const PREFIX = 'FAKE-SEALED:';

interface FakeOpts {
  available?: boolean | Error;
  backend?: string;
  decryptError?: Error;
  reEncrypt?: boolean;
  gen?: number;
  encryptError?: Error;
}

class FakeStorage implements VaultStorage {
  calls: string[] = [];
  constructor(public opts: FakeOpts = {}) {}
  isAsyncEncryptionAvailable(): Promise<boolean> {
    const a = this.opts.available ?? true;
    return a instanceof Error ? Promise.reject(a) : Promise.resolve(a);
  }
  getSelectedStorageBackend(): string {
    return this.opts.backend ?? 'gnome_libsecret';
  }
  encryptStringAsync(plain: string): Promise<Buffer> {
    this.calls.push('encrypt');
    if (this.opts.encryptError) {
      return Promise.reject(this.opts.encryptError);
    }
    return Promise.resolve(Buffer.from(`${PREFIX}${String(this.opts.gen ?? 1)}:${Buffer.from(plain).toString('base64')}`));
  }
  decryptStringAsync(data: Buffer): Promise<{ result: string; shouldReEncrypt: boolean }> {
    this.calls.push('decrypt');
    const s = data.toString();
    if (this.opts.decryptError || !s.startsWith(PREFIX)) {
      return Promise.reject(this.opts.decryptError ?? new Error('bad data'));
    }
    return Promise.resolve({
      result: Buffer.from(s.slice(s.indexOf(':', PREFIX.length) + 1), 'base64').toString(),
      shouldReEncrypt: this.opts.reEncrypt ?? false,
    });
  }
}

let root: string;
let logs: string[];
let storage: FakeStorage;
let vault: Vault;

function make(opts: FakeOpts = {}, platform: NodeJS.Platform = 'linux'): void {
  storage = new FakeStorage(opts);
  vault = createVault({ storage, root, platform, isReady: () => true, log: (l) => logs.push(l) });
}

const credFile = (id: string): string => join(root, 'profiles', id, 'credential.bin');

async function tree(dir: string): Promise<string[]> {
  const out: string[] = [];
  for (const e of await readdir(dir, { withFileTypes: true, recursive: true })) {
    out.push(join(e.parentPath, e.name));
  }
  return out.sort();
}

beforeEach(async () => {
  root = await mkdtemp(join(tmpdir(), 'conch-vault-'));
  logs = [];
  make();
});

afterEach(async () => {
  await rm(root, { recursive: true, force: true });
});

describe('put and get', () => {
  it('stores nothing but ciphertext and reads it back', async () => {
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'stored' });
    const raw = await readFile(credFile('alpha'));
    expect(raw.toString()).not.toContain(SECRET);
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
  });

  it('replaces an earlier credential', async () => {
    await vault.put('alpha', 'old');
    await vault.put('alpha', SECRET);
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
    const names = (await readdir(join(root, 'profiles', 'alpha'))).sort();
    expect(names).toEqual(['credential.bin']);
  });

  it('get of a profile never stored is absent, even when storage is unavailable', async () => {
    make({ available: false });
    expect(await vault.get('nobody')).toEqual({ kind: 'absent' });
  });

  it.each([
    ['basic_text', { backend: 'basic_text' }, 'insecure-backend'],
    ['unknown', { backend: 'unknown' }, 'unknown-backend'],
    ['unavailable', { available: false }, 'unavailable'],
  ] as const)('%s: put writes nothing to disk and reports session-only', async (_n, opts, reason) => {
    make(opts);
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'sessionOnly', reason });
    expect(await readdir(root)).toEqual([]);
    expect(storage.calls).toEqual([]);
  });

  it('temporarily unavailable: put writes nothing and says retry', async () => {
    make({ available: new Error('keyring busy') });
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'temporarilyUnavailable' });
    expect(await readdir(root)).toEqual([]);
  });

  it('an encryption error writes nothing and keeps the old credential', async () => {
    await vault.put('alpha', 'old');
    storage.opts.encryptError = new Error(`cannot seal ${SECRET}`);
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'failed' });
    storage.opts.encryptError = undefined;
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: 'old' });
    expect(await readdir(join(root, 'profiles', 'alpha'))).toEqual(['credential.bin']);
  });
});

describe('get when storage later degrades', () => {
  it('basic_text: does not decrypt and does not erase', async () => {
    await vault.put('alpha', SECRET);
    const before = await readFile(credFile('alpha'));
    make({ backend: 'basic_text' });
    expect(await vault.get('alpha')).toEqual({ kind: 'sessionOnly', reason: 'insecure-backend' });
    expect(storage.calls).toEqual([]);
    expect(await readFile(credFile('alpha'))).toEqual(before);
  });

  it('temporarily unavailable is retryable, not an erase', async () => {
    await vault.put('alpha', SECRET);
    const before = await readFile(credFile('alpha'));
    make({ available: new Error('locked') });
    expect(await vault.get('alpha')).toEqual({ kind: 'temporarilyUnavailable' });
    expect(await readFile(credFile('alpha'))).toEqual(before);
    make();
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
  });
});

describe('shouldReEncrypt', () => {
  it('re-encrypts with the current key and rewrites the file', async () => {
    await vault.put('alpha', SECRET);
    const file = credFile('alpha');
    const old = await readFile(file, 'utf8');
    expect(old.startsWith(`${PREFIX}1:`)).toBe(true);
    make({ reEncrypt: true, gen: 2 });
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
    expect(storage.calls).toEqual(['decrypt', 'encrypt']);
    expect((await readFile(file, 'utf8')).startsWith(`${PREFIX}2:`)).toBe(true);
    expect((await stat(file)).mode & 0o777).toBe(0o600);
    expect(await readdir(join(root, 'profiles', 'alpha'))).toEqual(['credential.bin']);
  });

  it('does not rewrite when the provider does not ask', async () => {
    await vault.put('alpha', SECRET);
    make({ reEncrypt: false, gen: 2 });
    await vault.get('alpha');
    expect(storage.calls).toEqual(['decrypt']);
    expect((await readFile(credFile('alpha'), 'utf8')).startsWith(`${PREFIX}1:`)).toBe(true);
  });

  it('a failed re-encrypt still returns the secret and keeps the file', async () => {
    await vault.put('alpha', SECRET);
    const before = await readFile(credFile('alpha'));
    make({ reEncrypt: true });
    storage.encryptStringAsync = () => Promise.reject(new Error('nope'));
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
    expect(await readFile(credFile('alpha'))).toEqual(before);
  });
});

describe('unusable credentials', () => {
  it('a decrypt error marks only that profile unusable and touches no other file', async () => {
    await vault.put('alpha', 'a-secret');
    await vault.put('beta', 'b-secret');
    const alpha = await readFile(credFile('alpha'));
    const beta = await readFile(credFile('beta'));
    await writeFile(credFile('alpha'), 'not what the provider wrote');
    expect(await vault.get('alpha')).toEqual({ kind: 'unusable' });
    expect(await vault.get('beta')).toEqual({ kind: 'found', secret: 'b-secret' });
    expect(await readFile(credFile('beta'))).toEqual(beta);
    expect(await readFile(credFile('alpha'))).not.toEqual(alpha); // left as found, not erased
    // A fresh sign-in replaces it.
    await vault.put('alpha', 'again');
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: 'again' });
  });

  it('a provider that throws on decrypt is unusable, not an erase', async () => {
    await vault.put('alpha', SECRET);
    make({ decryptError: new Error('Decryption failed') });
    expect(await vault.get('alpha')).toEqual({ kind: 'unusable' });
    expect((await stat(credFile('alpha'))).size).toBeGreaterThan(0);
  });

  it.each([
    ['empty', Buffer.alloc(0)],
    ['oversized', Buffer.alloc(70 * 1024, 1)],
  ])('%s file is unusable', async (_n, bytes) => {
    await mkdir(join(root, 'profiles', 'alpha'), { recursive: true });
    await writeFile(credFile('alpha'), bytes);
    expect(await vault.get('alpha')).toEqual({ kind: 'unusable' });
    expect(storage.calls).toEqual([]);
  });
});

describe('remove', () => {
  it('deletes the file and directory, and only that profile', async () => {
    await vault.put('alpha', 'a');
    await vault.put('beta', 'b');
    await vault.remove('alpha');
    expect(await readdir(join(root, 'profiles'))).toEqual(['beta']);
    expect(await vault.get('beta')).toEqual({ kind: 'found', secret: 'b' });
  });

  it('missing files are not errors', async () => {
    await expect(vault.remove('never-was')).resolves.toBeUndefined();
    await vault.put('alpha', 'a');
    await vault.remove('alpha');
    await expect(vault.remove('alpha')).resolves.toBeUndefined();
  });

  it('removes a profile left with a stray temp file', async () => {
    await mkdir(join(root, 'profiles', 'alpha'), { recursive: true });
    await writeFile(join(root, 'profiles', 'alpha', 'credential.bin.dead.tmp'), 'x');
    await vault.remove('alpha');
    expect(await readdir(join(root, 'profiles'))).toEqual([]);
  });
});

describe('profile ids', () => {
  it.each(['', '..', '../x', 'a/b', 'a\\b', '.hidden', 'a b', 'x'.repeat(65), 'a\0b', '/etc'])(
    'rejects %j on every operation and touches nothing',
    async (id) => {
      await expect(vault.put(id, SECRET)).rejects.toThrow('invalid profile id');
      await expect(vault.get(id)).rejects.toThrow('invalid profile id');
      await expect(vault.remove(id)).rejects.toThrow('invalid profile id');
      expect(await readdir(root)).toEqual([]);
    },
  );
});

describe('logging', () => {
  it('never carries a secret, ciphertext or path, across every outcome', async () => {
    const ciphertextish = [SECRET, Buffer.from(SECRET).toString('base64'), PREFIX, root];
    await vault.put('alpha', SECRET);
    await vault.get('alpha');
    make({ backend: 'basic_text' });
    await vault.put('alpha', SECRET);
    await vault.get('alpha');
    make({ available: new Error(`down ${SECRET}`) });
    await vault.put('alpha', SECRET);
    await vault.get('alpha');
    make({ decryptError: new Error(`bad ${SECRET} ${root}`) });
    await vault.get('alpha');
    make({ encryptError: new Error(`bad ${SECRET} ${root}`) });
    await vault.put('alpha', SECRET);
    make({ reEncrypt: true });
    storage.encryptStringAsync = () => Promise.reject(new Error(`bad ${SECRET}`));
    await vault.get('alpha');
    await writeFile(credFile('alpha'), 'junk');
    make();
    await vault.get('alpha');

    expect(logs.length).toBeGreaterThan(5);
    for (const line of logs) {
      for (const bad of ciphertextish) {
        expect(line).not.toContain(bad);
      }
      expect(line).toMatch(/^vault: [a-zA-Z-]+ profile=alpha$/);
    }
  });
});

describe('platform', () => {
  it('win32 stores when the async API is available, without asking for a backend', async () => {
    make({ available: true, backend: 'basic_text' }, 'win32');
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'stored' });
    expect(await tree(root)).toContain(credFile('alpha'));
  });
});
