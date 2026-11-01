import { describe, expect, it } from 'vitest';

import { TeardownHooks } from '../../main/profiles/hooks.ts';
import type { HealthResult } from '../../main/compat.ts';
import { ProfilesService, type ProbeResult, type StoredCheck } from '../../main/profiles/service.ts';
import { ProfileStore } from '../../main/profiles/store.ts';
import { fakeFactory, fakeVault, memoryFiles, ORIGIN_A, ORIGIN_B, type FakeActivation, type FakeVault } from './profiles-support.ts';

async function setup(options: { vault?: FakeVault; probe?: ProbeResult; stepBoundMs?: number; files?: string; vaultBoundMs?: number; verify?: (origin: string, token: string) => StoredCheck; servers?: string[]; override?: boolean; health?: () => Promise<HealthResult>; healthBoundMs?: number } = {}) {
  const log: string[] = [];
  const files = memoryFiles(options.files);
  const store = new ProfileStore(files);
  const vault = options.vault ?? fakeVault();
  const factory = fakeFactory(log);
  const hooks = new TeardownHooks();
  const revoked: [string, string][] = [];
  const verified: [string, string][] = [];
  const asked: string[] = [];
  let clock = 0;
  const service = new ProfilesService({
    store,
    vault,
    hooks,
    createActivation: factory.create,
    probe: () => Promise.resolve(options.probe ?? 'ok'),
    platform: 'linux',
    now: () => ++clock,
    log: (line) => log.push(`log: ${line}`),
    // By default the server says the stored credential is the member the
    // profile is bound to.
    // By default the server says it is a release on the list.
    compat: { servers: options.servers ?? ['v1.0.0'], override: options.override ?? false },
    desktopVersion: '0.0.0',
    readHealth: (origin) => {
      asked.push(origin);
      return (options.health ?? (() => Promise.resolve<HealthResult>({ kind: 'answered', version: 'v1.0.0' })))();
    },
    verifyStored: (origin, token) => {
      verified.push([origin, token]);
      if (options.verify !== undefined) return Promise.resolve(options.verify(origin, token));
      const bound = store.list().find((p) => p.origin === origin && p.principal !== null)?.principal;
      return Promise.resolve(bound === undefined || bound === null ? { kind: 'rejected' } : { kind: 'account', account: { id: bound.id, name: bound.name, lasting: true, guest: false } });
    },
    revokeStored: (origin, token) => {
      revoked.push([origin, token]);
      return Promise.resolve('ended');
    },
    ...(options.stepBoundMs === undefined ? {} : { stepBoundMs: options.stepBoundMs }),
    ...(options.vaultBoundMs === undefined ? {} : { vaultBoundMs: options.vaultBoundMs }),
    ...(options.healthBoundMs === undefined ? {} : { healthBoundMs: options.healthBoundMs }),
  });
  await service.start();
  const active = (): FakeActivation => {
    const last = factory.made.at(-1);
    if (last === undefined) throw new Error('no activation');
    return last;
  };
  return { service, store, vault, factory, hooks, files, log, active, revoked, verified, asked };
}

const ADA = { id: 7, name: 'ada', lasting: true, guest: false };
const GRACE = { id: 8, name: 'grace', lasting: true, guest: false };

async function addAndSwitch(s: Awaited<ReturnType<typeof setup>>, origin: string): Promise<string> {
  const added = await s.service.add(origin);
  if (!added.ok) throw new Error('add failed');
  expect(await s.service.switchTo(added.id)).toBe(true);
  return added.id;
}

