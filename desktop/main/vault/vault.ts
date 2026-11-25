// The credential vault: one bearer token per server profile, sealed with
// Electron's safeStorage, and only when the storage is a protected one.
//
// Input: an injected SafeStorage (the asynchronous API only; the synchronous
// one is removed in Electron 46), a data root, the platform and a log sink.
// Output: results as values, never thrown secrets. This module owns the
// decision "store or session-only" and the sealing; classify.ts owns the rule,
// files.ts the disk. It touches no IPC and no preload (D2-6 wires it).
//
// Rules: under any classification but `protected`, nothing is written, and on
// Linux a ciphertext is written only if it is sealed with the keyring's key
// (classify.ts, issue #2077): one sealed with Chromium's built-in key is
// refused even when the classification said protected a moment before. A
// decrypt error or corrupt file makes that one profile's credential unusable
// and touches no other profile's file. A provider that cannot answer is a
// retryable state, never an erase. A Linux file sealed with the built-in key
// (written before #2077) protects nothing: it is deleted unread, and the
// person signs in again. Plain-text mode is never switched on, and nothing
// here logs a secret, ciphertext or a path.
//
// The sweep of issue #2217 also finds such files at start (findLegacy) and
// ends each one (endLegacy): opened only to be handed to the caller's one use
// (the server's revocation), or not opened at all, and then deleted. The
// secret goes nowhere else: it is not returned, kept or logged.
import type { MainLog } from '../diagnostics.ts';
import { classify, linuxSeal, type Classification, type SafeStorageProbe } from './classify.ts';
import {
  profileIds,
  readCredential,
  removeProfile,
  UnsafeRoot,
  validProfileId,
  writeCredential,
} from './files.ts';

export type { Classification, SessionOnlyReason } from './classify.ts';

// The asynchronous part of Electron's safeStorage that the vault uses.
export interface VaultStorage extends SafeStorageProbe {
  encryptStringAsync(plainText: string): Promise<Buffer>;
  decryptStringAsync(encrypted: Buffer): Promise<{ result: string; shouldReEncrypt: boolean }>;
}

export interface VaultDeps {
  readonly storage: VaultStorage;
  readonly root: string;
  readonly platform: NodeJS.Platform;
  readonly isReady: () => boolean;
  // Receives fixed event names and a profile id. Never a secret.
  readonly log: MainLog;
  // Test seam for the directory fsync (see files.ts).
  readonly syncDir?: (dir: string) => Promise<void>;
}

type NotProtected = Extract<Classification, { kind: 'sessionOnly' | 'temporarilyUnavailable' }>;

export type PutResult =
  | { readonly kind: 'stored' }
  | NotProtected
  // Sealing or writing failed; the previous credential, if any, is intact.
  | { readonly kind: 'failed' };

export type GetResult =
  | { readonly kind: 'found'; readonly secret: string }
  | { readonly kind: 'absent' }
  // Cannot be decrypted or is corrupt. The file is left alone; the user signs
  // in again, and that put replaces it.
  | { readonly kind: 'unusable' }
  | NotProtected;

export type RemoveResult = { readonly kind: 'removed' } | { readonly kind: 'failed' };

// What endLegacy did with one profile's entry (issue #2217). not-legacy: the
// file is gone, or is no longer sealed with the built-in key (a new sign-in
// replaced it), and nothing was done. ended: it was sealed with the built-in
// key; opened says whether it was decrypted and handed to the use (with what
// the use answered), and removed whether the file is gone.
export type LegacyEnd<T> =
  | { readonly kind: 'not-legacy' }
  | { readonly kind: 'ended'; readonly opened: false; readonly removed: boolean }
  | { readonly kind: 'ended'; readonly opened: true; readonly used: T; readonly removed: boolean };

