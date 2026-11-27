import { chmod, lstat, mkdtemp, readFile, readdir, rm, stat, symlink, writeFile, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { PROBE_TEXT } from '../../main/vault/classify.ts';
import { createVault, type Vault, type VaultStorage } from '../../main/vault/vault.ts';

const SECRET = 'cnch_SECRET_CANARY_7f3a9c';
// What the fake seals with: Chromium's three-byte mark, then a readable body.
// v11 is the mark of a key from the keyring (classify.ts); the seal option
// replaces it (v10: Chromium's built-in key; anything else: unknown).
const BODY = 'FAKE-SEALED:';
const PREFIX = `v11${BODY}`;

interface FakeOpts {
  available?: boolean | Error;
  backend?: string;
  decryptError?: Error;
  reEncrypt?: boolean;
  gen?: number;
  encryptError?: Error;
  // The three-byte mark the fake seals with (default v11, the keyring's).
  seal?: string;
  // The mark the classification's probe comes back with (default: seal).
  probeSeal?: string;
  probeError?: Error;
}

class FakeStorage implements VaultStorage {
  // What the vault did with secrets; the classification's probes are counted
  // apart, so that these stay the vault's own steps.
  calls: string[] = [];
  probes = 0;
  constructor(public opts: FakeOpts = {}) {}
  isAsyncEncryptionAvailable(): Promise<boolean> {
    const a = this.opts.available ?? true;
    return a instanceof Error ? Promise.reject(a) : Promise.resolve(a);
  }
  getSelectedStorageBackend(): string {
    return this.opts.backend ?? 'gnome_libsecret';
  }
  encryptStringAsync(plain: string): Promise<Buffer> {
    if (plain === PROBE_TEXT) {
      this.probes += 1;
      if (this.opts.probeError) return Promise.reject(this.opts.probeError);
      return Promise.resolve(Buffer.from(`${this.opts.probeSeal ?? this.opts.seal ?? 'v11'}${BODY}probe`));
    }
    this.calls.push('encrypt');
    if (this.opts.encryptError) {
      return Promise.reject(this.opts.encryptError);
    }
    return Promise.resolve(Buffer.from(`${this.opts.seal ?? 'v11'}${BODY}${String(this.opts.gen ?? 1)}:${Buffer.from(plain).toString('base64')}`));
  }
  decryptStringAsync(data: Buffer): Promise<{ result: string; shouldReEncrypt: boolean }> {
    this.calls.push('decrypt');
    const s = data.toString();
    if (this.opts.decryptError || !s.slice(3).startsWith(BODY)) {
      return Promise.reject(this.opts.decryptError ?? new Error('bad data'));
    }
    return Promise.resolve({
      result: Buffer.from(s.slice(s.indexOf(':', 3 + BODY.length) + 1), 'base64').toString(),
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

// Issue #2077: on Linux a backend's name is not proof. A GNOME or COSMIC
// desktop with no keyring that answers names gnome_libsecret and seals with
// Chromium's built-in key (v10), which protects nothing.
describe('the keyring seal on Linux', () => {
  it('a backend that names a keyring but seals with the built-in key: put writes nothing and says no keyring', async () => {
    make({ seal: 'v10' });
    expect(await vault.classify()).toEqual({ kind: 'sessionOnly', reason: 'no-keyring' });
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'sessionOnly', reason: 'no-keyring' });
    expect(await readdir(root)).toEqual([]);
    // The secret itself was never sealed: only the public probe was.
    expect(storage.calls).toEqual([]);
  });

  it('a keyring that stops answering between the probe and the seal: the built-in-key ciphertext is not written, the old file stays', async () => {
    await vault.put('alpha', 'old');
    const before = await readFile(credFile('alpha'));
    storage.opts.seal = 'v10';
    storage.opts.probeSeal = 'v11';
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'sessionOnly', reason: 'no-keyring' });
    expect(await readFile(credFile('alpha'))).toEqual(before);
    expect(await readdir(join(root, 'profiles', 'alpha'))).toEqual(['credential.bin']);
    expect(logs).toContain('vault: no-keyring profile=alpha');
  });

  it('a seal mark this build does not know is not written', async () => {
    make({ seal: 'x99', probeSeal: 'v11' });
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'sessionOnly', reason: 'unknown-backend' });
    expect(await readdir(root)).toEqual([]);
  });

  it.each([
    ['protected now', {}],
    ['session-only now', { backend: 'basic_text' }],
    ['no keyring now', { seal: 'v10' }],
  ] as const)('a stored file sealed with the built-in key is deleted unread and reported as an older build’s, removed (%s)', async (_n, opts) => {
    await mkdir(join(root, 'profiles', 'alpha'), { recursive: true, mode: 0o700 });
    await writeFile(credFile('alpha'), `v10${BODY}1:${Buffer.from(SECRET).toString('base64')}`, { mode: 0o600 });
    await mkdir(join(root, 'profiles', 'beta'), { recursive: true, mode: 0o700 });
    await writeFile(credFile('beta'), 'v11other', { mode: 0o600 });
    make(opts);
    // Issue #2217's review: the caller is told it was an older build's entry,
    // deleted unread (so not ended at the server), not just unusable.
    expect(await vault.get('alpha')).toEqual({ kind: 'legacyRemoved' });
    expect(storage.calls).not.toContain('decrypt');
    await expect(stat(join(root, 'profiles', 'alpha'))).rejects.toThrow();
    // Another profile's file is not touched.
    expect(await readFile(credFile('beta'), 'utf8')).toBe('v11other');
    expect(logs).toContain('vault: built-in-key-removed profile=alpha');
    expect(await vault.get('alpha')).toEqual({ kind: 'absent' });
  });

  it('a re-encrypt that comes back sealed with the built-in key is not written; the keyring-sealed file stays', async () => {
    await vault.put('alpha', SECRET);
    const before = await readFile(credFile('alpha'));
    // The probe still says keyring; the re-seal of the secret does not.
    make({ reEncrypt: true, seal: 'v10', probeSeal: 'v11' });
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
    expect(await readFile(credFile('alpha'))).toEqual(before);
    expect(logs).toContain('vault: no-keyring profile=alpha');
  });

  it('win32 and darwin are not held to the Linux marks (their own protected seals start v10 too)', async () => {
    for (const platform of ['win32', 'darwin'] as const) {
      make({ seal: 'v10' }, platform);
      expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'stored' });
      expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
    }
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
    // The secret's re-encrypt fails; the classification's probe still answers.
    make({ reEncrypt: true, encryptError: new Error('nope') });
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
    await mkdir(join(root, 'profiles', 'alpha'), { recursive: true, mode: 0o700 });
    await writeFile(credFile('alpha'), bytes, { mode: 0o600 });
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
    await expect(vault.remove('never-was')).resolves.toEqual({ kind: 'removed' });
    await vault.put('alpha', 'a');
    await vault.remove('alpha');
    await expect(vault.remove('alpha')).resolves.toEqual({ kind: 'removed' });
  });

  it('removes a profile left with a stray temp file', async () => {
    await mkdir(join(root, 'profiles', 'alpha'), { recursive: true, mode: 0o700 });
    await writeFile(join(root, 'profiles', 'alpha', 'credential.bin.dead.tmp'), 'x');
    await vault.remove('alpha');
    expect(await readdir(join(root, 'profiles'))).toEqual([]);
  });
});

