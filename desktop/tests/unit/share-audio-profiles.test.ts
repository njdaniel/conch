// The per-profile choice of the Linux screen share's sound (issue #1854): an
// optional public field of the profile's own metadata, absent meaning
// system_excluding_call. It survives a reload, differs per profile, is never
// rewritten into an old file by itself, lives in no app-wide setting or
// credential record, and is written only for the profile and switch generation
// that asked.
import { describe, expect, it } from 'vitest';

import { TeardownHooks } from '../../main/profiles/hooks.ts';
import { ProfilesService } from '../../main/profiles/service.ts';
import { DEFAULT_SHARE_SOUND, ProfileStore, parseState } from '../../main/profiles/store.ts';
import { fakeFactory, fakeVault, memoryFiles, ORIGIN_A, ORIGIN_B } from './profiles-support.ts';

const ID_A = 'a'.repeat(32);
const ID_B = 'b'.repeat(32);

function file(extra: Record<string, unknown> = {}, active: string | null = ID_A): string {
  return JSON.stringify({
    version: 1,
    profiles: [
      { id: ID_A, label: 'a', origin: ORIGIN_A, principal: { id: 7, name: 'ada' } },
      { id: ID_B, label: 'b', origin: ORIGIN_B, principal: null, ...extra },
    ],
    active,
  });
}

async function loaded(text: string) {
  const files = memoryFiles(text);
  const store = new ProfileStore(files);
  expect(await store.load()).toEqual({ kind: 'ok' });
  return { files, store };
}

describe('the stored choice', () => {
  it('is system_excluding_call when nothing was ever set, and nothing is written to say so', async () => {
    const { files, store } = await loaded(file());
    expect(DEFAULT_SHARE_SOUND).toBe('system_excluding_call');
    expect(store.shareSound(ID_A)).toBe('system_excluding_call');
    expect(files.writes).toBe(0);
    // Keeping the default on a profile that never had one writes nothing either.
    expect(await store.setShareSound(ID_A, 'system_excluding_call', () => true)).toBe(true);
    expect(files.writes).toBe(0);
    expect(files.text).toBe(file());
  });

  it('survives a reload, differs per profile, and is written with the profile’s own metadata', async () => {
    const { files, store } = await loaded(file());
    expect(await store.setShareSound(ID_A, 'none', () => true)).toBe(true);
    expect(files.writes).toBe(1);
    const again = new ProfileStore(files);
    await again.load();
    expect(again.shareSound(ID_A)).toBe('none');
    expect(again.shareSound(ID_B)).toBe('system_excluding_call');
    // Independent: the other profile's own choice does not move the first's.
    expect(await again.setShareSound(ID_B, 'none', () => true)).toBe(true);
    expect(await again.setShareSound(ID_A, 'system_excluding_call', () => true)).toBe(true);
    const third = new ProfileStore(files);
    await third.load();
    expect([third.shareSound(ID_A), third.shareSound(ID_B)]).toEqual(['system_excluding_call', 'none']);
  });

  it('is public and plain: the file holds the profile’s fields and this one, never a token or a secret', async () => {
    const { files, store } = await loaded(file());
    await store.setShareSound(ID_A, 'none', () => true);
    const doc = JSON.parse(files.text ?? '{}') as { profiles: Record<string, unknown>[] };
    expect(Object.keys(doc.profiles[0] ?? {}).sort()).toEqual(['id', 'label', 'origin', 'principal', 'shareAudio']);
    expect(doc.profiles[0]?.['shareAudio']).toBe('none');
    expect(Object.keys(doc.profiles[1] ?? {}).sort()).toEqual(['id', 'label', 'origin', 'principal']);
  });

  it('reads a file that has it, and refuses a file whose value is not one of the two words', async () => {
    const good = await loaded(file({ shareAudio: 'none' }));
    expect(good.store.shareSound(ID_B)).toBe('none');
    for (const bad of ['application', 'everything', '', 1, null, true, ['none']]) {
      expect(parseState(file({ shareAudio: bad })), JSON.stringify(bad)).toBeNull();
    }
    const corrupt = new ProfileStore(memoryFiles(file({ shareAudio: 'application' })));
    expect(await corrupt.load()).toEqual({ kind: 'corrupt' });
  });

  it('is written only while the caller is still the one that asked: a switch in between writes nothing', async () => {
    const { files, store } = await loaded(file());
    let current = true;
    const write = store.setShareSound(ID_A, 'none', () => current);
    current = false;
    expect(await write).toBe(false);
    expect(files.writes).toBe(0);
    expect(store.shareSound(ID_A)).toBe('system_excluding_call');
    // A profile that is gone, likewise.
    expect(await store.setShareSound('c'.repeat(32), 'none', () => true)).toBe(false);
  });

  it('never leaves a file that cannot be read back', async () => {
    const { files, store } = await loaded(file());
    files.failWrite = true;
    await expect(store.setShareSound(ID_A, 'none', () => true)).rejects.toThrow();
    expect(store.shareSound(ID_A)).toBe('system_excluding_call');
  });
});

