// The desktop shell's platform, selected only by vite build --mode desktop.
// Every request goes through window.conchDesktop to the main process, which
// holds the bearer (issue #942, D2-3); the renderer holds a stand-in. The
// browser build never contains this directory.
//
// Live feeds are subscriptions that main opens and holds (#943, ./sockets).
// Attachments, links and the clipboard go through main too (#947): an upload
// or a download is a call of the bridge, a click on a link that opens a new
// window is main's to validate and hand to the OS browser, and the clipboard
// is written with the page's own API, which main permits for the owned window
// right after the person's own input. Not yet here: notifications (#773), and the operations whose answers hold a credential the renderer may
// not see (an operator's re-authentication, a password change, an invite
// code, a device link, which main will carry in later children).
import type { Platform } from '../types';
import { bridge } from './access';
import { credentials } from './credentials';
import { http } from './http';
import { bridge } from './access';
import { DesktopShell } from './Shell';
import { sockets } from './sockets';
import { serverOrigin } from './state';

export const platform: Platform = {
  name: 'desktop',
  http,
  sockets,
  credentials,
  services: {
    origin: serverOrigin,
    copyText: (text) => navigator.clipboard.writeText(text),
    saveFile: async (id) => {
      const b = bridge();
      if (b === undefined) return 'failed';
      try {
        return await b.saveFile(id);
      } catch {
        return 'failed';
      }
    },
    notifications: {
      supported: () => false,
      permission: () => 'unsupported',
      request: () => Promise.resolve('unsupported'),
      show() {
        throw new TypeError('notifications are not available in the desktop app yet');
      },
    },
    // A new window is never made; main turns the click into the OS browser.
    externalLink: { target: '_blank', rel: 'noopener noreferrer' },
    browserVoice: false,
    appVersion: () =>
      (bridge()?.profiles() ?? Promise.resolve(null)).then(
        (view) => view?.compat.desktop ?? null,
        () => null,
      ),
  },
  Shell: DesktopShell,
};
