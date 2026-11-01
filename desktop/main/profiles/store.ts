// The saved server profiles (linux-desktop.md §4): a random local id mapped to
// a canonical origin and, after the first sign-in, the verified principal.
// Never keyed by display name or hostname. Input: a ProfileFiles. Output: the
// list, and the mutations of it, each written before it is visible.
//
// The binding rule: an unbound profile binds on its first sign-in to
// (origin, principal id). A bound profile never takes another principal, and
// two profiles never hold the same (origin, principal id): a second account on
// a server is a second profile. Nothing here is secret.
import { parseServerAddress } from '../api/origin.ts';
import type { ProfileFiles } from './files.ts';
import { isProfileId, newProfileId } from './ids.ts';

export interface Principal {
  readonly id: number;
  readonly name: string;
}

export interface Profile {
  readonly id: string;
  readonly label: string;
  readonly origin: string;
  readonly principal: Principal | null;
}

export const MAX_PROFILES = 32;
export const MAX_LABEL = 64;

export type LoadResult = { readonly kind: 'ok' } | { readonly kind: 'corrupt' };

export type BindResult =
  | { readonly ok: true }
  | { readonly ok: false; readonly reason: 'missing' }
  // The profile belongs to another account.
  | { readonly ok: false; readonly reason: 'other-account'; readonly bound: Principal }
  // Another profile already holds this account on this server.
  | { readonly ok: false; readonly reason: 'duplicate'; readonly existing: string };

interface State {
  readonly profiles: readonly Profile[];
  readonly active: string | null;
}

const EMPTY: State = { profiles: [], active: null };

