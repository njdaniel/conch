import { describe, expect, it } from 'vitest';

import { TeardownHooks } from '../../main/profiles/hooks.ts';
import type { HealthResult } from '../../main/compat.ts';
import { keepHelpFor, ProfilesService, type ProbeResult, type StoredCheck } from '../../main/profiles/service.ts';
import { ProfileStore } from '../../main/profiles/store.ts';
import type { GetResult } from '../../main/vault/vault.ts';
import { fakeFactory, fakeVault, memoryFiles, ORIGIN_A, ORIGIN_B, type FakeActivation, type FakeVault } from './profiles-support.ts';

async function setup(options: { vault?: FakeVault; probe?: ProbeResult; stepBoundMs?: number; files?: string; vaultBoundMs?: number; verify?: (origin: string, token: string) => StoredCheck; servers?: string[]; override?: boolean; health?: (origin: string) => Promise<HealthResult>; sweepHealth?: (origin: string) => Promise<HealthResult>; healthBoundMs?: number; revoke?: (origin: string, token: string) => Promise<'ended' | 'unconfirmed' | 'unsent'>; revokeBoundMs?: number; background?: boolean } = {}) {
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
      return (options.health ?? (() => Promise.resolve<HealthResult>({ kind: 'answered', version: 'v1.0.0' })))(origin);
    },
    // The sweep's strict question (#2217): by default what health says.
    readSweepHealth: (origin) => {
      asked.push(origin);
      return (options.sweepHealth ?? options.health ?? (() => Promise.resolve<HealthResult>({ kind: 'answered', version: 'v1.0.0' })))(origin);
    },
    verifyStored: (origin, token) => {
      verified.push([origin, token]);
      if (options.verify !== undefined) return Promise.resolve(options.verify(origin, token));
      const bound = store.list().find((p) => p.origin === origin && p.principal !== null)?.principal;
      return Promise.resolve(bound === undefined || bound === null ? { kind: 'rejected' } : { kind: 'account', account: { id: bound.id, name: bound.name, lasting: true, guest: false } });
    },
    revokeStored: (origin, token) => {
      revoked.push([origin, token]);
      return options.revoke === undefined ? Promise.resolve('ended') : options.revoke(origin, token);
    },
    ...(options.stepBoundMs === undefined ? {} : { stepBoundMs: options.stepBoundMs }),
    ...(options.vaultBoundMs === undefined ? {} : { vaultBoundMs: options.vaultBoundMs }),
    ...(options.healthBoundMs === undefined ? {} : { healthBoundMs: options.healthBoundMs }),
    ...(options.revokeBoundMs === undefined ? {} : { revokeBoundMs: options.revokeBoundMs }),
  });
  await service.start();
  // The sweep of #2217 runs in the background; most tests start after it.
  if (options.background !== true) await service.swept;
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

