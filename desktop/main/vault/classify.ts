// Is this machine's credential storage a protected one? (linux-desktop.md §4.1,
// windows-desktop.md §9.1; fail-closed until #922 LE7 measures the providers.)
//
// Input: the asynchronous availability and backend name of Electron's
// safeStorage, and on Linux one probe encryption of a fixed, public string,
// through the SafeStorageProbe interface, and the platform. Output: a
// Classification. This module owns the rule and nothing else: it reads no
// file, encrypts no secret and logs nothing.
//
// Electron documents no call that says which asynchronous provider is active,
// only the synchronous backend names, so on Linux storage counts as protected
// only when the async API reports available AND the backend names a real
// secret store AND a probe comes back sealed with a key from that store.
// `basic_text` (a hardcoded key), `unknown` and anything not listed are
// session-only.
//
// Why the probe (issue #2077, measured with Electron 44.10.0): the backend
// name follows the desktop's name, not what answered. On a GNOME or COSMIC
// desktop with no Secret Service, or with a locked keyring and no prompter,
// the backend still says `gnome_libsecret` and the async API still says
// available, but the ciphertext starts `v10`: Chromium's built-in key, which
// protects nothing (the same as `basic_text`). Only `v11` means the key came
// from the keyring (libsecret or KWallet). Any other prefix is not one this
// build has measured, and fails closed.

// The part of Electron's safeStorage that classification reads.
export interface SafeStorageProbe {
  isAsyncEncryptionAvailable(): Promise<boolean>;
  getSelectedStorageBackend(): string;
  encryptStringAsync(plainText: string): Promise<Buffer>;
}

// The fixed, public text the Linux probe seals. It is not a secret, and the
// ciphertext is only looked at, never kept.
export const PROBE_TEXT = 'conch-desktop storage probe';

// How Chromium marks a ciphertext on Linux: the first three bytes.
const LINUX_KEYRING_PREFIX = 'v11';
const LINUX_BUILT_IN_PREFIX = 'v10';

// linuxSeal says which key sealed a Linux ciphertext: the keyring's, the
// built-in one, or one this build does not know.
export function linuxSeal(ciphertext: Uint8Array): 'keyring' | 'built-in' | 'unknown' {
  const prefix = Buffer.from(ciphertext.subarray(0, 3)).toString('latin1');
  if (prefix === LINUX_KEYRING_PREFIX) return 'keyring';
  if (prefix === LINUX_BUILT_IN_PREFIX) return 'built-in';
  return 'unknown';
}

export type SessionOnlyReason =
  | 'unavailable' // the asynchronous API says encryption is not available
  | 'insecure-backend' // basic_text: encrypted with a hardcoded key
  | 'no-keyring' // a real store is named, but no keyring gave a key (none, locked or denied)
  | 'unknown-backend' // unknown, or a name this build does not recognise
  | 'unsafe-root'; // the data directory is not private to this user

export type Classification =
  | { readonly kind: 'protected' }
  | { readonly kind: 'sessionOnly'; readonly reason: SessionOnlyReason }
  // A question that failed to answer (a thrown error, or the app not ready):
  // retryable, and never a reason to erase or to store.
  | { readonly kind: 'temporarilyUnavailable' };

// The Linux backends that are real secret stores.
const PROTECTED_LINUX_BACKENDS: ReadonlySet<string> = new Set([
  'gnome_libsecret',
  'kwallet',
  'kwallet5',
  'kwallet6',
]);

export interface ClassifyDeps {
  readonly storage: SafeStorageProbe;
  readonly platform: NodeJS.Platform;
  readonly isReady: () => boolean;
}

export async function classify(deps: ClassifyDeps): Promise<Classification> {
  if (!deps.isReady()) {
    return { kind: 'temporarilyUnavailable' };
  }
  let available: boolean;
  try {
    available = await deps.storage.isAsyncEncryptionAvailable();
  } catch {
    return { kind: 'temporarilyUnavailable' };
  }
  if (!available) {
    return { kind: 'sessionOnly', reason: 'unavailable' };
  }
  if (deps.platform === 'win32' || deps.platform === 'darwin') {
    // Windows (DPAPI) and macOS (Keychain) have one provider each. DPAPI
    // protects from other users, not from other apps of the same user; the UI
    // says so in those words (windows-desktop.md §9.1).
    return { kind: 'protected' };
  }
  if (deps.platform !== 'linux') {
    // Any other platform is not one this build has measured: fail closed.
    return { kind: 'sessionOnly', reason: 'unknown-backend' };
  }
  let backend: string;
  try {
    backend = deps.storage.getSelectedStorageBackend();
  } catch {
    return { kind: 'temporarilyUnavailable' };
  }
  if (PROTECTED_LINUX_BACKENDS.has(backend)) {
    let probe: Buffer;
    try {
      probe = await deps.storage.encryptStringAsync(PROBE_TEXT);
    } catch {
      return { kind: 'temporarilyUnavailable' };
    }
    switch (linuxSeal(probe)) {
      case 'keyring':
        return { kind: 'protected' };
      case 'built-in':
        return { kind: 'sessionOnly', reason: 'no-keyring' };
      case 'unknown':
        return { kind: 'sessionOnly', reason: 'unknown-backend' };
    }
  }
  if (backend === 'basic_text') {
    return { kind: 'sessionOnly', reason: 'insecure-backend' };
  }
  return { kind: 'sessionOnly', reason: 'unknown-backend' };
}
