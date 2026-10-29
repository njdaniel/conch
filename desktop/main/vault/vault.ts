// The credential vault: one bearer token per server profile, sealed with
// Electron's safeStorage, and only when the storage is a protected one.
//
// Input: an injected SafeStorage (the asynchronous API only; the synchronous
// one is removed in Electron 46), a data root, the platform and a log sink.
// Output: results as values, never thrown secrets. This module owns the
// decision "store or session-only" and the sealing; classify.ts owns the rule,
// files.ts the disk. It touches no IPC and no preload (D2-6 wires it).
//
// Rules: under any classification but `protected`, nothing is written. A
// decrypt error or corrupt file makes that one profile's credential unusable
// and touches no other profile's file. A provider that cannot answer is a
// retryable state, never an erase. Plain-text mode is never switched on, and
// nothing here logs a secret, ciphertext or a path.
import { classify, type Classification, type SafeStorageProbe } from './classify.ts';
import { readCredential, removeProfile, validProfileId, writeCredential } from './files.ts';

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
  readonly log: (line: string) => void;
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

export interface Vault {
  classify(): Promise<Classification>;
  put(profileId: string, secret: string): Promise<PutResult>;
  get(profileId: string): Promise<GetResult>;
  remove(profileId: string): Promise<void>;
}

export function createVault(deps: VaultDeps): Vault {
  const state = (): Promise<Classification> =>
    classify({ storage: deps.storage, platform: deps.platform, isReady: deps.isReady });

  const event = (name: string, profileId: string): void => {
    deps.log(`vault: ${name} profile=${profileId}`);
  };

  return {
    classify: state,

    async put(profileId, secret) {
      requireId(profileId);
      const c = await state();
      if (c.kind !== 'protected') {
        event(c.kind, profileId);
        return c;
      }
      try {
        const sealed = await deps.storage.encryptStringAsync(secret);
        await writeCredential(deps.root, profileId, sealed);
      } catch {
        event('put-failed', profileId);
        return { kind: 'failed' };
      }
      return { kind: 'stored' };
    },

    async get(profileId) {
      requireId(profileId);
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
      if (read.kind === 'corrupt') {
        event('corrupt', profileId);
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
          await writeCredential(
            deps.root,
            profileId,
            await deps.storage.encryptStringAsync(opened.result),
          );
        } catch {
          // The secret is still good; the next get tries again.
          event('reencrypt-failed', profileId);
        }
      }
      return { kind: 'found', secret: opened.result };
    },

    async remove(profileId) {
      requireId(profileId);
      await removeProfile(deps.root, profileId);
    },
  };
}

function requireId(profileId: string): void {
  if (!validProfileId(profileId)) {
    throw new Error('invalid profile id');
  }
}
