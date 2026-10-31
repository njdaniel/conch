// The desktop shell's platform, selected only by vite build --mode desktop.
// Every request goes through window.conchDesktop to the main process, which
// holds the bearer (issue #942, D2-3); the renderer holds a stand-in. The
// browser build never contains this directory.
//
// Live feeds are subscriptions that main opens and holds (#943, ./sockets).
// Not yet here: the clipboard, notifications and external links (#947, #773), attachments
// (#947), and the operations whose answers hold a credential the renderer may
// not see (an operator's re-authentication, a password change, an invite
// code, a device link, which main will carry in later children).
import type { Platform } from '../types';
import { credentials } from './credentials';
import { http } from './http';
import { DesktopShell } from './Shell';
import { sockets } from './sockets';
import { serverOrigin } from './state';

const later = (what: string) => (): Promise<never> => Promise.reject(new TypeError(`${what} is not available in the desktop app yet`));

export const platform: Platform = {
  name: 'desktop',
  http,
  sockets,
  credentials,
  services: {
    origin: serverOrigin,
    copyText: later('Copying'),
    notifications: {
      supported: () => false,
      permission: () => 'unsupported',
      request: () => Promise.resolve('unsupported'),
      show() {
        throw new TypeError('notifications are not available in the desktop app yet');
      },
    },
    externalLink: { rel: 'noopener noreferrer' },
    browserVoice: false,
  },
  Shell: DesktopShell,
};
