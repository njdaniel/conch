import { describe, expect, it } from 'vitest';

import { classify, linuxSeal, PROBE_TEXT, type Classification } from '../../main/vault/classify.ts';

interface Case {
  readonly name: string;
  readonly platform: NodeJS.Platform;
  readonly ready?: boolean;
  readonly available: boolean | Error;
  readonly backend: string | Error;
  // The three-byte mark the probe comes back with (default v11, the keyring's
  // key), or an error the probe throws.
  readonly probe?: string | Error;
  readonly want: Classification;
}

const sessionOnly = (reason: 'unavailable' | 'insecure-backend' | 'unknown-backend' | 'no-keyring'): Classification => ({
  kind: 'sessionOnly',
  reason,
});

const cases: readonly Case[] = [
  // Every documented Linux backend, with the async API available.
  { name: 'gnome_libsecret', platform: 'linux', available: true, backend: 'gnome_libsecret', want: { kind: 'protected' } },
  { name: 'kwallet', platform: 'linux', available: true, backend: 'kwallet', want: { kind: 'protected' } },
  { name: 'kwallet5', platform: 'linux', available: true, backend: 'kwallet5', want: { kind: 'protected' } },
  { name: 'kwallet6', platform: 'linux', available: true, backend: 'kwallet6', want: { kind: 'protected' } },
  { name: 'basic_text', platform: 'linux', available: true, backend: 'basic_text', want: sessionOnly('insecure-backend') },
  { name: 'unknown', platform: 'linux', available: true, backend: 'unknown', want: sessionOnly('unknown-backend') },
  { name: 'a name this build has never heard of', platform: 'linux', available: true, backend: 'kwallet7', want: sessionOnly('unknown-backend') },
  { name: 'an empty name', platform: 'linux', available: true, backend: '', want: sessionOnly('unknown-backend') },
  // A real backend's name is not enough (#2077): the probe must come back
  // sealed with the keyring's key. A GNOME or COSMIC desktop with no Secret
  // Service, or a locked one, names gnome_libsecret but seals with v10.
  { name: 'libsecret named, but sealed with the built-in key (no keyring answered)', platform: 'linux', available: true, backend: 'gnome_libsecret', probe: 'v10', want: sessionOnly('no-keyring') },
  { name: 'kwallet named, but sealed with the built-in key', platform: 'linux', available: true, backend: 'kwallet', probe: 'v10', want: sessionOnly('no-keyring') },
  { name: 'libsecret named, sealed with a mark this build does not know', platform: 'linux', available: true, backend: 'gnome_libsecret', probe: 'v12', want: sessionOnly('unknown-backend') },
  { name: 'libsecret named, the probe throws', platform: 'linux', available: true, backend: 'gnome_libsecret', probe: new Error('prompt dismissed'), want: { kind: 'temporarilyUnavailable' } },
  // basic_text stays session-only even when the async API seals with the
  // keyring (measured on sway): the design's rule, unchanged.
  { name: 'basic_text, even if a keyring sealed the probe', platform: 'linux', available: true, backend: 'basic_text', probe: 'v11', want: sessionOnly('insecure-backend') },
  // A real backend does not rescue an unavailable async API.
  { name: 'libsecret but async unavailable', platform: 'linux', available: false, backend: 'gnome_libsecret', want: sessionOnly('unavailable') },
  { name: 'basic_text and unavailable', platform: 'linux', available: false, backend: 'basic_text', want: sessionOnly('unavailable') },
  // Errors are retryable, never a decision.
  { name: 'availability throws', platform: 'linux', available: new Error('dbus'), backend: 'gnome_libsecret', want: { kind: 'temporarilyUnavailable' } },
  { name: 'backend throws', platform: 'linux', available: true, backend: new Error('boom'), want: { kind: 'temporarilyUnavailable' } },
  { name: 'app not ready', platform: 'linux', ready: false, available: true, backend: 'gnome_libsecret', want: { kind: 'temporarilyUnavailable' } },
  // Other platforms: one provider each, so availability decides.
  { name: 'win32 available', platform: 'win32', available: true, backend: new Error('not asked'), want: { kind: 'protected' } },
  // Windows and macOS mark their own (protected) ciphertexts v10 too: the
  // Linux probe rule is Linux's alone.
  { name: 'win32 is not probed', platform: 'win32', available: true, backend: new Error('not asked'), probe: new Error('not asked'), want: { kind: 'protected' } },
  { name: 'darwin available', platform: 'darwin', available: true, backend: new Error('not asked'), want: { kind: 'protected' } },
  { name: 'freebsd available is not assumed protected', platform: 'freebsd', available: true, backend: 'gnome_libsecret', want: sessionOnly('unknown-backend') },
  { name: 'sunos available', platform: 'sunos', available: true, backend: 'unknown', want: sessionOnly('unknown-backend') },
  { name: 'win32 unavailable', platform: 'win32', available: false, backend: 'unknown', want: sessionOnly('unavailable') },
];

describe('classify', () => {
  for (const c of cases) {
    it(`${c.platform}: ${c.name}`, async () => {
      const got = await classify({
        platform: c.platform,
        isReady: () => c.ready ?? true,
        storage: {
          isAsyncEncryptionAvailable: () =>
            c.available instanceof Error ? Promise.reject(c.available) : Promise.resolve(c.available),
          getSelectedStorageBackend: () => {
            if (c.backend instanceof Error) {
              throw c.backend;
            }
            return c.backend;
          },
          encryptStringAsync: (plain) => {
            expect(plain).toBe(PROBE_TEXT);
            const probe = c.probe ?? 'v11';
            return probe instanceof Error ? Promise.reject(probe) : Promise.resolve(Buffer.from(`${probe}sealed`));
          },
        },
      });
      expect(got).toEqual(c.want);
    });
  }
});

describe('linuxSeal', () => {
  it.each([
    ['v11 is the keyring key', 'v11abc', 'keyring'],
    ['v10 is the built-in key', 'v10abc', 'built-in'],
    ['another mark is unknown', 'v12abc', 'unknown'],
    ['too short is unknown', 'v1', 'unknown'],
    ['empty is unknown', '', 'unknown'],
  ] as const)('%s', (_name, bytes, want) => {
    expect(linuxSeal(Buffer.from(bytes, 'latin1'))).toBe(want);
  });
});