describe('unsafe root', () => {
  async function loosen(): Promise<void> {
    await chmod(root, 0o755);
  }

  it('put reports session-only unsafe-root, writes nothing', async () => {
    await loosen();
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'sessionOnly', reason: 'unsafe-root' });
    expect(await readdir(root)).toEqual([]);
  });

  it('get reports session-only unsafe-root and never decrypts', async () => {
    await vault.put('alpha', SECRET);
    await chmod(root, 0o755);
    storage.calls = [];
    expect(await vault.get('alpha')).toEqual({ kind: 'sessionOnly', reason: 'unsafe-root' });
    expect(storage.calls).toEqual([]);
    await chmod(root, 0o700);
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
  });
});

describe('remove failures', () => {
  it('returns a fixed failure, not a raw fs error with a path', async () => {
    await vault.put('alpha', 'a');
    await chmod(join(root, 'profiles'), 0o500);
    try {
      const r = await vault.remove('alpha');
      expect(r).toEqual({ kind: 'failed' });
      expect(JSON.stringify(r)).not.toContain(root);
    } finally {
      await chmod(join(root, 'profiles'), 0o700);
    }
  });
});

describe('directory fsync', () => {
  it('put is stored even when the directory fsync fails', async () => {
    storage = new FakeStorage();
    vault = createVault({
      storage, root, platform: 'linux', isReady: () => true, log: (l) => logs.push(l),
      syncDir: () => Promise.reject(new Error('EPERM')),
    });
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'stored' });
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
  });

  it('get with shouldReEncrypt does not log a failure when only the directory fsync fails', async () => {
    await vault.put('alpha', SECRET);
    storage = new FakeStorage({ reEncrypt: true, gen: 2 });
    vault = createVault({
      storage, root, platform: 'linux', isReady: () => true, log: (l) => logs.push(l),
      syncDir: () => Promise.reject(new Error('EPERM')),
    });
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
    expect(logs.filter((l) => l.includes('reencrypt-failed'))).toEqual([]);
    expect((await readFile(credFile('alpha'), 'utf8')).startsWith(`${PREFIX}2:`)).toBe(true);
  });
});