describe('adding profiles', () => {
  it('uses the address rules and asks the address what it is before saving', async () => {
    const s = await setup();
    expect(await s.service.add('http://example.org')).toEqual({ ok: false, refusal: 'scheme' });
    expect(await s.service.add('https://u:p@a.example')).toEqual({ ok: false, refusal: 'userinfo' });
    expect(s.store.list()).toHaveLength(0);
    expect(await s.service.add(ORIGIN_A)).toMatchObject({ ok: true });
    expect(s.store.list()).toHaveLength(1);
    for (const [probe, refusal] of [['certificate', 'certificate'], ['not-conch', 'not-conch'], ['unreachable', 'unreachable']] as const) {
      const t = await setup({ probe });
      expect(await t.service.add(ORIGIN_A)).toEqual({ ok: false, refusal });
      expect(t.store.list()).toHaveLength(0);
    }
  });

  it('shows the host, and the account for a bound profile, in the view', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    expect(await s.service.signedIn(ADA)).toBe(true);
    const view = s.service.view();
    expect(view.active).toBe(id);
    expect(view.profiles).toEqual([{ id, label: 'a.example', host: 'a.example', origin: ORIGIN_A, account: 'ada', active: true }]);
    expect(JSON.stringify(view)).not.toMatch(/token|secret/i);
  });
});

describe('binding', () => {
  it('refuses another principal on a bound profile, forgets and revokes that credential, and keeps the first', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    expect(await s.service.signedIn(ADA)).toBe(true);
    expect(s.vault.secrets.get(id)).toBe('tok-ada');
    s.active().held = 'tok-grace';
    expect(await s.service.signedIn(GRACE)).toBe(false);
    expect(s.active().signedOut).toBe(true);
    expect(s.service.view().notice).toBe('account-mismatch');
    expect(s.store.get(id)?.principal?.name).toBe('ada');
    // The first account's credential was not replaced.
    expect(s.vault.secrets.get(id)).toBe('tok-ada');
  });

  it('puts a second account of the same server in a second profile', async () => {
    const s = await setup();
    const one = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    const two = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-grace';
    expect(await s.service.signedIn(GRACE)).toBe(true);
    expect(s.vault.secrets.get(one)).toBe('tok-ada');
    expect(s.vault.secrets.get(two)).toBe('tok-grace');
    // Signing in again as ada on the second profile does not make a duplicate.
    s.active().held = 'tok-ada2';
    expect(await s.service.signedIn(ADA)).toBe(false);
    expect(s.vault.secrets.get(two)).toBe('tok-grace');
  });

  it('binds and keeps nothing for a guest, and keeps nothing for an operator’s window sign-in', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-guest';
    expect(await s.service.signedIn({ id: 99, name: 'guest', lasting: false, guest: true })).toBe(true);
    expect(s.store.get(id)?.principal).toBeNull();
    s.active().held = 'tok-op';
    expect(await s.service.signedIn({ id: 1, name: 'op', lasting: false, guest: false })).toBe(true);
    expect(s.store.get(id)?.principal?.name).toBe('op');
    expect(s.vault.secrets.size).toBe(0);
  });
});

describe('credentials and storage', () => {
  it('with protected storage a profile starts signed in from the vault, as after a restart', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-ada');
  });

  it('with session-only storage nothing is written, the token is held in memory only, and Retry puts it when storage turns protected', async () => {
    const vault = fakeVault({ kind: 'sessionOnly', reason: 'insecure-backend' });
    const s = await setup({ vault });
    expect(s.service.view().storage).toEqual({ kind: 'session-only', reason: 'insecure-backend' });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    expect(vault.secrets.size).toBe(0);
    // Switching away and back keeps the sign-in of this run, in memory.
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-ada');
    const getsBefore = vault.calls.filter((c) => c.startsWith('get')).length;
    // Retry while still unprotected changes nothing.
    expect((await s.service.retry()).storage).toEqual({ kind: 'session-only', reason: 'insecure-backend' });
    expect(vault.secrets.size).toBe(0);
    // Storage becomes protected: the in-memory token is put, not read back.
    vault.classification = { kind: 'protected' };
    expect((await s.service.retry()).storage).toEqual({ kind: 'protected' });
    expect(vault.secrets.get(id)).toBe('tok-ada');
    expect(vault.calls.filter((c) => c.startsWith('get')).length).toBe(getsBefore);
  });

  it('a stored credential that cannot be used is "sign in again" and touches no other profile', async () => {
    const s = await setup();
    const one = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-1';
    await s.service.signedIn(ADA);
    const two = await addAndSwitch(s, ORIGIN_B);
    s.active().held = 'tok-2';
    await s.service.signedIn(ADA);
    s.vault.unusable.add(one);
    await s.service.switchTo(one);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().notice).toBe('sign-in-again');
    expect(s.vault.secrets.get(two)).toBe('tok-2');
    expect(s.vault.calls).not.toContain(`remove ${one}`);
  });

  it('a temporarily unavailable store is a retryable state, shown as such', async () => {
    const s = await setup({ vault: fakeVault({ kind: 'temporarilyUnavailable' }) });
    expect(s.service.view().storage).toEqual({ kind: 'unavailable' });
  });

  it('shows the storage state before any sign-in, including the unsafe data directory', async () => {
    const s = await setup({ vault: fakeVault({ kind: 'sessionOnly', reason: 'unsafe-root' }) });
    expect(s.service.view().storage).toEqual({ kind: 'session-only', reason: 'unsafe-root' });
    expect(s.service.view().active).toBeNull();
  });
});