describe('through the profiles service', () => {
  async function service(text: string) {
    const files = memoryFiles(text);
    const store = new ProfileStore(files);
    const log: string[] = [];
    const svc = new ProfilesService({
      store,
      vault: fakeVault(),
      hooks: new TeardownHooks(),
      createActivation: fakeFactory(log).create,
      probe: () => Promise.resolve('ok'),
      platform: 'linux',
      now: () => 0,
      log: (line) => log.push(line),
      compat: { servers: ['v1.0.0'], override: false },
      desktopVersion: '0.0.0',
      readHealth: () => Promise.resolve({ kind: 'answered', version: 'v1.0.0' }),
      readSweepHealth: () => Promise.resolve({ kind: 'answered', version: 'v1.0.0' }),
      verifyStored: () => Promise.resolve({ kind: 'rejected' }),
      revokeStored: () => Promise.resolve('ended'),
    });
    await svc.start();
    return { svc, store, files, log };
  }

  it('reads and writes the choice of the profile in force, and none without one', async () => {
    const s = await service(file({}, null));
    // No profile is active until one is switched to.
    expect(s.svc.shareSoundChoice()).toBeNull();
    expect(await s.svc.setShareSoundChoice('none')).toBe(false);
    expect(await s.svc.switchTo(ID_A)).toBe(true);
    expect(s.svc.shareSoundChoice()).toBe('system_excluding_call');
    expect(await s.svc.setShareSoundChoice('none')).toBe(true);
    expect(s.svc.shareSoundChoice()).toBe('none');
    // Another profile has its own.
    expect(await s.svc.switchTo(ID_B)).toBe(true);
    expect(s.svc.shareSoundChoice()).toBe('system_excluding_call');
    expect(await s.svc.switchTo(ID_A)).toBe(true);
    expect(s.svc.shareSoundChoice()).toBe('none');
  });

  it('a switch that overtakes the write writes nothing for either profile', async () => {
    const s = await service(file());
    await s.svc.switchTo(ID_A);
    const writes = s.files.writes;
    const write = s.svc.setShareSoundChoice('none');
    void s.svc.switchTo(ID_B);
    expect(await write).toBe(false);
    // The old profile's file is as it was; the new one was not written for either.
    expect(s.store.shareSound(ID_A)).toBe('system_excluding_call');
    expect(s.store.shareSound(ID_B)).toBe('system_excluding_call');
    expect(s.files.writes).toBe(writes);
  });

  it('a write that fails is false, logged by profile id alone', async () => {
    const s = await service(file());
    await s.svc.switchTo(ID_A);
    s.files.failWrite = true;
    expect(await s.svc.setShareSoundChoice('none')).toBe(false);
    expect(s.log.join('\n')).toContain(ID_A);
    expect(s.log.join('\n')).not.toContain('example');
  });

  it('is not in the profile view the page reads, nor in the app-wide settings', async () => {
    const s = await service(file({ shareAudio: 'none' }));
    await s.svc.switchTo(ID_B);
    expect(JSON.stringify(s.svc.view())).not.toMatch(/shareAudio|share_audio|system_excluding_call/);
  });
});