describe('one profile at a time', () => {
  // A decrypt that waits until the test lets it go.
  function gate(): { release: () => void; started: Promise<void> } {
    let release!: () => void;
    let started!: () => void;
    const hold = new Promise<void>((r) => { release = r; });
    const startedP = new Promise<void>((r) => { started = r; });
    const real = storage.decryptStringAsync.bind(storage);
    storage.decryptStringAsync = async (d) => {
      started();
      await hold;
      return real(d);
    };
    return { release, started: startedP };
  }

  it('a get re-encrypting cannot write back a token that remove just discarded', async () => {
    await vault.put('alpha', SECRET);
    make({ reEncrypt: true, gen: 2 });
    const g = gate();
    const got = vault.get('alpha');
    await g.started;
    const removed = vault.remove('alpha');
    await new Promise((r) => setTimeout(r, 50));
    g.release();
    await Promise.all([got, removed]);
    expect(await readdir(join(root, 'profiles'))).toEqual([]);
    expect(await vault.get('alpha')).toEqual({ kind: 'absent' });
  });

  it('a put during a re-encrypt of the old blob keeps the newer token', async () => {
    await vault.put('alpha', 'old-token');
    make({ reEncrypt: true, gen: 2 });
    const g = gate();
    const got = vault.get('alpha');
    await g.started;
    const put = vault.put('alpha', 'new-token');
    await new Promise((r) => setTimeout(r, 50));
    g.release();
    expect(await got).toEqual({ kind: 'found', secret: 'old-token' });
    expect(await put).toEqual({ kind: 'stored' });
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: 'new-token' });
  });

  it('different profiles do not wait for each other', async () => {
    await vault.put('alpha', 'a');
    await vault.put('beta', 'b');
    const g = gate();
    const slow = vault.get('alpha');
    await g.started;
    storage.decryptStringAsync = (d) => Promise.resolve({ result: d.toString().length > 0 ? 'b' : '', shouldReEncrypt: false });
    expect(await vault.get('beta')).toEqual({ kind: 'found', secret: 'b' });
    g.release();
    await slow;
  });

  it('a failing operation does not wedge the queue', async () => {
    await vault.put('alpha', 'a');
    make({ decryptError: new Error('x') });
    expect(await vault.get('alpha')).toEqual({ kind: 'unusable' });
    make();
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: 'a' });
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
      expect(line).toMatch(/^vault: [a-zA-Z-]+(?: profile=alpha)?$/);
    }
  });
});