describe('sign-out', () => {
  it('runs the media hooks first, forgets the credential, deletes the entry whatever the storage, and starts the profile again without it', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.hooks.media('voice', () => {
      s.log.push('hook:voice');
    });
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    s.log.length = 0;
    expect(await s.service.signOut()).toBe('ended');
    const order = s.log.filter((l) => !l.startsWith('log:'));
    expect(order.slice(0, 5)).toEqual(['hook:voice', '1:sockets', '1:sign-out', '1:requests', '1:renderer']);
    expect(order.at(-1)).toMatch(/^2:open /);
    expect(s.vault.secrets.has(id)).toBe(false);
    expect(s.active().spec.token).toBeNull();
    expect(s.active().profileId).toBe(id);
  });

  it('removes the entry even when the storage is session-only, and clears the memory copy', async () => {
    const vault = fakeVault({ kind: 'sessionOnly', reason: 'unavailable' });
    const s = await setup({ vault });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok';
    await s.service.signedIn(ADA);
    await s.service.signOut();
    expect(vault.calls).toContain(`remove ${id}`);
    expect(s.active().spec.token).toBeNull();
  });

  it('says so when the server could not confirm the revocation, and still forgets locally', async () => {
    const s = await setup();
    await addAndSwitch(s, ORIGIN_A);
    s.active().signOut = () => {
      s.active().held = null;
      return Promise.resolve('unsent');
    };
    s.active().held = 'tok';
    await s.service.signedIn(ADA);
    expect(await s.service.signOut()).toBe('unsent');
    expect(s.service.view().notice).toBe('revocation-unsent');
    expect(s.active().spec.token).toBeNull();
  });
});

describe('sign-out cannot be undone by a vault that will not delete', () => {
  it('reopens without the credential when the entry could not be removed, and removes it when it can', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    expect(s.vault.secrets.get(id)).toBe('tok-ada');
    // Offline, and the entry cannot be deleted (EBUSY, EPERM).
    s.vault.removeFails = true;
    s.active().signOut = () => {
      s.active().held = null;
      return Promise.resolve('unsent');
    };
    await s.service.signOut();
    expect(s.vault.secrets.get(id)).toBe('tok-ada');
    expect(s.active().spec.token).toBeNull();
    // Switching away and back does not find it either, and each try removes again.
    await s.service.switchTo(null);
    s.vault.removeFails = false;
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    expect(s.vault.secrets.has(id)).toBe(false);
  });

  it('a later sign-in is kept again', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-1';
    await s.service.signedIn(ADA);
    s.vault.removeFails = true;
    await s.service.signOut();
    s.active().held = 'tok-2';
    await s.service.signedIn(ADA);
    expect(s.vault.secrets.get(id)).toBe('tok-2');
    s.vault.removeFails = false;
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-2');
  });
});