export interface Vault {
  classify(): Promise<Classification>;
  put(profileId: string, secret: string): Promise<PutResult>;
  get(profileId: string): Promise<GetResult>;
  remove(profileId: string): Promise<RemoveResult>;
  // The profile ids whose entry is sealed with Chromium's built-in key, on
  // Linux (issue #2217); none elsewhere, and none from a root that is missing
  // or not private.
  findLegacy(): Promise<readonly string[]>;
  // Ends one such entry: with a use, it is decrypted only to be handed to it,
  // once; with null it is not decrypted. Either way it is then deleted. An
  // entry not sealed with the built-in key is left alone. use must not throw.
  endLegacy<T>(profileId: string, use: ((secret: string) => Promise<T>) | null): Promise<LegacyEnd<T>>;
}

export function createVault(deps: VaultDeps): Vault {
  const state = (): Promise<Classification> =>
    classify({ storage: deps.storage, platform: deps.platform, isReady: deps.isReady });

  // One operation at a time per profile id, so a re-encrypt in a get cannot
  // write back what a remove just deleted, nor over a newer put. Within this
  // process only: another process is kept out by the single-instance lock of
  // #945.
  const tails = new Map<string, Promise<unknown>>();
  const serial = <T>(id: string, op: () => Promise<T>): Promise<T> => {
    const run = (tails.get(id) ?? Promise.resolve()).then(op);
    const tail = run.catch(() => undefined);
    tails.set(id, tail);
    void tail.then(() => {
      if (tails.get(id) === tail) {
        tails.delete(id);
      }
    });
    return run;
  };
  const write = (id: string, data: Uint8Array): Promise<void> =>
    writeCredential(deps.root, id, data, { syncDir: deps.syncDir });

  const event = (
    name: NotProtected['kind'] | 'unsafe-root' | 'no-keyring' | 'built-in-key-removed' | 'put-failed' | 'read-failed' | 'corrupt' | 'decrypt-failed' | 'reencrypt-failed' | 'remove-failed',
    profileId: string,
  ): void => {
    deps.log(`vault: ${name}`, { ring: true });
    deps.log(`vault: ${name} profile=${profileId}`);
  };

  async function doPut(profileId: string, secret: string): Promise<PutResult> {
    const c = await state();
    if (c.kind !== 'protected') {
      event(c.kind, profileId);
      return c;
    }
    let sealed: Buffer;
    try {
      sealed = await deps.storage.encryptStringAsync(secret);
    } catch {
      event('put-failed', profileId);
      return { kind: 'failed' };
    }
    const seal = deps.platform === 'linux' ? linuxSeal(sealed) : 'keyring';
    if (seal !== 'keyring') {
      // The keyring stopped giving its key between the classification and
      // this seal (locked, or gone): never write what the built-in key, or a
      // key this build does not know, sealed.
      event('no-keyring', profileId);
      return { kind: 'sessionOnly', reason: seal === 'built-in' ? 'no-keyring' : 'unknown-backend' };
    }
    try {
      await write(profileId, sealed);
    } catch (err) {
      if (err instanceof UnsafeRoot) {
        // Not a transient failure: the user must fix the directory. Reported as
        // session-only with its own reason so #945 can say so; nothing written.
        event('unsafe-root', profileId);
        return { kind: 'sessionOnly', reason: 'unsafe-root' };
      }
      event('put-failed', profileId);
      return { kind: 'failed' };
    }
    return { kind: 'stored' };
  }

  async function doGet(profileId: string): Promise<GetResult> {
    let read;
    try {
      read = await readCredential(deps.root, profileId);
    } catch {
      event('read-failed', profileId);
      return { kind: 'unusable' };
    }
    if (read.kind === 'absent') {
      return read;
    }
    if (read.kind === 'unsafe-root') {
      // Never decrypt from a directory others may have written to.
      event('unsafe-root', profileId);
      return { kind: 'sessionOnly', reason: 'unsafe-root' };
    }
    if (read.kind === 'corrupt') {
      event('corrupt', profileId);
      return { kind: 'unusable' };
    }
    if (deps.platform === 'linux' && linuxSeal(read.data) === 'built-in') {
      // Sealed with Chromium's built-in key (by a build before #2077, on a
      // desktop with no keyring that answered): anyone who can read the file
      // can open it. It is never decrypted or used; it is deleted, and the
      // person signs in again.
      try {
        await removeProfile(deps.root, profileId);
      } catch {
        event('remove-failed', profileId);
        return { kind: 'unusable' };
      }
      event('built-in-key-removed', profileId);
      return { kind: 'unusable' };
    }
    const c = await state();
    if (c.kind !== 'protected') {
      // The file may be sealed with a key we would not trust now (a hardcoded
      // one) or the provider is asleep: do not decrypt, do not erase.
      event(c.kind, profileId);
      return c;
    }
    let opened: { result: string; shouldReEncrypt: boolean };
    try {
      opened = await deps.storage.decryptStringAsync(read.data);
    } catch {
      event('decrypt-failed', profileId);
      return { kind: 'unusable' };
    }
    if (opened.shouldReEncrypt) {
      try {
        const resealed = await deps.storage.encryptStringAsync(opened.result);
        // The same rule as a put: on Linux only the keyring's key is written.
        // Otherwise the old file, sealed with the keyring, stays as it is.
        if (deps.platform === 'linux' && linuxSeal(resealed) !== 'keyring') event('no-keyring', profileId);
        else await write(profileId, resealed);
      } catch {
        // The secret is still good; the next get tries again.
        event('reencrypt-failed', profileId);
      }
    }
    return { kind: 'found', secret: opened.result };
  }

  async function doRemove(profileId: string): Promise<RemoveResult> {
    try {
      await removeProfile(deps.root, profileId);
    } catch {
      // A fixed result: a raw fs error carries a path.
      event('remove-failed', profileId);
      return { kind: 'failed' };
    }
    return { kind: 'removed' };
  }

  async function doEndLegacy<T>(profileId: string, use: ((secret: string) => Promise<T>) | null): Promise<LegacyEnd<T>> {
    if (deps.platform !== 'linux') return { kind: 'not-legacy' };
    // The same read as a get: readCredential's lstat checks (a plain, private
    // root; a plain directory; a regular file of a sane size), then the mark.
    let read;
    try {
      read = await readCredential(deps.root, profileId);
    } catch {
      return { kind: 'not-legacy' };
    }
    if (read.kind !== 'bytes' || linuxSeal(read.data) !== 'built-in') return { kind: 'not-legacy' };
    let used: { readonly value: T } | null = null;
    if (use !== null && deps.isReady()) {
      // The built-in key opens it whatever the keyring says; nothing it holds
      // is written back (no re-encrypt), returned or logged.
      let secret: string | null = null;
      try {
        secret = (await deps.storage.decryptStringAsync(read.data)).result;
      } catch {
        secret = null;
      }
      if (secret !== null) {
        try {
          used = { value: await use(secret) };
        } catch {
          used = null;
        } finally {
          // Dropped at once: the use was its one hand-off.
          secret = null;
        }
      }
    }
    let removed = true;
    try {
      await removeProfile(deps.root, profileId);
    } catch {
      removed = false;
    }
    return used === null ? { kind: 'ended', opened: false, removed } : { kind: 'ended', opened: true, used: used.value, removed };
  }

  return {
    classify: state,
    async put(profileId, secret) {
      requireId(profileId);
      return serial(profileId, () => doPut(profileId, secret));
    },
    async get(profileId) {
      requireId(profileId);
      return serial(profileId, () => doGet(profileId));
    },
    async remove(profileId) {
      requireId(profileId);
      return serial(profileId, () => doRemove(profileId));
    },
    async findLegacy() {
      if (deps.platform !== 'linux') return [];
      const found: string[] = [];
      for (const id of await profileIds(deps.root)) {
        // Read in its own turn, as a get would be, so that it sees what an
        // operation already asked of that profile has left.
        const legacy = await serial(id, async () => {
          const read = await readCredential(deps.root, id).catch(() => null);
          return read !== null && read.kind === 'bytes' && linuxSeal(read.data) === 'built-in';
        });
        if (legacy) found.push(id);
      }
      return found;
    },
    async endLegacy(profileId, use) {
      requireId(profileId);
      return serial(profileId, () => doEndLegacy(profileId, use));
    },
  };
}

function requireId(profileId: string): void {
  if (!validProfileId(profileId)) {
    throw new Error('invalid profile id');
  }
}