describe('finding profiles by origin (issue #1591)', () => {
  it('lists every profile for a canonical origin, with its account and whether it is active, and none for another', async () => {
    const s = await setup();
    const first = await addAndSwitch(s, ORIGIN_A);
    expect(await s.service.signedIn(ADA)).toBe(true);
    const second = (await s.service.add(ORIGIN_A)) as { ok: true; id: string };
    const other = (await s.service.add(ORIGIN_B)) as { ok: true; id: string };
    expect(s.service.findByOrigin(ORIGIN_A).map(({ id, account, active }) => ({ id, account, active }))).toEqual([
      { id: first, account: 'ada', active: true },
      { id: second.id, account: null, active: false },
    ]);
    expect(s.service.findByOrigin(ORIGIN_B).map((p) => p.id)).toEqual([other.id]);
    expect(s.service.findByOrigin('https://c.example')).toEqual([]);
    // Only the canonical form matches: the caller canonicalises first.
    expect(s.service.findByOrigin('https://A.example')).toEqual([]);
    // The same entries as the view's.
    expect(s.service.findByOrigin(ORIGIN_A)).toEqual(s.service.view().profiles.filter((p) => p.origin === ORIGIN_A));
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
  it.each([
    ['operator', false, 'operator-window-only'],
    ['member', true, null],
    ['guest', false, null],
  ] as const)('shows the sign-in storage note only for an operator: %s', async (role, lasting, notice) => {
    const s = await setup();
    await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-account';
    expect(await s.service.signedIn({ id: 7, name: role, lasting, guest: role === 'guest' })).toBe(true);
    expect(s.service.view().notice).toBe(notice);
    // Polling the view keeps the same notice, rather than generating another.
    expect(s.service.view().notice).toBe(notice);
    await s.service.signOut();
    expect(s.service.view().notice).toBeNull();
  });

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
    // The server's refusal is said as what it is (#2077): the saved sign-in ended.
    expect(s.service.view().notice).toBe('sign-in-ended');
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

// "Keep me signed in on this computer" (issue #2077), and what a restart finds.
describe('keeping a sign-in across restarts', () => {
  // A second service over the same profiles.json and vault: the app started again.
  async function restart(s: Awaited<ReturnType<typeof setup>>, options: { verify?: (o: string, t: string) => StoredCheck } = {}) {
    return setup({ vault: s.vault, files: s.files.text ?? undefined, ...options });
  }
  const noToken = (lines: readonly string[], token: string): void => {
    for (const line of lines) expect(line).not.toContain(token);
  };

  it('is on by default: a member’s sign-in goes to the vault, and a restart opens signed in without a password', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    expect(s.service.view().keep).toBe(true);
    // The sign-in form links the Linux guide's section; main names it.
    expect(s.service.view().keepHelp).toBe('https://github.com/njdaniel/conch/blob/main/docs/desktop/linux.md#staying-signed-in');
    s.active().held = 'tok-ada';
    expect(await s.service.signedIn(ADA)).toBe(true);
    expect(s.vault.secrets.get(id)).toBe('tok-ada');
    expect(s.log).toContain('log: conch-desktop: a sign-in was kept in the keyring');

    const again = await restart(s);
    expect(again.active().spec.profile?.id).toBe(id);
    expect(again.active().spec.token).toBe('tok-ada');
    expect(again.verified).toEqual([[ORIGIN_A, 'tok-ada']]);
    expect(again.log).toContain('log: conch-desktop: a saved sign-in was restored');
    noToken([...s.log, ...again.log], 'tok-ada');
  });

  it('unchecked: nothing is written, the sign-in lasts this run only, an older entry is deleted, and a restart asks again', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.vault.secrets.set(id, 'tok-older');
    expect((await s.service.setKeep(false)).keep).toBe(false);
    s.active().held = 'tok-ada';
    expect(await s.service.signedIn(ADA)).toBe(true);
    await new Promise((r) => setTimeout(r, 10));
    expect(s.vault.secrets.has(id)).toBe(false);
    expect(s.vault.calls).not.toContain(`put ${id}`);
    // This run keeps it in memory: a switch away and back is still signed in.
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-ada');
    // "Don't keep" holds for the profile for this run: its later window
    // shows it, and a sign-out ends it.
    expect(s.service.view().keep).toBe(false);

    const again = await restart(s);
    expect(again.active().spec.token).toBeNull();
    expect(again.log).toContain('log: conch-desktop: no saved sign-in for this server');
  });

  it('names help only where the platform’s guide covers staying signed in', () => {
    expect(keepHelpFor('linux')).toMatch(/^https:\/\/github\.com\/njdaniel\/conch\/blob\/main\/docs\/desktop\/linux\.md#staying-signed-in$/);
    expect(keepHelpFor('win32')).toBeNull();
    expect(keepHelpFor('darwin')).toBeNull();
  });

  it('"don\u2019t keep" survives a switch away and back: a password change there (a rotated credential) is not put either', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    await s.service.setKeep(false);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-ada');
    // The new window shows no box; its password change rotates the credential
    // and comes back through signedIn, as replaceCredential does.
    s.active().held = 'tok-ada-rotated';
    expect(await s.service.signedIn(ADA)).toBe(true);
    await new Promise((r) => setTimeout(r, 10));
    expect(s.vault.calls).not.toContain(`put ${id}`);
    expect(s.vault.secrets.has(id)).toBe(false);
    expect(s.service.view().keep).toBe(false);
    // A sign-out ends the choice: the next sign-in form starts kept again.
    await s.service.signOut();
    expect(s.service.view().keep).toBe(true);
  });

  it('turned off while the keep\u2019s write is on its way, what it wrote comes out and the sign-in lasts this run', async () => {
    const vault = fakeVault();
    let open: () => void = () => undefined;
    vault.putGate = new Promise<void>((resolve) => {
      open = resolve;
    });
    const s = await setup({ vault });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    const signing = s.service.signedIn(ADA);
    await new Promise((r) => setTimeout(r, 5));
    const turningOff = s.service.setKeep(false);
    open();
    await signing;
    await turningOff;
    await new Promise((r) => setTimeout(r, 10));
    expect(vault.secrets.has(id)).toBe(false);
    expect(s.service.view().keep).toBe(false);
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBe('tok-ada');
    expect((await restart(s)).active().spec.token).toBeNull();
  });

  it('turned off again while turning it back on is writing, nothing stays in the vault', async () => {
    const vault = fakeVault();
    const s = await setup({ vault });
    const id = await addAndSwitch(s, ORIGIN_A);
    await s.service.setKeep(false);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    await new Promise((r) => setTimeout(r, 10));
    let open: () => void = () => undefined;
    vault.putGate = new Promise<void>((resolve) => {
      open = resolve;
    });
    const on = s.service.setKeep(true);
    await new Promise((r) => setTimeout(r, 5));
    const off = s.service.setKeep(false);
    open();
    await on;
    await off;
    await new Promise((r) => setTimeout(r, 10));
    expect(vault.secrets.has(id)).toBe(false);
    expect(s.service.view().keep).toBe(false);
    // And Retry does not put it later.
    await s.service.retry();
    expect(vault.secrets.has(id)).toBe(false);
  });

  // A write the bound reported failed may still land (review of 59ce6835):
  // with the choice off, it never stays in the vault.
  function lateVault() {
    const vault = fakeVault();
    let land: () => void = () => undefined;
    vault.putGate = new Promise<void>((resolve) => {
      land = resolve;
    });
    return {
      vault,
      land: () => {
        land();
      },
    };
  }
  const later = (ms: number) => new Promise((r) => setTimeout(r, ms));

  it('on, then off while the write is pending past the bound: the write that lands late is deleted', async () => {
    const { vault, land } = lateVault();
    const s = await setup({ vault, vaultBoundMs: 20 });
    const id = await addAndSwitch(s, ORIGIN_A);
    await s.service.setKeep(false);
    vault.putGate = null;
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    const late = lateVault();
    vault.putGate = late.vault.putGate;
    const on = s.service.setKeep(true);
    await later(5);
    const off = s.service.setKeep(false);
    await on;
    // The bound has passed: the write was reported failed, and is still on its way.
    late.land();
    land();
    await off;
    await later(10);
    expect(vault.secrets.has(id)).toBe(false);
    expect(s.service.view().keep).toBe(false);
  });

  it('a sign-in whose write lands after the bound, then off: the late write is deleted', async () => {
    const { vault, land } = lateVault();
    const s = await setup({ vault, vaultBoundMs: 20 });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    expect(s.service.view().notice).toBe('not-kept');
    const off = s.service.setKeep(false);
    land();
    await off;
    await later(10);
    expect(vault.secrets.has(id)).toBe(false);
    expect((await restart(s)).active().spec.token).toBeNull();
  });

  it('a sign-out while turning it back on is writing leaves no stale "don\u2019t keep": the next form starts kept', async () => {
    const { vault, land } = lateVault();
    vault.putGate = null;
    const s = await setup({ vault });
    const id = await addAndSwitch(s, ORIGIN_A);
    await s.service.setKeep(false);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    const gate = lateVault();
    vault.putGate = gate.vault.putGate;
    const on = s.service.setKeep(true);
    await later(5);
    await s.service.signOut();
    gate.land();
    land();
    await on;
    await later(10);
    expect(s.service.view().keep).toBe(true);
    expect(vault.secrets.has(id)).toBe(false);
  });

  it('keeps only the credential the sign-in put in force: one that replaced it while the binding was written is not kept by this call', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-first';
    const first = s.service.signedIn(ADA);
    // Another credential is put in force before the first call's binding returns.
    s.active().held = 'tok-second';
    expect(await first).toBe(true);
    expect(s.vault.secrets.has(id)).toBe(false);
    // The later sign-in's own call keeps its own credential.
    await s.service.signedIn(ADA);
    expect(s.vault.secrets.get(id)).toBe('tok-second');
  });

  it('a pasted credential is kept only if it is still the one whoami was asked about', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    const active = s.active();
    active.held = 'tok-pasted';
    let answer: (a: typeof ADA) => void = () => undefined;
    active.whoami = () =>
      new Promise((resolve) => {
        answer = resolve;
      });
    const kept = s.service.keptCredential();
    active.held = 'tok-other';
    answer(ADA);
    expect(await kept).toBe(true);
    expect(s.vault.secrets.has(id)).toBe(false);
  });

  it('Retry never puts a sign-in the person chose not to keep', async () => {
    const vault = fakeVault({ kind: 'sessionOnly', reason: 'no-keyring' });
    const s = await setup({ vault });
    const id = await addAndSwitch(s, ORIGIN_A);
    await s.service.setKeep(false);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    vault.classification = { kind: 'protected' };
    expect((await s.service.retry()).storage).toEqual({ kind: 'protected' });
    expect(vault.secrets.has(id)).toBe(false);
  });

  it('turned back on before the sign-in, it keeps; turned on after an unkept sign-in, it puts it then', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    await s.service.setKeep(false);
    expect((await s.service.setKeep(true)).keep).toBe(true);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    expect(s.vault.secrets.get(id)).toBe('tok-ada');

    const t = await setup();
    const other = await addAndSwitch(t, ORIGIN_B);
    await t.service.setKeep(false);
    t.active().held = 'tok-grace';
    await t.service.signedIn(GRACE);
    await new Promise((r) => setTimeout(r, 10));
    expect(t.vault.secrets.has(other)).toBe(false);
    await t.service.setKeep(true);
    expect(t.vault.secrets.get(other)).toBe('tok-grace');
  });

  it('turned off after the sign-in was kept (a racing click), the entry comes out and nothing is copied to memory', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    expect(s.vault.secrets.get(id)).toBe('tok-ada');
    await s.service.setKeep(false);
    expect(s.vault.secrets.has(id)).toBe(false);
    // The window keeps its own sign-in; the profile starts signed out next time.
    expect(s.active().token()).toBe('tok-ada');
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
  });

  it('turned off in an operator’s window, the operator’s credential is not copied anywhere and does not outlive the window (ADR-012)', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-op';
    await s.service.signedIn({ id: 1, name: 'op', lasting: false, guest: false });
    await s.service.setKeep(false);
    await s.service.setKeep(true);
    expect(s.vault.secrets.size).toBe(0);
    expect(s.vault.calls).not.toContain(`put ${id}`);
    await s.service.switchTo(null);
    await s.service.switchTo(id);
    expect(s.active().spec.token).toBeNull();
  });

  it('turned off while a session-only sign-in is held, Retry never puts it later', async () => {
    const vault = fakeVault({ kind: 'sessionOnly', reason: 'no-keyring' });
    const s = await setup({ vault });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    await s.service.setKeep(false);
    vault.classification = { kind: 'protected' };
    await s.service.retry();
    expect(vault.secrets.has(id)).toBe(false);
  });

  it.each([
    ['no keyring unlocked', { kind: 'sessionOnly', reason: 'no-keyring' }, { kind: 'session-only', reason: 'no-keyring' }, 'no-keyring'],
    ['a keyring that protects nothing', { kind: 'sessionOnly', reason: 'insecure-backend' }, { kind: 'session-only', reason: 'insecure-backend' }, 'insecure-backend'],
    ['a keyring that does not answer', { kind: 'temporarilyUnavailable' }, { kind: 'unavailable' }, 'temporarilyUnavailable'],
  ] as const)('%s: nothing is written even with the choice on, the view and the log say why', async (_name, classification, storage, why) => {
    const vault = fakeVault(classification);
    const s = await setup({ vault });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    expect(vault.secrets.has(id)).toBe(false);
    expect(s.service.view().storage).toEqual(storage);
    expect(s.log).toContain(`log: conch-desktop: a sign-in is kept until the app closes (${why})`);
    expect((await restart(s)).active().spec.token).toBeNull();
  });

  it('an operator’s sign-in is never put, whatever the choice, and a restart asks again (ADR-012)', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    await s.service.setKeep(true);
    s.active().held = 'tok-op';
    expect(await s.service.signedIn({ id: 1, name: 'op', lasting: false, guest: false })).toBe(true);
    expect(s.vault.calls).not.toContain(`put ${id}`);
    expect(s.vault.secrets.size).toBe(0);
    expect(s.service.view().notice).toBe('operator-window-only');
    expect(s.log).toContain('log: conch-desktop: an operator sign-in is kept for this window only');
    const again = await restart(s);
    expect(again.active().spec.token).toBeNull();
    expect(again.log).toContain('log: conch-desktop: no saved sign-in for this server');
  });

  it('a keep the keyring did not take in time says so, and the sign-in lasts this run', async () => {
    const vault = fakeVault();
    vault.putGate = new Promise(() => undefined);
    const s = await setup({ vault, vaultBoundMs: 20 });
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    expect(s.service.view().notice).toBe('not-kept');
    expect(vault.secrets.has(id)).toBe(false);
    expect(s.log).toContain('log: conch-desktop: a sign-in is kept until the app closes (failed)');
  });

  it('a saved sign-in the server ended (revoked, expired, signed out elsewhere) opens the sign-in with a plain notice, and is deleted', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    const again = await restart(s, { verify: () => ({ kind: 'rejected' }) });
    expect(again.active().spec.token).toBeNull();
    expect(again.service.view().notice).toBe('sign-in-ended');
    await new Promise((r) => setTimeout(r, 10));
    expect(again.vault.secrets.has(id)).toBe(false);
    expect(again.log).toContain('log: conch-desktop: the server ended a saved sign-in; it was deleted');
    // And the next start does not find it either.
    expect((await restart(again)).active().spec.token).toBeNull();
  });

  it('sign-out deletes the kept sign-in, and a restart asks again', async () => {
    const s = await setup();
    const id = await addAndSwitch(s, ORIGIN_A);
    s.active().held = 'tok-ada';
    await s.service.signedIn(ADA);
    await s.service.signOut();
    await new Promise((r) => setTimeout(r, 10));
    expect(s.vault.secrets.has(id)).toBe(false);
    const again = await restart(s);
    expect(again.active().spec.token).toBeNull();
    expect(again.log).toContain('log: conch-desktop: no saved sign-in for this server');
  });
});