describe('a vault that hangs', () => {
  it('a delete that never answers does not hold sign-out, nor the reopened profile, and nothing is read from the vault', async () => {
    const s = await setup({ vaultBoundMs: 30 });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    s.vault.removeHangs = true;
    const getsBefore = s.vault.calls.filter((c) => c.startsWith('get')).length;
    expect(await s.service.signOut()).toBe('ended');
    expect(s.active().spec.token).toBeNull();
    expect(s.active().profileId).toBe(id);
    // The reopening consulted the vault for nothing.
    expect(s.vault.calls.filter((c) => c.startsWith('get')).length).toBe(getsBefore);
    // Switching away and back still does not find the credential.
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
  });

  it('a read that never answers is bounded: the profile starts signed out and storage says it did not answer', async () => {
    const s = await setup({ vaultBoundMs: 30 });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    await s.service.switchTo(null);
    // A delete of some other time is still pending in the vault's queue for this id.
    s.vault.removeHangs = true;
    void s.vault.remove(id);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().storage).toEqual({ kind: 'unavailable' });
  });

  it('an offline sign-out whose delete also fails still opens signed out, and a later delete that works ends it', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    s.vault.removeFails = true;
    s.active().signOut = () => {
      s.active().held = null;
      return Promise.resolve('unsent');
    };
    expect(await s.service.signOut()).toBe('unsent');
    expect(s.active().spec.token).toBeNull();
    s.vault.removeFails = false;
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    await new Promise((r) => setTimeout(r, 10));
    expect(s.vault.secrets.has(id)).toBe(false);
  });
});

describe('Retry never writes back what was deleted meanwhile', () => {
  async function twoHeld() {
    const s = await setup({ vault: fakeVault({ kind: 'sessionOnly', reason: 'insecure-backend' }) });
    const one = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-1';
    await s.service.signedIn(ADA);
    const two = await addAndSwitch(s, ORIGIN_B);
    s.active().held = 'tok-2';
    await s.service.signedIn(ADA);
    return { s, one, two };
  }

  it('overlapping a sign-out', async () => {
    const { s, one, two } = await twoHeld();
    // two is the active profile. Storage turns protected; the first write waits.
    s.vault.classification = { kind: 'protected' };
    let open: () => void = () => undefined;
    s.vault.putGate = new Promise<void>((r) => {
      open = r;
    });
    const retrying = s.service.retry();
    await new Promise((r) => setTimeout(r, 10));
    const out = s.service.signOut();
    await new Promise((r) => setTimeout(r, 10));
    open();
    await Promise.all([retrying, out]);
    s.vault.putGate = null;
    await new Promise((r) => setTimeout(r, 10));
    expect(s.vault.secrets.get(one)).toBe('tok-1');
    expect(s.vault.secrets.has(two)).toBe(false);
  });

  it('overlapping a removal', async () => {
    const { s, one, two } = await twoHeld();
    await s.service.switchTo(one);
    s.vault.classification = { kind: 'protected' };
    let open: () => void = () => undefined;
    s.vault.putGate = new Promise<void>((r) => {
      open = r;
    });
    const retrying = s.service.retry();
    await new Promise((r) => setTimeout(r, 10));
    const removing = s.service.remove(two);
    await new Promise((r) => setTimeout(r, 10));
    open();
    await Promise.all([retrying, removing]);
    s.vault.putGate = null;
    await new Promise((r) => setTimeout(r, 10));
    expect(s.vault.secrets.has(two)).toBe(false);
    expect(s.vault.secrets.get(one)).toBe('tok-1');
  });
});

describe('a stored credential is checked before a window holds it', () => {
  async function stored(verify: (o: string, t: string) => StoredCheck) {
    const s = await setup({ verify });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    await s.service.switchTo(null);
    return { s, id };
  }
  const account = (id: number, over: { lasting?: boolean; guest?: boolean } = {}): StoredCheck => ({ kind: 'account', account: { id, name: 'x', lasting: true, guest: false, ...over } });

  it('adopts it when the server says it is the account the profile is bound to', async () => {
    const { s, id } = await stored(() => account(7));
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-ada');
  });

  it.each([
    ['another account', () => account(8)],
    ['an operator’s', () => account(7, { lasting: false })],
    ['a guest’s', () => account(7, { lasting: false, guest: true })],
  ] as const)('refuses %s: not adopted, revoked, deleted, and said why', async (_name, verify) => {
    const { s, id } = await stored(verify);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    expect(s.revoked).toEqual([[ORIGIN_A, 'tok-ada']]);
    expect(s.service.view().notice).toBe('account-mismatch');
    await new Promise((r) => setTimeout(r, 10));
    expect(s.vault.secrets.has(id)).toBe(false);
    expect(s.store.get(id)?.principal?.name).toBe('ada');
  });

  it('does not adopt a credential the server no longer accepts, and forgets it', async () => {
    const { s, id } = await stored(() => ({ kind: 'rejected' }));
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().notice).toBe('sign-in-again');
    await new Promise((r) => setTimeout(r, 10));
    expect(s.vault.secrets.has(id)).toBe(false);
  });

  it('does not trust a server that cannot be asked with it, and keeps the entry', async () => {
    const { s, id } = await stored(() => ({ kind: 'unreachable' }));
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().notice).toBe('could-not-verify');
    expect(s.vault.secrets.get(id)).toBe('tok-ada');
    expect(s.revoked).toEqual([]);
  });
});

