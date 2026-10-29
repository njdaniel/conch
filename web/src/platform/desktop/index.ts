// The desktop shell's platform, selected only by vite build --mode desktop. In
// this issue (D2-2) it is a stub: every operation answers as a server that
// cannot be reached, the credential is kept nowhere, and no service is
// offered. D2-3 and later replace it with the typed client of
// window.conchDesktop (./bridge). The browser build never contains this file.
import type { CredentialStore, FeedConnection, Platform } from '../types';

const unreachable = (): Promise<never> => Promise.reject(new TypeError('conchd cannot be reached from the desktop shell yet'));

const credentials: CredentialStore = {
  read: () => null,
  isStored: () => false,
  write: () => false,
  drop: () => undefined,
  onLastingChange: () => () => undefined,
  readPreference: () => null,
  writePreference: () => undefined,
};

export const platform: Platform = {
  name: 'desktop',
  http: { get: unreachable, send: unreachable, open: unreachable, health: unreachable, upload: unreachable, download: unreachable },
  sockets: {
    open(): FeedConnection {
      throw new TypeError('conchd cannot be reached from the desktop shell yet');
    },
  },
  credentials,
  services: {
    origin: () => '',
    copyText: unreachable,
    notifications: {
      supported: () => false,
      permission: () => 'unsupported',
      request: () => Promise.resolve('unsupported'),
      show() {
        throw new TypeError('no notifications in the desktop shell yet');
      },
    },
    externalLink: { rel: 'noopener noreferrer' },
    browserVoice: false,
  },
};
