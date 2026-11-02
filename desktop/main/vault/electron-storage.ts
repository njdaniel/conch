// The one place Electron's safeStorage becomes the vault's VaultStorage
// (issue #945). It is the only file under vault/ that imports 'electron'; a
// source test says so. Call it only after `ready`: the vault asks
// `isReady: () => app.isReady()` before every use and answers "temporarily
// unavailable" until then.
import type { SafeStorage } from 'electron';

import type { VaultStorage } from './vault.ts';

export function electronStorage(safeStorage: SafeStorage): VaultStorage {
  return {
    isAsyncEncryptionAvailable: () => safeStorage.isAsyncEncryptionAvailable(),
    getSelectedStorageBackend: () => safeStorage.getSelectedStorageBackend(),
    encryptStringAsync: (plainText) => safeStorage.encryptStringAsync(plainText),
    decryptStringAsync: (encrypted) => safeStorage.decryptStringAsync(encrypted),
  };
}