// cleanLabel is a display label: trimmed, 1 to 64 characters, no control
// character. null when it is not one.
export function cleanLabel(input: string): string | null {
  const label = input.trim();
  if (label.length === 0 || label.length > MAX_LABEL || /[\u0000-\u001f\u007f-\u009f\u2028\u2029\p{Cf}]/u.test(label)) return null;
  return label;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function parseProfile(value: unknown): Profile | null {
  if (!isRecord(value)) return null;
  const { id, label, origin, principal } = value;
  if (Object.keys(value).some((k) => !['id', 'label', 'origin', 'principal'].includes(k))) return null;
  if (!isProfileId(id) || typeof label !== 'string' || cleanLabel(label) !== label || typeof origin !== 'string') return null;
  const parsed = parseServerAddress(origin);
  if (!parsed.ok || parsed.origin !== origin) return null;
  if (principal === null) return { id, label, origin, principal: null };
  if (!isRecord(principal) || Object.keys(principal).length !== 2) return null;
  const { id: pid, name } = principal;
  if (typeof pid !== 'number' || !Number.isSafeInteger(pid) || pid <= 0 || typeof name !== 'string' || name.length === 0 || name.length > 256) return null;
  return { id, label, origin, principal: { id: pid, name } };
}

// parseState reads profiles.json's text, or null when it is not exactly what
// this store writes.
export function parseState(text: string): State | null {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch {
    return null;
  }
  if (!isRecord(data) || data['version'] !== 1 || !Array.isArray(data['profiles']) || data['profiles'].length > MAX_PROFILES) return null;
  if (Object.keys(data).some((k) => !['version', 'profiles', 'active'].includes(k))) return null;
  const profiles: Profile[] = [];
  for (const raw of data['profiles'] as unknown[]) {
    const profile = parseProfile(raw);
    if (profile === null) return null;
    profiles.push(profile);
  }
  const ids = new Set(profiles.map((p) => p.id));
  const bound = new Set(profiles.flatMap((p) => (p.principal === null ? [] : [`${p.origin}\n${String(p.principal.id)}`])));
  if (ids.size !== profiles.length || bound.size !== profiles.filter((p) => p.principal !== null).length) return null;
  const active = data['active'] ?? null;
  if (active !== null && (typeof active !== 'string' || !ids.has(active))) return null;
  return { profiles, active };
}

export class ProfileStore {
  readonly #files: ProfileFiles;
  readonly #newId: () => string;
  #state: State = EMPTY;
  #corrupt = false;
  #tail: Promise<unknown> = Promise.resolve();

  constructor(files: ProfileFiles, newId: () => string = newProfileId) {
    this.#files = files;
    this.#newId = newId;
  }

  // load reads profiles.json. A missing file is an empty list; one that is
  // unreadable or not what this store writes is corrupt, and then nothing is
  // changed on disk until recover().
  async load(): Promise<LoadResult> {
    const read = await this.#files.read();
    if (read.kind === 'absent') {
      this.#state = EMPTY;
      this.#corrupt = false;
      return { kind: 'ok' };
    }
    const state = read.kind === 'text' ? parseState(read.text) : null;
    if (state === null) {
      this.#state = EMPTY;
      this.#corrupt = true;
      return { kind: 'corrupt' };
    }
    this.#state = state;
    this.#corrupt = false;
    return { kind: 'ok' };
  }

  get corrupt(): boolean {
    return this.#corrupt;
  }

  list(): readonly Profile[] {
    return this.#state.profiles;
  }

  get(id: string): Profile | undefined {
    return this.#state.profiles.find((p) => p.id === id);
  }

  // The profile that was active when the app last ran.
  get active(): string | null {
    return this.#state.active;
  }

  // recover sets a corrupt file aside and starts an empty list. The file must
  // be moved to a name of its own first; if it cannot be, nothing is written and
  // this rejects, so the original is never overwritten. The vault and every
  // profile directory are left as they are.
  async recover(): Promise<void> {
    if (!this.#corrupt) return;
    await this.#files.quarantine();
    this.#corrupt = false;
    try {
      await this.#commit(() => EMPTY);
    } catch (err) {
      // The original is safe where it was moved; the list stays unusable until
      // a fresh one can be written.
      this.#corrupt = true;
      throw err;
    }
  }

  // commit applies a change to the state as it is when its turn comes, so
  // changes made at once do not overwrite each other, and writes the result
  // only if it is a state the next launch will accept. A change returns null
  // when it no longer applies; commit then writes nothing and says false.
  #commit(change: (state: State) => State | null): Promise<boolean> {
    const run = this.#tail.then(async () => {
      if (this.#corrupt) throw new Error('profiles.json is corrupt');
      const next = change(this.#state);
      if (next === null) return false;
      const text = `${JSON.stringify({ version: 1, profiles: next.profiles, active: next.active }, null, 2)}\n`;
      if (parseState(text) === null) throw new Error('refusing to write a state that cannot be read back');
      await this.#files.write(text);
      this.#state = next;
      return true;
    });
    this.#tail = run.catch(() => undefined);
    return run;
  }

  // add saves a new, unbound profile for a canonical origin.
  async add(origin: string, label?: string): Promise<Profile | null> {
    if (this.#corrupt) throw new Error('profiles.json is corrupt');
    const parsed = parseServerAddress(origin);
    if (!parsed.ok || parsed.origin !== origin) return null;
    const named = cleanLabel(label ?? new URL(origin).host);
    if (named === null) return null;
    const profile: Profile = { id: this.#newId(), label: named, origin, principal: null };
    await this.#files.ensureProfileDir(profile.id);
    try {
      const added = await this.#commit((state) => (state.profiles.length >= MAX_PROFILES ? null : { ...state, profiles: [...state.profiles, profile] }));
      if (!added) {
        await this.#files.removeProfileDir(profile.id).catch(() => undefined);
        return null;
      }
    } catch (err) {
      await this.#files.removeProfileDir(profile.id).catch(() => undefined);
      throw err;
    }
    return profile;
  }

  // rename changes the display label, and nothing that is a key.
  async rename(id: string, label: string): Promise<boolean> {
    const named = cleanLabel(label);
    if (named === null) return false;
    return this.#commit((state) =>
      state.profiles.some((p) => p.id === id) ? { ...state, profiles: state.profiles.map((p) => (p.id === id ? { ...p, label: named } : p)) } : null,
    );
  }

  // remove forgets the profile and deletes its directory. The caller deletes
  // the vault entry.
  async remove(id: string): Promise<boolean> {
    const removed = await this.#commit((state) =>
      state.profiles.some((p) => p.id === id) ? { profiles: state.profiles.filter((p) => p.id !== id), active: state.active === id ? null : state.active } : null,
    );
    if (removed) await this.#files.removeProfileDir(id).catch(() => undefined);
    return removed;
  }

  async setActive(id: string | null): Promise<void> {
    await this.#commit((state) => ((id !== null && !state.profiles.some((p) => p.id === id)) || state.active === id ? null : { ...state, active: id }));
  }

  // bind applies the binding rule for a sign-in as principal on profile id, to
  // the state as it is when this turn comes.
  async bind(id: string, principal: Principal): Promise<BindResult> {
    let result: BindResult = { ok: false, reason: 'missing' };
    await this.#commit((state) => {
      const profile = state.profiles.find((p) => p.id === id);
      if (profile === undefined) {
        result = { ok: false, reason: 'missing' };
        return null;
      }
      if (profile.principal !== null) {
        if (profile.principal.id !== principal.id) {
          result = { ok: false, reason: 'other-account', bound: profile.principal };
          return null;
        }
        if (profile.principal.name === principal.name) {
          result = { ok: true };
          return null;
        }
      } else {
        const other = state.profiles.find((p) => p.origin === profile.origin && p.principal?.id === principal.id);
        if (other !== undefined) {
          result = { ok: false, reason: 'duplicate', existing: other.id };
          return null;
        }
      }
      result = { ok: true };
      return { ...state, profiles: state.profiles.map((p) => (p.id === id ? { ...p, principal } : p)) };
    });
    return result;
  }
}
