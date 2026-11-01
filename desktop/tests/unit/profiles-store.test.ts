import { mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { createProfileFiles } from '../../main/profiles/files.ts';
import { isProfileId, newProfileId } from '../../main/profiles/ids.ts';
import { cleanLabel, ProfileStore } from '../../main/profiles/store.ts';
import { memoryFiles, ORIGIN_A, ORIGIN_B } from './profiles-support.ts';

describe('profile ids', () => {
  it('are 128 random bits as 32 lowercase hex characters, and never repeat', () => {
    const ids = new Set(Array.from({ length: 200 }, newProfileId));
    expect(ids.size).toBe(200);
    for (const id of ids) expect(id).toMatch(/^[0-9a-f]{32}$/);
    expect(isProfileId('ABCDEF0123456789abcdef0123456789')).toBe(false);
    expect(isProfileId('../x')).toBe(false);
    expect(isProfileId(5)).toBe(false);
  });
});

describe('the profile store', () => {
  it('adds, renames and removes; a label is never a key', async () => {
    const files = memoryFiles();
    const store = new ProfileStore(files);
    await store.load();
    const a = await store.add(ORIGIN_A);
    const b = await store.add(ORIGIN_A, 'Work');
    expect(a?.label).toBe('a.example');
    expect(b?.label).toBe('Work');
    expect(a?.id).not.toBe(b?.id);
    expect(files.dirs.size).toBe(2);
    expect(await store.rename(a?.id ?? '', '  Home  ')).toBe(true);
    expect(store.get(a?.id ?? '')?.label).toBe('Home');
    expect(store.get(a?.id ?? '')?.id).toBe(a?.id);
    expect(await store.rename(a?.id ?? '', '')).toBe(false);
    expect(await store.rename(a?.id ?? '', 'x\u0007y')).toBe(false);
    expect(await store.remove(a?.id ?? '')).toBe(true);
    expect(files.dirs.has(a?.id ?? '')).toBe(false);
    expect(store.list().map((p) => p.label)).toEqual(['Work']);
    expect(await store.remove(a?.id ?? '')).toBe(false);
  });

  it('refuses an origin that is not canonical, and a label that is not text', async () => {
    const store = new ProfileStore(memoryFiles());
    await store.load();
    expect(await store.add('https://a.example/path')).toBeNull();
    expect(await store.add('HTTP://a.example')).toBeNull();
    expect(await store.add(ORIGIN_A, '\n')).toBeNull();
    expect(cleanLabel('x'.repeat(65))).toBeNull();
    expect(cleanLabel(' ok ')).toBe('ok');
  });

  it('binds on the first sign-in to (origin, principal), and never silently replaces', async () => {
    const store = new ProfileStore(memoryFiles());
    await store.load();
    const first = await store.add(ORIGIN_A);
    const second = await store.add(ORIGIN_A);
    const other = await store.add(ORIGIN_B);
    const ada = { id: 7, name: 'ada' };
    const grace = { id: 8, name: 'grace' };
    expect(await store.bind(first?.id ?? '', ada)).toEqual({ ok: true });
    // The same account again is fine, and a rename on the server updates the name.
    expect(await store.bind(first?.id ?? '', ada)).toEqual({ ok: true });
    // Another principal on a bound profile: refused, the first binding stays.
    expect(await store.bind(first?.id ?? '', grace)).toEqual({ ok: false, reason: 'other-account', bound: ada });
    expect(store.get(first?.id ?? '')?.principal).toEqual(ada);
    // The same account on the same server in a second profile: refused.
    expect(await store.bind(second?.id ?? '', ada)).toEqual({ ok: false, reason: 'duplicate', existing: first?.id });
    // Another account on the same server uses the other profile.
    expect(await store.bind(second?.id ?? '', grace)).toEqual({ ok: true });
    // The same principal id on another server is another account.
    expect(await store.bind(other?.id ?? '', ada)).toEqual({ ok: true });
    expect(await store.bind('0'.repeat(32), ada)).toEqual({ ok: false, reason: 'missing' });
  });

  it('rejects bidi and zero-width format characters in a label', () => {
    expect(cleanLabel('a\u202Eb')).toBeNull();
    expect(cleanLabel('a\u200Bb')).toBeNull();
    expect(cleanLabel('a\u2066b')).toBeNull();
    expect(cleanLabel('Büro')).toBe('Büro');
  });

  it('writes a state the next launch accepts, whatever ran at once', async () => {
    const files = memoryFiles();
    const store = new ProfileStore(files);
    await store.load();
    const one = await store.add(ORIGIN_A);
    const two = await store.add(ORIGIN_A);
    const ada = { id: 7, name: 'ada' };
    // The same account on two profiles of one server, at the same moment: one wins.
    const [x, y] = await Promise.all([store.bind(one?.id ?? '', ada), store.bind(two?.id ?? '', ada)]);
    expect([x.ok, y.ok].sort()).toEqual([false, true]);
    await Promise.all([store.add(ORIGIN_B), store.add(ORIGIN_B), store.rename(one?.id ?? '', 'x'), store.remove(two?.id ?? '')]);
    const again = new ProfileStore(memoryFiles(files.text ?? ''));
    expect(await again.load()).toEqual({ kind: 'ok' });
    expect(again.list()).toEqual(store.list());
  });

  it('keeps nothing secret in profiles.json, and reads back what it wrote', async () => {
    const files = memoryFiles();
    const store = new ProfileStore(files);
    await store.load();
    const p = await store.add(ORIGIN_A);
    await store.bind(p?.id ?? '', { id: 7, name: 'ada' });
    await store.setActive(p?.id ?? '');
    const again = new ProfileStore(memoryFiles(files.text ?? ''));
    expect(await again.load()).toEqual({ kind: 'ok' });
    expect(again.list()).toEqual(store.list());
    expect(again.active).toBe(p?.id);
    expect(Object.keys(JSON.parse(files.text ?? '{}') as object).sort()).toEqual(['active', 'profiles', 'version']);
  });

  it.each([
    ['not JSON', '{'],
    ['another version', '{"version":2,"profiles":[]}'],
    ['an extra key', '{"version":1,"profiles":[],"token":"x"}'],
    ['an id that is not 32 hex', '{"version":1,"profiles":[{"id":"abc","label":"a","origin":"https://a.example","principal":null}]}'],
    ['an origin that is not canonical', `{"version":1,"profiles":[{"id":"${'a'.repeat(32)}","label":"a","origin":"https://a.example/x","principal":null}]}`],
    ['a repeated id', `{"version":1,"profiles":[{"id":"${'a'.repeat(32)}","label":"a","origin":"https://a.example","principal":null},{"id":"${'a'.repeat(32)}","label":"b","origin":"https://b.example","principal":null}]}`],
    ['an active profile that is not listed', `{"version":1,"profiles":[],"active":"${'a'.repeat(32)}"}`],
    ['one account bound twice', `{"version":1,"profiles":[{"id":"${'a'.repeat(32)}","label":"a","origin":"https://a.example","principal":{"id":1,"name":"x"}},{"id":"${'b'.repeat(32)}","label":"b","origin":"https://a.example","principal":{"id":1,"name":"x"}}]}`],
  ])('calls a file with %s corrupt, and changes nothing', async (_name, text) => {
    const files = memoryFiles(text);
    const store = new ProfileStore(files);
    expect(await store.load()).toEqual({ kind: 'corrupt' });
    expect(store.corrupt).toBe(true);
    // No mutation writes anything, and nothing is quarantined, until recover.
    await expect(store.add(ORIGIN_A)).rejects.toThrow();
    expect(files.writes).toBe(0);
    expect(files.text).toBe(text);
    expect(files.quarantined).toBe(0);
    expect(files.dirs.size).toBe(0);
  });

  it('recovers a corrupt file only when asked: sets it aside and starts empty', async () => {
    const files = memoryFiles('{');
    const store = new ProfileStore(files);
    await store.load();
    await store.recover();
    expect(store.corrupt).toBe(false);
    expect(files.quarantined).toBe(1);
    expect(store.list()).toEqual([]);
    expect(await store.add(ORIGIN_A)).not.toBeNull();
  });
});

describe('recovery never destroys the original', () => {
  it('does not write when the file cannot be set aside, and the list stays unusable', async () => {
    const files = memoryFiles('{ nope');
    files.failQuarantine = true;
    const store = new ProfileStore(files);
    await store.load();
    await expect(store.recover()).rejects.toThrow();
    expect(files.text).toBe('{ nope');
    expect(files.writes).toBe(0);
    expect(store.corrupt).toBe(true);
    await expect(store.add(ORIGIN_A)).rejects.toThrow();
  });

  it('keeps the original, set aside, when the fresh file cannot be written', async () => {
    const files = memoryFiles('{ nope');
    const store = new ProfileStore(files);
    await store.load();
    files.failWrite = true;
    await expect(store.recover()).rejects.toThrow();
    expect(files.aside).toEqual(['{ nope']);
    expect(store.corrupt).toBe(true);
    files.failWrite = false;
    await store.recover();
    expect(store.corrupt).toBe(false);
  });
});

describe('changes made at once all stand', () => {
  it('a bind and a rename of another profile', async () => {
    const files = memoryFiles();
    const store = new ProfileStore(files);
    await store.load();
    const one = await store.add(ORIGIN_A);
    const two = await store.add(ORIGIN_B);
    const [bound, renamed] = await Promise.all([store.bind(one?.id ?? '', { id: 7, name: 'ada' }), store.rename(two?.id ?? '', 'Work')]);
    expect(bound).toEqual({ ok: true });
    expect(renamed).toBe(true);
    expect(store.get(one?.id ?? '')?.principal?.name).toBe('ada');
    expect(store.get(two?.id ?? '')?.label).toBe('Work');
    const again = new ProfileStore(memoryFiles(files.text ?? ''));
    await again.load();
    expect(again.list()).toEqual(store.list());
  });

  it('an add and a remove', async () => {
    const files = memoryFiles();
    const store = new ProfileStore(files);
    await store.load();
    const one = await store.add(ORIGIN_A);
    const [added, removed] = await Promise.all([store.add(ORIGIN_B), store.remove(one?.id ?? '')]);
    expect(removed).toBe(true);
    expect(store.list().map((p) => p.id)).toEqual([added?.id]);
    const again = new ProfileStore(memoryFiles(files.text ?? ''));
    await again.load();
    expect(again.list()).toEqual(store.list());
  });
});

describe('profiles.json on disk', () => {
  const roots: string[] = [];
  afterEach(() => {
    for (const r of roots.splice(0)) rmSync(r, { recursive: true, force: true });
  });
  const root = (): string => {
    const r = join(mkdtempSync(join(tmpdir(), 'conch-profiles-')), 'data');
    roots.push(join(r, '..'));
    return r;
  };

  it('makes the root 0700 and the file 0600, replaces it atomically and leaves no temp file', async () => {
    const dir = root();
    const store = new ProfileStore(createProfileFiles(dir));
    await store.load();
    const p = await store.add(ORIGIN_A);
    await store.setActive(p?.id ?? '');
    expect(statSync(dir).mode & 0o777).toBe(0o700);
    expect(statSync(join(dir, 'profiles.json')).mode & 0o777).toBe(0o600);
    expect(statSync(join(dir, 'profiles')).mode & 0o777).toBe(0o700);
    expect(statSync(join(dir, 'profiles', p?.id ?? '')).mode & 0o777).toBe(0o700);
    expect(readdirSync(dir).filter((n) => n.endsWith('.tmp'))).toEqual([]);
    await store.remove(p?.id ?? '');
    expect(readdirSync(join(dir, 'profiles'))).toEqual([]);
  });

  it('refuses a directory it does not own', async () => {
    const dir = root();
    const files = createProfileFiles(dir, { uid: (process.getuid?.() ?? 0) + 1 });
    await expect(files.write('{}')).rejects.toThrow();
    await expect(files.ensureProfileDir('a'.repeat(32))).rejects.toThrow();
    expect(readdirSync(dir).filter((n) => n === 'profiles.json')).toEqual([]);
  });

  it('a corrupt profiles.json gives a recovery state and changes no other file', async () => {
    const dir = root();
    const files = createProfileFiles(dir);
    const store = new ProfileStore(files);
    await store.load();
    const p = await store.add(ORIGIN_A);
    writeFileSync(join(dir, 'profiles.json'), '{ not json');
    const other = join(dir, 'profiles', p?.id ?? '', 'note');
    writeFileSync(other, 'keep');
    const before = readdirSync(dir).sort();
    const again = new ProfileStore(files);
    expect(await again.load()).toEqual({ kind: 'corrupt' });
    expect(readFileSync(join(dir, 'profiles.json'), 'utf8')).toBe('{ not json');
    expect(readFileSync(other, 'utf8')).toBe('keep');
    expect(readdirSync(dir).sort()).toEqual(before);
    await again.recover();
    expect(readdirSync(dir).some((n) => /^profiles\.json\.corrupt-\d+-[0-9a-f]{8}$/.test(n))).toBe(true);
    expect(readFileSync(other, 'utf8')).toBe('keep');
  });
});