describe('a sign-out or removal acts on the activation it was asked of', () => {
  async function two() {
    const s = await setup();
    const a = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-A';
    await s.service.signedIn(ADA);
    const b = await addAndSwitch(s, ORIGIN_B);
    s.active().held = 'tok-B';
    await s.service.signedIn(ADA);
    await s.service.switchTo(a);
    return { s, a, b };
  }

  it('sign-out clicked in A while a switch to B is queued behind a slow hook revokes A, not B', async () => {
    const { s, a, b } = await two();
    let open: () => void = () => undefined;
    const gate = new Promise<void>((r) => {
      open = r;
    });
    s.hooks.media('slow-share', () => gate);
    const switching = s.service.switchTo(b);
    await new Promise((r) => setTimeout(r, 10));
    const out = s.service.signOut();
    await new Promise((r) => setTimeout(r, 10));
    open();
    await Promise.all([switching, out]);
    const inB = s.factory.made.find((m) => m.profileId === b);
    expect(inB?.signedOut).toBe(false);
    expect(s.revoked).toEqual([[ORIGIN_A, 'tok-A']]);
    await new Promise((r) => setTimeout(r, 10));
    expect(s.vault.secrets.has(a)).toBe(false);
    expect(s.vault.secrets.get(b)).toBe('tok-B');
  });

  it('removing A while a switch to B is queued revokes A’s credential, not B’s', async () => {
    const { s, a, b } = await two();
    let open: () => void = () => undefined;
    const gate = new Promise<void>((r) => {
      open = r;
    });
    s.hooks.media('slow-share', () => gate);
    const switching = s.service.switchTo(b);
    await new Promise((r) => setTimeout(r, 10));
    const removing = s.service.remove(a);
    await new Promise((r) => setTimeout(r, 10));
    open();
    await Promise.all([switching, removing]);
    const inB = s.factory.made.find((m) => m.profileId === b);
    expect(inB?.signedOut).toBe(false);
    expect(s.revoked).toEqual([[ORIGIN_A, 'tok-A']]);
    expect(s.vault.secrets.has(a)).toBe(false);
  });
});

describe('a pasted credential whose account cannot be checked is not left in force', () => {
  it('forgets it when whoami fails, so it never stays live unbound', async () => {
    const s = await setup();
    await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-pasted';
    s.active().account = null;
    expect(await s.service.keptCredential()).toBe(false);
    expect(s.active().held).toBeNull();
    expect(s.active().signedOut).toBe(true);
  });
});

