// Is this machine's credential storage a protected one? (linux-desktop.md §4.1,
// windows-desktop.md §9.1; fail-closed until #922 LE7 measures the providers.)
//
// Input: the asynchronous availability and backend name of Electron's
// safeStorage, through the SafeStorageProbe interface, and the platform.
// Output: a Classification. This module owns the rule and nothing else: it
// reads no file, encrypts nothing and logs nothing.
//
// Electron documents no call that says which asynchronous provider is active,
// only the synchronous backend names, so on Linux storage counts as protected
// only when the async API reports available AND the backend names a real
// secret store. `basic_text` (a hardcoded key), `unknown` and anything not
// listed are session-only.

// The part of Electron's safeStorage that classification reads.
export interface SafeStorageProbe {
  isAsyncEncryptionAvailable(): Promise<boolean>;
  getSelectedStorageBackend(): string;
}

export type SessionOnlyReason =
  | 'unavailable' // the asynchronous API says encryption is not available
  | 'insecure-backend' // basic_text: encrypted with a hardcoded key
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
    return { kind: 'protected' };
  }
  if (backend === 'basic_text') {
    return { kind: 'sessionOnly', reason: 'insecure-backend' };
  }
  return { kind: 'sessionOnly', reason: 'unknown-backend' };
}