// Issue #2217: at start, every entry an older build sealed with the built-in
// key (v10) is found and ended, opened only for its one hand-off to the
// server's revocation, or not opened at all, and deleted.
describe('the start sweep of entries sealed with the built-in key', () => {
  // endLegacy's decide callback: a fixed use, or none (delete unopened).
  const using =
    <T,>(use: (secret: string) => Promise<T>) =>
    (): Promise<(secret: string) => Promise<T>> =>
      Promise.resolve(use);
  const unopened = (): Promise<null> => Promise.resolve(null);

  // The file an older build wrote: the built-in key's mark and the fake's body.
  const legacyFile = (secret: string): string => `v10${BODY}1:${Buffer.from(secret).toString('base64')}`;

  async function plant(id: string, text: string, mode = 0o600): Promise<void> {
    await mkdir(join(root, 'profiles', id), { recursive: true, mode: 0o700 });
    await writeFile(credFile(id), text, { mode });
  }

  it('finds only the entries sealed with the built-in key, and only on Linux', async () => {
    await plant('alpha', legacyFile(SECRET));
    await plant('beta', 'v11other');
    await plant('gamma', `x99${BODY}unknown`);
    await plant('delta', legacyFile('second-secret'));
    expect(await vault.findLegacy()).toEqual(['alpha', 'delta']);
    // Finding opens nothing and changes nothing.
    expect(storage.calls).toEqual([]);
    expect(await readFile(credFile('alpha'), 'utf8')).toBe(legacyFile(SECRET));
    for (const platform of ['win32', 'darwin'] as const) {
      // Their own protected seals start v10 too.
      make({}, platform);
      expect(await vault.findLegacy()).toEqual([]);
      expect(await vault.endLegacy('alpha', using(() => Promise.resolve('used')))).toEqual({ kind: 'not-legacy' });
      expect(await readFile(credFile('alpha'), 'utf8')).toBe(legacyFile(SECRET));
    }
  });

  it('a missing vault, or one with no profiles folder, finds nothing', async () => {
    await rm(root, { recursive: true, force: true });
    expect(await vault.findLegacy()).toEqual([]);
    await mkdir(root, { mode: 0o700 });
    expect(await vault.findLegacy()).toEqual([]);
  });

  it('with a use: decrypts once, hands the secret only to the use, then deletes the entry', async () => {
    await plant('alpha', legacyFile(SECRET));
    await plant('beta', 'v11other');
    const handed: string[] = [];
    const ended = await vault.endLegacy('alpha', using((secret) => {
      handed.push(secret);
      return Promise.resolve('ended' as const);
    }));
    expect(ended).toEqual({ kind: 'ended', opened: true, used: 'ended', removed: true });
    expect(handed).toEqual([SECRET]);
    expect(storage.calls).toEqual(['decrypt']);
    await expect(stat(join(root, 'profiles', 'alpha'))).rejects.toThrow();
    // Not returned anywhere, and the other profile is not touched.
    expect(JSON.stringify(ended)).not.toContain(SECRET);
    expect(await readFile(credFile('beta'), 'utf8')).toBe('v11other');
    expect(await vault.get('alpha')).toEqual({ kind: 'absent' });
  });

  it('with no use: deletes the entry without decrypting it', async () => {
    await plant('alpha', legacyFile(SECRET));
    expect(await vault.endLegacy('alpha', unopened)).toEqual({ kind: 'ended', opened: false, removed: true });
    expect(storage.calls).toEqual([]);
    await expect(stat(join(root, 'profiles', 'alpha'))).rejects.toThrow();
  });

  it('an entry that cannot be opened, or whose use throws, is deleted all the same and reported unopened', async () => {
    await plant('alpha', legacyFile(SECRET));
    make({ decryptError: new Error(`bad ${SECRET}`) });
    let called = false;
    expect(
      await vault.endLegacy('alpha', using(() => {
        called = true;
        return Promise.resolve('ended');
      })),
    ).toEqual({ kind: 'ended', opened: false, removed: true });
    expect(called).toBe(false);
    await expect(stat(join(root, 'profiles', 'alpha'))).rejects.toThrow();

    make();
    await plant('beta', legacyFile(SECRET));
    expect(await vault.endLegacy('beta', using(() => Promise.reject(new Error(SECRET))))).toEqual({ kind: 'ended', opened: false, removed: true });
    await expect(stat(join(root, 'profiles', 'beta'))).rejects.toThrow();
  });

  it('a keyring-sealed (v11) entry is never opened or touched, with a use or without', async () => {
    await vault.put('alpha', SECRET);
    const before = await readFile(credFile('alpha'));
    storage.calls = [];
    let called = false;
    const use = (): Promise<string> => {
      called = true;
      return Promise.resolve('ended');
    };
    expect(await vault.findLegacy()).toEqual([]);
    expect(await vault.endLegacy('alpha', using(use))).toEqual({ kind: 'not-legacy' });
    expect(await vault.endLegacy('alpha', unopened)).toEqual({ kind: 'not-legacy' });
    expect(called).toBe(false);
    expect(storage.calls).toEqual([]);
    expect(await readFile(credFile('alpha'))).toEqual(before);
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
  });

  it('an entry a new sign-in replaced before its turn is left alone', async () => {
    await plant('alpha', legacyFile('old-secret'));
    expect(await vault.findLegacy()).toEqual(['alpha']);
    // The put is asked first; the sweep's turn comes after it.
    const put = vault.put('alpha', SECRET);
    const ended = vault.endLegacy('alpha', using(() => Promise.resolve('ended')));
    expect(await put).toEqual({ kind: 'stored' });
    expect(await ended).toEqual({ kind: 'not-legacy' });
    expect(await vault.get('alpha')).toEqual({ kind: 'found', secret: SECRET });
  });

  it('follows no symlink and leaves a foreign or odd file alone', async () => {
    const outside = await mkdtemp(join(tmpdir(), 'conch-vault-outside-'));
    try {
      // A credential file that is a symlink to a v10 file elsewhere.
      await writeFile(join(outside, 'target.bin'), legacyFile(SECRET), { mode: 0o600 });
      await mkdir(join(root, 'profiles', 'linkfile'), { recursive: true, mode: 0o700 });
      await symlink(join(outside, 'target.bin'), credFile('linkfile'));
      // A profile directory that is a symlink to a directory elsewhere.
      await mkdir(join(outside, 'dir'), { mode: 0o700 });
      await writeFile(join(outside, 'dir', 'credential.bin'), legacyFile(SECRET), { mode: 0o600 });
      await symlink(join(outside, 'dir'), join(root, 'profiles', 'linkdir'));
      // A name that is not a profile id, a file where a directory should be,
      // and a v10 file too large to be ours.
      await mkdir(join(root, 'profiles', 'not.an.id'), { mode: 0o700 });
      await writeFile(join(root, 'profiles', 'not.an.id', 'credential.bin'), legacyFile(SECRET), { mode: 0o600 });
      await writeFile(join(root, 'profiles', 'plainfile'), legacyFile(SECRET), { mode: 0o600 });
      await plant('huge', legacyFile(SECRET) + 'x'.repeat(70 * 1024));

      expect(await vault.findLegacy()).toEqual([]);
      for (const id of ['linkfile', 'linkdir', 'plainfile', 'huge']) {
        expect(await vault.endLegacy(id, using(() => Promise.resolve('ended')))).toEqual({ kind: 'not-legacy' });
      }
      expect(storage.calls).toEqual([]);
      expect(await readFile(join(outside, 'target.bin'), 'utf8')).toBe(legacyFile(SECRET));
      expect(await readFile(join(outside, 'dir', 'credential.bin'), 'utf8')).toBe(legacyFile(SECRET));
      expect((await lstat(credFile('linkfile'))).isSymbolicLink()).toBe(true);
      expect((await lstat(join(root, 'profiles', 'linkdir'))).isSymbolicLink()).toBe(true);
      expect(await readFile(join(root, 'profiles', 'not.an.id', 'credential.bin'), 'utf8')).toBe(legacyFile(SECRET));
      expect(await readFile(join(root, 'profiles', 'plainfile'), 'utf8')).toBe(legacyFile(SECRET));
      expect((await stat(credFile('huge'))).size).toBeGreaterThan(64 * 1024);
    } finally {
      await rm(outside, { recursive: true, force: true });
    }
  });

  // Issue #2217's review, N4: listed as v10, then swapped for a symlink to a
  // v10 file elsewhere before its turn: never opened, and the target is kept.
  it('an entry swapped for a symlink after the listing is left alone, and its target is not opened', async () => {
    const outside = await mkdtemp(join(tmpdir(), 'conch-vault-outside-'));
    try {
      await plant('alpha', legacyFile(SECRET));
      expect(await vault.findLegacy()).toEqual(['alpha']);
      await writeFile(join(outside, 'target.bin'), legacyFile('other-secret'), { mode: 0o644 });
      await chmod(join(outside, 'target.bin'), 0o644);
      await rm(credFile('alpha'));
      await symlink(join(outside, 'target.bin'), credFile('alpha'));
      let called = false;
      expect(
        await vault.endLegacy(
          'alpha',
          using(() => {
            called = true;
            return Promise.resolve('ended');
          }),
        ),
      ).toEqual({ kind: 'not-legacy' });
      expect(called).toBe(false);
      expect(storage.calls).toEqual([]);
      expect(await readFile(join(outside, 'target.bin'), 'utf8')).toBe(legacyFile('other-secret'));
    } finally {
      await rm(outside, { recursive: true, force: true });
    }
  });

  it('decides only once the entry is known to be sealed with the built-in key, inside its turn', async () => {
    await vault.put('alpha', SECRET);
    let asked = 0;
    const decide = (): Promise<null> => {
      asked++;
      return Promise.resolve(null);
    };
    expect(await vault.endLegacy('alpha', decide)).toEqual({ kind: 'not-legacy' });
    expect(asked).toBe(0);
    await plant('beta', legacyFile(SECRET));
    expect(await vault.endLegacy('beta', decide)).toEqual({ kind: 'ended', opened: false, removed: true });
    expect(asked).toBe(1);
  });

  it('a decrypt that never answers is given up at its bound, and the entry is deleted unopened', async () => {
    await plant('alpha', legacyFile(SECRET));
    vault = createVault({ storage, root, platform: 'linux', isReady: () => true, log: (l) => logs.push(l), legacyDecryptBoundMs: 20 });
    storage.decryptStringAsync = () => new Promise(() => undefined);
    let called = false;
    expect(
      await vault.endLegacy(
        'alpha',
        using(() => {
          called = true;
          return Promise.resolve('ended');
        }),
      ),
    ).toEqual({ kind: 'ended', opened: false, removed: true });
    expect(called).toBe(false);
    await expect(stat(join(root, 'profiles', 'alpha'))).rejects.toThrow();
  });

  // Exposed ciphertext cannot establish which origin owns the bearer.
  it.each(['v10', 'v11'] as const)('a %s file at 0644 is listed, then deleted without being read or decided on', async (mark) => {
    await plant('alpha', mark === 'v10' ? legacyFile(SECRET) : `v11${BODY}1:${Buffer.from(SECRET).toString('base64')}`, 0o644);
    await chmod(credFile('alpha'), 0o644);
    expect(await vault.findLegacy()).toEqual(['alpha']);
    let asked = false;
    const decide = (): Promise<null> => {
      asked = true;
      return Promise.resolve(null);
    };
    expect(await vault.endLegacy('alpha', decide)).toEqual({ kind: 'loose-mode', removed: true });
    expect(asked).toBe(false);
    expect(storage.calls).toEqual([]);
    await expect(stat(join(root, 'profiles', 'alpha'))).rejects.toThrow();
    expect(logs).toEqual([]);
  });

  it('a get removes a 0644 file unread and reports the exposure', async () => {
    await plant('alpha', legacyFile(SECRET), 0o644);
    await chmod(credFile('alpha'), 0o644);
    expect(await vault.get('alpha')).toEqual({ kind: 'exposed', removed: true });
    expect(storage.calls).toEqual([]);
    await expect(stat(credFile('alpha'))).rejects.toMatchObject({ code: 'ENOENT' });
  });

  it('a root that is not private to this user is not searched or touched', async () => {
    await plant('alpha', legacyFile(SECRET));
    await chmod(root, 0o755);
    expect(await vault.findLegacy()).toEqual([]);
    expect(await vault.endLegacy('alpha', using(() => Promise.resolve('ended')))).toEqual({ kind: 'not-legacy' });
    expect(storage.calls).toEqual([]);
    expect(await readFile(credFile('alpha'), 'utf8')).toBe(legacyFile(SECRET));
  });

  it('looks at no more than 256 entries of the profiles folder', async () => {
    for (let i = 0; i < 300; i++) await plant(`p${String(i).padStart(3, '0')}`, legacyFile(SECRET));
    const found = await vault.findLegacy();
    expect(found.length).toBe(256);
  });

  it('logs nothing, so no secret, ciphertext or path', async () => {
    await plant('alpha', legacyFile(SECRET));
    await plant('beta', legacyFile(SECRET));
    await plant('gamma', legacyFile(SECRET));
    await vault.findLegacy();
    await vault.endLegacy('alpha', using(() => Promise.resolve('ended')));
    await vault.endLegacy('beta', unopened);
    make({ decryptError: new Error(`bad ${SECRET} ${root}`) });
    await vault.endLegacy('gamma', using(() => Promise.resolve('ended')));
    expect(logs).toEqual([]);
  });
});

describe('platform', () => {
  it('win32 stores when the async API is available, without asking for a backend', async () => {
    make({ available: true, backend: 'basic_text' }, 'win32');
    expect(await vault.put('alpha', SECRET)).toEqual({ kind: 'stored' });
    expect(await tree(root)).toContain(credFile('alpha'));
  });
});