describe('removing a profile', () => {
  it('tells the server the active profile’s credential is over, before deleting it', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    const old = s.active();
    expect(await s.service.remove(id)).toBe(true);
    expect(old.signedOut).toBe(true);
    expect(s.vault.secrets.has(id)).toBe(false);
  });

  it('revokes an inactive profile’s stored credential too, bounded, before deleting it', async () => {
    const s = await setup();
    const one = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-1';
    await s.service.signedIn(ADA);
    await addAndSwitch(s, ORIGIN_B);
    expect(await s.service.remove(one)).toBe(true);
    expect(s.revoked).toEqual([[ORIGIN_A, 'tok-1']]);
    expect(s.vault.secrets.has(one)).toBe(false);
  });

  it('revokes the in-memory credential of an inactive session-only profile', async () => {
    const s = await setup({ vault: fakeVault({ kind: 'sessionOnly', reason: 'insecure-backend' }) });
    const one = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-1';
    await s.service.signedIn(ADA);
    await addAndSwitch(s, ORIGIN_B);
    await s.service.remove(one);
    expect(s.revoked).toEqual([[ORIGIN_A, 'tok-1']]);
  });

  it('ends it if active, deletes the vault entry and the profile directory, and no other profile’s', async () => {
    const s = await setup();
    const one = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-1';
    await s.service.signedIn(ADA);
    const two = await addAndSwitch(s, ORIGIN_B);
    s.active().held = 'tok-2';
    await s.service.signedIn(ADA);
    await s.service.switchTo(one);
    expect(await s.service.remove(one)).toBe(true);
    expect(s.vault.secrets.has(one)).toBe(false);
    expect(s.vault.secrets.get(two)).toBe('tok-2');
    expect(s.files.dirs.has(one)).toBe(false);
    expect(s.files.dirs.has(two)).toBe(true);
    expect(s.service.view().active).toBeNull();
    expect(s.store.list().map((p) => p.id)).toEqual([two]);
  });

  it('keeps the profile, so it can be tried again, when the vault entry cannot be deleted', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.vault.removeFails = true;
    expect(await s.service.remove(id)).toBe(false);
    expect(s.store.get(id)).toBeDefined();
    s.vault.removeFails = false;
    expect(await s.service.remove(id)).toBe(true);
  });
});

describe('start', () => {
  it('opens the profile that was active last time', async () => {
    const first = await setup();
    const id = await addAndSwitch(first, ORIGIN_A);
    const second = await setup({ files: first.files.text ?? '' });
    expect(second.active().profileId).toBe(id);
  });

  it('shows the recovery screen for a corrupt profiles.json and changes nothing', async () => {
    const s = await setup({ files: '{ nope' });
    expect(s.service.view().recovery).toBe(true);
    expect(s.active().profileId).toBeNull();
    expect(s.files.writes).toBe(0);
    expect(await s.service.add(ORIGIN_A)).toMatchObject({ ok: false });
    expect(await s.service.switchTo(null)).toBe(false);
    expect((await s.service.recover()).recovery).toBe(false);
  });
});

