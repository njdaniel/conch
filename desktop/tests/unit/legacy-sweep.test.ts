// The start sweep of issue #2217 with the real vault on a temporary root
// (issue #2217's review, N1): a remove, a sign-out or a kept sign-in asked
// while the sweep waits on the server's /healthz must not delete or overwrite
// an older build's entry first. The sweep holds the profile's vault turn from
// before it asks the server; what is asked meanwhile runs after the entry is
// ended: revoked when the server answers, or deleted unopened with the notice.
import { chmod, mkdir, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import * as fs from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { HealthResult } from '../../main/compat.ts';
import { TeardownHooks } from '../../main/profiles/hooks.ts';
import { ProfilesService } from '../../main/profiles/service.ts';
import { ProfileStore } from '../../main/profiles/store.ts';
import { PROBE_TEXT } from '../../main/vault/classify.ts';
import { createVault, type Vault, type VaultStorage } from '../../main/vault/vault.ts';
import { fakeFactory, memoryFiles, ORIGIN_A, ORIGIN_B } from './profiles-support.ts';

vi.mock('node:fs/promises', { spy: true });

const A = 'a'.repeat(32);
const B = 'b'.repeat(32);
const LEGACY = 'tok-legacy-SECRET-canary-2217';
const ADA = { id: 7, name: 'ada', lasting: true, guest: false };

// Seals as Chromium marks it on Linux: v11 (the keyring's key) for every new
// write; a v10 file, as an older build left it, opens all the same.
class Storage implements VaultStorage {
  decrypts = 0;
  isAsyncEncryptionAvailable(): Promise<boolean> {
    return Promise.resolve(true);
  }
  getSelectedStorageBackend(): string {
    return 'gnome_libsecret';
  }
  encryptStringAsync(plain: string): Promise<Buffer> {
    return Promise.resolve(Buffer.from(plain === PROBE_TEXT ? 'v11FAKE:probe' : `v11FAKE:${Buffer.from(plain).toString('base64')}`));
  }
  decryptStringAsync(data: Buffer): Promise<{ result: string; shouldReEncrypt: boolean }> {
    this.decrypts++;
    const s = data.toString();
    if (!s.slice(3).startsWith('FAKE:')) return Promise.reject(new Error('bad data'));
    return Promise.resolve({ result: Buffer.from(s.slice(8), 'base64').toString(), shouldReEncrypt: false });
  }
}

let root: string;

beforeEach(async () => {
  root = await mkdtemp(join(tmpdir(), 'conch-legacy-sweep-'));
});

afterEach(async () => {
  vi.restoreAllMocks();
  await rm(root, { recursive: true, force: true });
});

const entry = (id: string): string => join(root, 'profiles', id, 'credential.bin');

async function plantCredential(id: string, token: string, mark = 'v10', mode = 0o600): Promise<void> {
  await mkdir(join(root, 'profiles', id), { recursive: true, mode: 0o700 });
  await writeFile(entry(id), `${mark}FAKE:${Buffer.from(token).toString('base64')}`, { mode });
  await chmod(entry(id), mode);
}

// A gate: the sweep's /healthz question to B waits on it.
function gate(): { readonly wait: Promise<HealthResult>; readonly open: (h: HealthResult) => void } {
  let open: (h: HealthResult) => void = () => undefined;
  const wait = new Promise<HealthResult>((resolve) => {
    open = resolve;
  });
  return { wait, open };
}

const tick = (ms = 20): Promise<void> => new Promise((r) => setTimeout(r, ms));

async function setup(
  health: Promise<HealthResult>,
  options: {
    readonly healthBoundMs?: number;
    readonly vaultBoundMs?: number;
    readonly plant?: { readonly mark: string; readonly mode: number; readonly profileMode?: number; readonly profilesMode?: number };
    readonly listingFails?: boolean;
  } = {},
) {
  const log: string[] = [];
  let askedB = 0;
  const ask = (origin: string): Promise<HealthResult> => (origin === ORIGIN_B && askedB++ === 0 ? health : Promise.resolve({ kind: 'answered', version: 'v1.0.0' }));
  const storage = new Storage();
  const vault: Vault = createVault({ storage, root, platform: 'linux', isReady: () => true, log: (l) => log.push(l) });
  if (options.listingFails === true) vault.findLegacy = () => Promise.reject(new Error('listing unavailable'));
  const files = memoryFiles(
    JSON.stringify({
      version: 1,
      profiles: [
        { id: A, label: 'a.example', origin: ORIGIN_A, principal: { id: 7, name: 'ada' } },
        { id: B, label: 'b.example', origin: ORIGIN_B, principal: { id: 7, name: 'ada' } },
      ],
      active: A,
    }),
  );
  const store = new ProfileStore(files);
  const factory = fakeFactory(log);
  const revoked: [string, string][] = [];
  const service = new ProfilesService({
    store,
    vault,
    hooks: new TeardownHooks(),
    createActivation: factory.create,
    probe: () => Promise.resolve('ok'),
    platform: 'linux',
    now: Date.now,
    log: (line) => log.push(`log: ${line}`),
    compat: { servers: ['v1.0.0'], override: false },
    desktopVersion: '0.0.0',
    // A starts active, so the first question to B is the sweep's (whichever
    // reader the build asks it with): it waits on the gate. B's own window,
    // later, is answered at once.
    readHealth: ask,
    readSweepHealth: ask,
    verifyStored: () => Promise.resolve({ kind: 'account', account: ADA }),
    revokeStored: (origin, token) => {
      revoked.push([origin, token]);
      return Promise.resolve('ended');
    },
    ...(options.healthBoundMs === undefined ? {} : { healthBoundMs: options.healthBoundMs }),
    ...(options.vaultBoundMs === undefined ? {} : { vaultBoundMs: options.vaultBoundMs }),
  });
  await plantCredential(B, LEGACY, options.plant?.mark, options.plant?.mode);
  if (options.plant?.profileMode !== undefined) await chmod(join(root, 'profiles', B), options.plant.profileMode);
  if (options.plant?.profilesMode !== undefined) await chmod(join(root, 'profiles'), options.plant.profilesMode);
  await service.start();
  if (options.plant === undefined) {
    // Once the sweep has asked B (and waits on the gate), B becomes active:
    // its old entry is not a sign-in.
    for (let i = 0; askedB === 0 && i < 200; i++) await tick(5);
    expect(askedB).toBe(1);
  } else {
    // Let the sweep finish before the tested profile becomes active.
    await service.swept;
  }
  const startupLog = [...log];
  await service.switchTo(B);
  const active = () => {
    const last = factory.made.at(-1);
    if (last === undefined) throw new Error('no activation');
    return last;
  };
  return { service, vault, storage, revoked, log, startupLog, active };
}

describe('an operation asked while the sweep waits on the server (issue #2217, N1)', () => {
  it('remove: the old entry is revoked first, then the profile goes', async () => {
    const h = gate();
    const s = await setup(h.wait);
    // B is active and signed out: its old entry was not used.
    expect(s.active().spec.token).toBeNull();
    const removing = s.service.remove(B);
    await tick();
    h.open({ kind: 'answered', version: 'v1.0.0' });
    expect(await removing).toBe(true);
    await s.service.swept;
    expect(s.revoked).toContainEqual([ORIGIN_B, LEGACY]);
    await expect(stat(join(root, 'profiles', B))).rejects.toThrow();
    expect(s.log).toContain('log: conch-desktop: a sign-in saved by an older build (v10): ended');
  });

  it('sign-out: the old entry is revoked, as well as the window’s own sign-in', async () => {
    const h = gate();
    const s = await setup(h.wait);
    s.active().held = 'tok-new';
    const signingOut = s.service.signOut();
    await tick();
    h.open({ kind: 'answered', version: 'v1.0.0' });
    await signingOut;
    await s.service.swept;
    expect(s.revoked).toContainEqual([ORIGIN_B, LEGACY]);
    await expect(stat(entry(B))).rejects.toThrow();
    expect(s.log).toContain('log: conch-desktop: a sign-in saved by an older build (v10): ended');
  });

  it('sign-out with the server unreachable: deleted unopened, and the restarted profile has the notice', async () => {
    const h = gate();
    const s = await setup(h.wait);
    s.active().held = 'tok-new';
    const signingOut = s.service.signOut();
    await tick();
    h.open({ kind: 'unreachable' });
    await signingOut;
    await s.service.swept;
    expect(s.revoked).not.toContainEqual([ORIGIN_B, LEGACY]);
    expect(s.storage.decrypts).toBe(0);
    await expect(stat(entry(B))).rejects.toThrow();
    expect(s.service.view().notice).toBe('legacy-not-ended');
  });

  it('a kept sign-in: the old entry is revoked first, and the new keyring-sealed entry survives', async () => {
    const h = gate();
    const s = await setup(h.wait);
    s.active().held = 'tok-new';
    const keeping = s.service.signedIn(ADA);
    await tick();
    h.open({ kind: 'answered', version: 'v1.0.0' });
    expect(await keeping).toBe(true);
    await s.service.swept;
    expect(s.revoked).toEqual([[ORIGIN_B, LEGACY]]);
    expect((await readFile(entry(B))).subarray(0, 3).toString('latin1')).toBe('v11');
    expect(await s.vault.get(B)).toEqual({ kind: 'found', secret: 'tok-new' });
    expect(s.log).toContain('log: conch-desktop: a sign-in saved by an older build (v10): ended');
  });

  it('a kept sign-in with the server unreachable: the notice, and the new entry survives', async () => {
    const h = gate();
    const s = await setup(h.wait);
    s.active().held = 'tok-new';
    const keeping = s.service.signedIn(ADA);
    await tick();
    h.open({ kind: 'unreachable' });
    expect(await keeping).toBe(true);
    await s.service.swept;
    expect(s.revoked).toEqual([]);
    expect(s.storage.decrypts).toBe(0);
    expect(await s.vault.get(B)).toEqual({ kind: 'found', secret: 'tok-new' });
    expect(s.service.view().notice).toBe('legacy-not-ended');
  });

  it('a server that never answers holds the profile’s turn no longer than the health bound', async () => {
    const s = await setup(new Promise<HealthResult>(() => undefined), { healthBoundMs: 30 });
    s.active().held = 'tok-new';
    // The kept sign-in waits behind the sweep's turn, which gives up on the
    // server at the health bound, and then lands within the vault bound.
    expect(await s.service.signedIn(ADA)).toBe(true);
    await s.service.swept;
    expect(s.storage.decrypts).toBe(0);
    expect(s.revoked).toEqual([]);
    expect(await s.vault.get(B)).toEqual({ kind: 'found', secret: 'tok-new' });
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(s.log).toContain('log: conch-desktop: a sign-in saved by an older build (v10): deleted-unreachable');
  });
});

describe('a credential file open to group or others (issue #2227)', () => {
  const healthy = Promise.resolve({ kind: 'answered', version: 'v1.0.0' } as const);

  it.each([
    { mark: 'v10', phase: 'during startup', listingFails: false },
    { mark: 'v11', phase: 'during startup', listingFails: false },
    { mark: 'v10', phase: 'after a failed listing', listingFails: true },
    { mark: 'v11', phase: 'after a failed listing', listingFails: true },
  ])('$mark at 0644: deleted unread with the notice $phase', async ({ mark, listingFails }) => {
    const s = await setup(healthy, { plant: { mark, mode: 0o644 }, listingFails });
    expect(s.storage.decrypts).toBe(0);
    expect(s.revoked).toEqual([]);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().notice).toBe('legacy-not-ended');
    await expect(stat(entry(B))).rejects.toMatchObject({ code: 'ENOENT' });
    expect(s.log).toContain('log: conch-desktop: a saved sign-in open to group or others: deleted-loose-mode');
    expect(s.log.join('\n')).not.toContain(LEGACY);
  });

  const refusedDirectory = 'log: conch-desktop: saved sign-in cleanup refused an unsafe vault directory';

  it.each([
    { mark: 'v11', mode: 0o644, profileMode: 0o777 },
    { mark: 'v10', mode: 0o600, profileMode: 0o770 },
  ])('warns about a $mark credential in a writable profile directory and keeps it intact', async (plant) => {
    const s = await setup(healthy, { plant });
    expect(s.startupLog).toContain(refusedDirectory);
    expect(s.storage.decrypts).toBe(0);
    expect(s.revoked).toEqual([]);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().notice).toBe('legacy-not-ended');
    s.active().held = 'tok-new';
    expect(await s.service.signedIn(ADA)).toBe(true);
    expect(s.service.view().storage).toEqual({ kind: 'session-only', reason: 'unsafe-root' });
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect((await stat(join(root, 'profiles', B))).mode & 0o777).toBe(plant.profileMode);
    expect(await readFile(entry(B), 'utf8')).toBe(`${plant.mark}FAKE:${Buffer.from(LEGACY).toString('base64')}`);
  });

  it('reports an unsafe profiles directory in the sweep and on get without traversing it', async () => {
    const list = vi.spyOn(fs, 'opendir');
    const inspect = vi.spyOn(fs, 'lstat');
    const s = await setup(healthy, { plant: { mark: 'v10', mode: 0o600, profilesMode: 0o770 } });
    expect(await s.vault.findLegacy()).toBe('unsafe-directory');
    expect(s.startupLog).toContain(refusedDirectory);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(await s.vault.get(B)).toEqual({ kind: 'unsafe-directory' });
    expect(await s.vault.remove(B)).toEqual({ kind: 'failed' });
    expect(await s.vault.put(B, 'tok-new')).toEqual({ kind: 'sessionOnly', reason: 'unsafe-root' });
    expect(list).not.toHaveBeenCalled();
    expect(inspect).not.toHaveBeenCalledWith(join(root, 'profiles', B));
    expect(inspect).not.toHaveBeenCalledWith(entry(B));
    expect(s.storage.decrypts).toBe(0);
    expect(s.revoked).toEqual([]);
    expect((await stat(entry(B))).isFile()).toBe(true);
    expect((await stat(join(root, 'profiles'))).mode & 0o777).toBe(0o770);
  });

  it('warns on lookup when the startup listing could not report the unsafe directory', async () => {
    const s = await setup(healthy, { listingFails: true, plant: { mark: 'v11', mode: 0o644, profileMode: 0o777 } });
    expect(s.startupLog).not.toContain(refusedDirectory);
    expect(s.log).toContain(refusedDirectory);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(s.service.view().storage).toEqual({ kind: 'session-only', reason: 'unsafe-root' });
    expect((await stat(entry(B))).isFile()).toBe(true);
  });

  it('warns after a timed-out get when opening an exposed file and then deleting it are denied', async () => {
    const denied = Object.assign(new Error('access denied'), { code: 'EACCES' });
    vi.spyOn(fs, 'open').mockRejectedValueOnce(denied);
    let failDeletion = (): void => undefined;
    const deletion = new Promise<void>((_resolve, reject) => {
      failDeletion = () => {
        reject(denied);
      };
    });
    const deleting = vi.spyOn(fs, 'rm').mockReturnValueOnce(deletion);
    const s = await setup(healthy, { listingFails: true, vaultBoundMs: 100, plant: { mark: 'v11', mode: 0o044 } });
    expect(deleting).toHaveBeenCalledWith(join(root, 'profiles', B), { recursive: true, force: true });
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().notice).toBeNull();
    failDeletion();
    await vi.waitFor(() => {
      expect(s.log).toContain('log: conch-desktop: a saved sign-in open to group or others: not-deleted');
      expect(s.service.view().notice).toBe('legacy-not-ended');
    });
    expect(s.storage.decrypts).toBe(0);
    expect(s.revoked).toEqual([]);
    expect((await stat(entry(B))).isFile()).toBe(true);
  });
});