// Issue #2217: at start, whatever any server's health, every vault entry an
// older build sealed with Chromium's built-in key (v10) is ended: revoked at
// its server when the server answers, else deleted unopened with a notice.
describe('the start sweep of sign-ins an older build saved with the built-in key', () => {
  const A = 'a'.repeat(32);
  const B = 'b'.repeat(32);
  const ORPHAN = 'c'.repeat(32);
  const LEGACY = 'tok-legacy-SECRET-canary-2217';
  const answered = (): Promise<HealthResult> => Promise.resolve({ kind: 'answered', version: 'v1.0.0' });
  const unreachable = (): Promise<HealthResult> => Promise.resolve({ kind: 'unreachable' });

  // profiles.json with A on ORIGIN_A and B on ORIGIN_B, both bound to ada.
  function profilesJson(active: string | null): string {
    return JSON.stringify({
      version: 1,
      profiles: [
        { id: A, label: 'a.example', origin: ORIGIN_A, principal: { id: 7, name: 'ada' } },
        { id: B, label: 'b.example', origin: ORIGIN_B, principal: { id: 7, name: 'ada' } },
      ],
      active,
    });
  }

  function legacyVault(entries: Record<string, string>): FakeVault {
    const vault = fakeVault();
    for (const [id, secret] of Object.entries(entries)) vault.legacy.set(id, secret);
    return vault;
  }

  const sweepLines = (log: readonly string[]): string[] => log.filter((l) => l.includes('older build (v10)'));
  const LINE = /^log: conch-desktop: a sign-in saved by an older build \(v10\): (ended|ended-not-deleted|deleted-unreachable|deleted-unconfirmed|deleted-unopened|deleted-no-profile|not-deleted|replaced|late|not-listed|skipped)$/;

  it('when the server answers: the entry is opened only to be revoked as sign-out does, then deleted, and it is never a sign-in', async () => {
    const vault = legacyVault({ [A]: LEGACY });
    const s = await setup({ vault, files: profilesJson(A) });
    expect(s.revoked).toEqual([[ORIGIN_A, LEGACY]]);
    expect(vault.legacy.has(A)).toBe(false);
    expect(vault.calls).toContain(`decrypt ${A}`);
    // Not handed to the window, not checked as a sign-in, not read as one.
    expect(s.active().spec.token).toBeNull();
    expect(s.verified).toEqual([]);
    expect(vault.calls).not.toContain(`get ${A}`);
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): ended']);
    // Ended at the server: nothing to tell the person beyond signing in.
    expect(s.service.view().notice).toBeNull();
  });

  it('when the server does not answer: deleted without being opened, and the profile says it could not be ended at the server', async () => {
    const vault = legacyVault({ [A]: LEGACY });
    const s = await setup({ vault, files: profilesJson(A), health: unreachable });
    expect(vault.calls).not.toContain(`decrypt ${A}`);
    expect(s.revoked).toEqual([]);
    expect(vault.legacy.has(A)).toBe(false);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): deleted-unreachable']);
  });

  it('a /healthz that never answers is the same, within the health bound', async () => {
    const vault = legacyVault({ [A]: LEGACY });
    const s = await setup({ vault, files: profilesJson(A), health: () => new Promise<HealthResult>(() => undefined), healthBoundMs: 20 });
    expect(vault.calls).not.toContain(`decrypt ${A}`);
    expect(s.revoked).toEqual([]);
    expect(vault.legacy.has(A)).toBe(false);
    expect(s.service.view().notice).toBe('legacy-not-ended');
  });

  it('runs for every profile, active or not, and the notice is that profile’s', async () => {
    const vault = legacyVault({ [A]: 'tok-a', [B]: 'tok-b' });
    const s = await setup({ vault, files: profilesJson(A), health: (origin) => (origin === ORIGIN_A ? answered() : unreachable()) });
    expect(s.revoked).toEqual([[ORIGIN_A, 'tok-a']]);
    expect(vault.calls).not.toContain(`decrypt ${B}`);
    expect(vault.legacy.size).toBe(0);
    // A was ended at its server: no notice there.
    expect(s.service.view().notice).toBeNull();
    expect(await s.service.switchTo(B)).toBe(true);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(await s.service.switchTo(A)).toBe(true);
    expect(s.service.view().notice).toBeNull();
    expect(sweepLines(s.log).sort()).toEqual([
      'log: conch-desktop: a sign-in saved by an older build (v10): deleted-unreachable',
      'log: conch-desktop: a sign-in saved by an older build (v10): ended',
    ]);
  });

  it('runs whatever the active server’s compatibility verdict, and with no profile active', async () => {
    // The release is not on the list: no automatic sign-in, but the sweep runs.
    const vault = legacyVault({ [A]: LEGACY, [B]: 'tok-b' });
    const s = await setup({ vault, files: profilesJson(null), servers: ['v9.9.9'] });
    expect(s.revoked.sort()).toEqual([[ORIGIN_A, LEGACY], [ORIGIN_B, 'tok-b']]);
    expect(vault.legacy.size).toBe(0);
  });

  it('a revocation the server does not confirm, or an entry that cannot be opened, is deleted with the notice', async () => {
    const vault = legacyVault({ [A]: LEGACY, [B]: 'tok-b' });
    vault.unopenable.add(B);
    const s = await setup({ vault, files: profilesJson(A), revoke: () => Promise.resolve('unconfirmed') });
    expect(s.revoked).toEqual([[ORIGIN_A, LEGACY]]);
    expect(vault.legacy.size).toBe(0);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    await s.service.switchTo(B);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(sweepLines(s.log).sort()).toEqual([
      'log: conch-desktop: a sign-in saved by an older build (v10): deleted-unconfirmed',
      'log: conch-desktop: a sign-in saved by an older build (v10): deleted-unopened',
    ]);
  });

  it('a revocation that never answers is bounded, and counts as unconfirmed', async () => {
    const vault = legacyVault({ [A]: LEGACY });
    const s = await setup({ vault, files: profilesJson(A), revoke: () => new Promise(() => undefined), revokeBoundMs: 20 });
    expect(vault.legacy.has(A)).toBe(false);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): deleted-unconfirmed']);
  });

  it('an entry no saved server owns is deleted unopened, and no server is asked', async () => {
    const vault = legacyVault({ [ORPHAN]: LEGACY });
    const s = await setup({ vault, files: profilesJson(null) });
    expect(vault.legacy.size).toBe(0);
    expect(vault.calls).not.toContain(`decrypt ${ORPHAN}`);
    expect(s.asked).toEqual([]);
    expect(s.revoked).toEqual([]);
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): deleted-no-profile']);
  });

  it('keyring-sealed (v11) entries are never touched by the sweep, and restore as before', async () => {
    const vault = legacyVault({ [B]: 'tok-b' });
    vault.secrets.set(A, 'tok-a-v11');
    const s = await setup({ vault, files: profilesJson(A) });
    expect(vault.calls).not.toContain(`end-legacy ${A}`);
    expect(vault.secrets.get(A)).toBe('tok-a-v11');
    expect(s.revoked).toEqual([[ORIGIN_B, 'tok-b']]);
    // A starts signed in from its v11 entry, as before.
    expect(s.active().spec.token).toBe('tok-a-v11');
  });

  it('with nothing to sweep, it searches once and does nothing else', async () => {
    const s = await setup({ files: profilesJson(A) });
    expect(s.vault.calls.filter((c) => c === 'find-legacy')).toHaveLength(1);
    expect(s.vault.calls.filter((c) => c.startsWith('end-legacy'))).toEqual([]);
    expect(sweepLines(s.log)).toEqual([]);
    await s.service.switchTo(B);
    await s.service.switchTo(A);
    expect(s.vault.calls.filter((c) => c === 'find-legacy')).toHaveLength(1);
  });

  it('a vault that cannot be searched opens nothing; a read then deletes the entry unread, and the profile says it was not ended at the server', async () => {
    const failing = legacyVault({ [A]: LEGACY });
    failing.findFails = true;
    const s = await setup({ vault: failing, files: profilesJson(A) });
    expect(s.revoked).toEqual([]);
    expect(failing.calls).not.toContain(`decrypt ${A}`);
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): not-listed']);
    // A's activation read it: #2214's path deleted it unread (issue #2217's
    // review, N2: that is not ended at the server either).
    expect(failing.legacy.has(A)).toBe(false);
    expect(s.active().spec.token).toBeNull();
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(s.log).toContain('log: conch-desktop: a sign-in saved by an older build was deleted unread; sign in again');

    const vault = legacyVault({ [A]: LEGACY });
    const t = await setup({ vault, files: '{ nope' });
    expect(vault.calls).not.toContain('find-legacy');
    expect(vault.legacy.has(A)).toBe(true);
    expect(sweepLines(t.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): skipped']);
  });

  it.each([
    { result: { kind: 'exposed', removed: true }, line: 'a saved sign-in open to group or others: deleted-loose-mode' },
    { result: { kind: 'legacyRemoved' }, line: 'a sign-in saved by an older build was deleted unread; sign in again' },
  ] satisfies { result: GetResult; line: string }[])('records $result.kind for the right profile after its lookup was superseded', async ({ result, line }) => {
    const s = await setup({ files: profilesJson(A) });
    let startGet = (): void => undefined;
    const started = new Promise<void>((resolve) => {
      startGet = resolve;
    });
    let finishCleanup = (): void => undefined;
    const cleanup = new Promise<GetResult>((resolve) => {
      finishCleanup = () => {
        resolve(result);
      };
    });
    const get = s.vault.get.bind(s.vault);
    s.vault.get = (id) => {
      if (id !== B) return get(id);
      startGet();
      return cleanup;
    };

    const opening = s.service.switchTo(B);
    await started;
    const superseding = s.service.switchTo(A);
    finishCleanup();
    await Promise.all([opening, superseding]);
    s.vault.get = get;

    expect(s.service.view().notice).toBeNull();
    expect(s.log).toContain(`log: conch-desktop: ${line}`);
    await s.service.switchTo(B);
    expect(s.service.view().notice).toBe('legacy-not-ended');
  });

  // Issue #2217's review, N2: whenever an entry is not confirmed ended at the
  // server, the profile says so, whatever the delete did.
  it('an entry that was not ended and cannot be deleted says so, and the profile has the notice', async () => {
    for (const health of [unreachable, answered]) {
      const vault = legacyVault({ [A]: LEGACY });
      vault.legacyRemoveFails = true;
      const s = await setup({ vault, files: profilesJson(A), health, revoke: () => Promise.resolve('unconfirmed') });
      expect(s.service.view().notice).toBe('legacy-not-ended');
      expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): not-deleted']);
    }
  });

  it('an entry ended at the server but not deleted needs no notice, even when a read deletes it later', async () => {
    const vault = legacyVault({ [A]: LEGACY });
    vault.legacyRemoveFails = true;
    const s = await setup({ vault, files: profilesJson(A) });
    expect(s.revoked).toEqual([[ORIGIN_A, LEGACY]]);
    expect(s.service.view().notice).toBeNull();
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): ended-not-deleted']);
    vault.legacyRemoveFails = false;
    expect(await s.service.switchTo(B)).toBe(true);
    expect(await s.service.switchTo(A)).toBe(true);
    expect(vault.legacy.has(A)).toBe(false);
    expect(s.service.view().notice).toBe('sign-in-again');
  });

  it('an end that does not finish within its bound puts the notice up', async () => {
    const vault = legacyVault({ [A]: LEGACY });
    const endLegacy = vault.endLegacy.bind(vault);
    // A turn that never comes back.
    vault.endLegacy = () => new Promise(() => undefined);
    const s = await setup({ vault, files: profilesJson(A), vaultBoundMs: 5, healthBoundMs: 5, revokeBoundMs: 5 });
    vault.endLegacy = endLegacy;
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): late']);
  });

  it('an entry replaced before its turn puts the notice up: the old sign-in was not ended', async () => {
    const vault = legacyVault({ [A]: LEGACY });
    const findLegacy = vault.findLegacy.bind(vault);
    vault.findLegacy = async () => {
      const ids = await findLegacy();
      // Something replaced it between the search and its turn.
      vault.legacy.delete(A);
      return ids;
    };
    const s = await setup({ vault, files: profilesJson(A) });
    expect(s.revoked).toEqual([]);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): replaced']);
  });

  // Issue #2217's review, N3: the sweep opens an entry only for the health
  // document, not any 200.
  it('opens nothing for a server whose strict health read is not an answer, even if the lenient one is', async () => {
    const vault = legacyVault({ [A]: LEGACY });
    const s = await setup({ vault, files: profilesJson(A), health: answered, sweepHealth: unreachable });
    expect(vault.calls).not.toContain(`decrypt ${A}`);
    expect(s.revoked).toEqual([]);
    expect(vault.legacy.has(A)).toBe(false);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): deleted-unreachable']);
  });

  it('logs no secret, and only fixed lines from its closed vocabulary', async () => {
    const vault = legacyVault({ [A]: LEGACY, [B]: `${LEGACY}-b`, [ORPHAN]: `${LEGACY}-c` });
    const s = await setup({ vault, files: profilesJson(A), health: (origin) => (origin === ORIGIN_A ? answered() : unreachable()) });
    const lines = sweepLines(s.log);
    expect(lines).toHaveLength(3);
    for (const line of lines) expect(line).toMatch(LINE);
    for (const line of s.log) {
      expect(line).not.toContain('SECRET-canary');
      expect(line).not.toContain(A);
    }
    expect(JSON.stringify(s.service.view())).not.toContain('SECRET-canary');
  });

  it('does not hold up start or the window: a server that is slow to answer is swept in the background', async () => {
    let answer = (): void => undefined;
    const slow = new Promise<HealthResult>((resolve) => {
      answer = () => {
        resolve({ kind: 'answered', version: 'v1.0.0' });
      };
    });
    const vault = legacyVault({ [B]: 'tok-b' });
    // The sweep's question to B is the first; B's own window later gets an
    // answer at once.
    let askedB = 0;
    const health = (origin: string): Promise<HealthResult> => (origin === ORIGIN_B && askedB++ === 0 ? slow : answered());
    const s = await setup({ vault, files: profilesJson(A), health, background: true });
    let swept = false;
    void s.service.swept.then(() => {
      swept = true;
    });
    // Start is over and A's window is open, while B's sweep waits.
    expect(s.factory.made).toHaveLength(1);
    expect(s.active().spec.profile?.id).toBe(A);
    await Promise.resolve();
    expect(swept).toBe(false);
    expect(vault.legacy.has(B)).toBe(true);
    // While it waits, B's old entry is not a sign-in.
    expect(await s.service.switchTo(B)).toBe(true);
    expect(s.active().spec.token).toBeNull();
    expect(vault.calls).not.toContain(`get ${B}`);
    answer();
    await s.service.swept;
    expect(s.revoked).toEqual([[ORIGIN_B, 'tok-b']]);
    expect(vault.legacy.has(B)).toBe(false);
  });

  it('a search of the vault that never answers holds the window back no longer than the vault bound', async () => {
    const vault = legacyVault({ [A]: LEGACY });
    vault.findHangs = true;
    const s = await setup({ vault, files: profilesJson(A), vaultBoundMs: 20 });
    expect(s.factory.made).toHaveLength(1);
    expect(sweepLines(s.log)).toEqual(['log: conch-desktop: a sign-in saved by an older build (v10): not-listed']);
  });

  it('the notice ends when the person signs out of that profile or removes it', async () => {
    const vault = legacyVault({ [A]: LEGACY, [B]: 'tok-b' });
    const s = await setup({ vault, files: profilesJson(A), health: unreachable });
    expect(s.service.view().notice).toBe('legacy-not-ended');
    await s.service.signOut();
    // signOut's own notice says what it did; then none is left of the sweep.
    await s.service.switchTo(B);
    await s.service.switchTo(A);
    expect(s.service.view().notice).toBeNull();
    await s.service.switchTo(B);
    expect(s.service.view().notice).toBe('legacy-not-ended');
    expect(await s.service.remove(B)).toBe(true);
    expect(s.service.view().notice).toBeNull();
  });
});