describe('server compatibility (issue #946)', () => {
  // A profile signed in once on a supported server, then left; the server's
  // answer to /healthz is whatever `server.health` says when it is switched to.
  async function signedInProfile(options: { vault?: FakeVault; servers?: string[]; override?: boolean; healthBoundMs?: number } = {}) {
    const server: { health: () => Promise<HealthResult> } = { health: () => Promise.resolve({ kind: 'answered', version: 'v1.0.0' }) };
    const s = await setup({ ...options, health: () => server.health() });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    await s.service.switchTo(null);
    return { s, id, server };
  }
  const answers = (version: string | null): (() => Promise<HealthResult>) => () => Promise.resolve({ kind: 'answered', version });

  it('proceeds on a listed version: the credential is read, checked, and adopted', async () => {
    const { s, id } = await signedInProfile();
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-ada');
    expect(s.service.view().compat).toEqual({ state: 'supported', server: 'v1.0.0', desktop: '0.0.0', override: false, invalid: null });
    expect(s.asked.at(-1)).toBe(ORIGIN_A);
  });

  it.each([
    ['an unlisted version', answers('v2.0.0'), 'unsupported', 'v2.0.0'],
    ['a development version', answers('v0.0.0-dev'), 'unsupported', 'v0.0.0-dev'],
    ['no version', answers(null), 'unsupported', null],
    ['a malformed version', answers('<b>x</b>'), 'unsupported', null],
    ['no answer', () => Promise.resolve<HealthResult>({ kind: 'unreachable' }), 'unreachable', null],
  ] as const)('refuses automatic sign-in on %s, and asks the vault and the server about the credential nothing', async (_name, health, state, server) => {
    const { s, id, server: fake } = await signedInProfile();
    fake.health = health;
    const gets = s.vault.calls.filter((c) => c.startsWith('get')).length;
    const checks = s.verified.length;
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    expect(s.vault.calls.filter((c) => c.startsWith('get')).length).toBe(gets);
    expect(s.verified.length).toBe(checks);
    expect(s.service.view().compat).toEqual({ state, server, desktop: '0.0.0', override: false, invalid: null });
    expect(s.service.signInPermitted()).toBe(false);
    // Nothing is forgotten or revoked: the server may be tested later.
    expect(s.vault.secrets.get(id)).toBe('tok-ada');
    expect(s.revoked).toEqual([]);
    expect(s.service.view().notice).toBeNull();
  });

  it('does not hand a credential held in memory to an unsupported server either', async () => {
    const vault = fakeVault({ kind: 'sessionOnly', reason: 'insecure-backend' });
    const { s, id, server } = await signedInProfile({ vault });
    server.health = answers('v2.0.0');
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    // The server is listed again: the same run's credential is there, and is
    // checked as ever.
    server.health = answers('v1.0.0');
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-ada');
  });

  it('keeps the signed-out set: a signed-out profile starts signed out on a supported server and is not read on another', async () => {
    const { s, id, server } = await signedInProfile();
    await s.service.switchTo(id);
    s.vault.removeFails = true;
    await s.service.signOut();
    await s.service.switchTo(null);
    server.health = answers('v1.0.0');
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    server.health = answers('v2.0.0');
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
  });

  it('a server that does not answer in time is unreachable, and the switch goes on', async () => {
    const { s, id, server } = await signedInProfile({ healthBoundMs: 20 });
    server.health = () => new Promise<HealthResult>(() => undefined);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().compat.state).toBe('unreachable');
  });

  it('a version check that throws is unreachable', async () => {
    const { s, id, server } = await signedInProfile();
    server.health = () => Promise.reject(new Error('boom'));
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().compat.state).toBe('unreachable');
  });

  it('says nothing is checked on the server list, and a new switch forgets the old verdict', async () => {
    const { s, id, server } = await signedInProfile();
    server.health = answers('v2.0.0');
    await s.service.switchTo(id);
    expect(s.service.view().compat.state).toBe('unsupported');
    await s.service.switchTo(null);
    expect(s.service.view().compat).toEqual({ state: 'none', server: null, desktop: '0.0.0', override: false, invalid: null });
    expect(s.service.signInPermitted()).toBe(false);
  });

  it('the development override lets an unlisted server sign in, still checks the credential, and says so', async () => {
    const { s, id, server } = await signedInProfile({ override: true });
    server.health = answers('v2.0.0');
    const checks = s.verified.length;
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-ada');
    expect(s.verified.length).toBe(checks + 1);
    expect(s.service.view().compat).toEqual({ state: 'unlisted', server: 'v2.0.0', desktop: '0.0.0', override: true, invalid: null });
    expect(s.service.signInPermitted()).toBe(true);
    await s.service.switchTo(null);
    expect(s.service.view().compat.override).toBe(true);
  });

  it('the override does not reach a server that did not answer', async () => {
    const { s, id, server } = await signedInProfile({ override: true });
    server.health = () => Promise.resolve<HealthResult>({ kind: 'unreachable' });
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.signInPermitted()).toBe(false);
  });

  it('lets a sign-in through only for a supported server', async () => {
    const { s, id, server } = await signedInProfile();
    await s.service.switchTo(id);
    expect(s.service.signInPermitted()).toBe(true);
    server.health = answers('v2.0.0');
    await s.service.switchTo(id);
    expect(s.service.signInPermitted()).toBe(false);
  });

  it('shows the first answer a required family sent that was not what this app understands, as an incompatibility', async () => {
    const { s, id } = await signedInProfile();
    await s.service.switchTo(id);
    const { onInvalid } = s.active().spec;
    onInvalid('health');
    expect(s.service.view().compat.invalid).toBeNull();
    onInvalid('listChannels');
    onInvalid('whoami');
    expect(s.service.view().compat.invalid).toBe('channels');
    // It is a report, not a sign-out.
    expect(s.active().signedOut).toBe(false);
    await s.service.switchTo(null);
    expect(s.service.view().compat.invalid).toBeNull();
  });

  it('ignores a report from an activation that has been left', async () => {
    const { s, id } = await signedInProfile();
    await s.service.switchTo(id);
    const { onInvalid } = s.active().spec;
    await s.service.switchTo(null);
    onInvalid('whoami');
    expect(s.service.view().compat.invalid).toBeNull();
  });
});
