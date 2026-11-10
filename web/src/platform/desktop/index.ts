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
// right after the person's own input. Notifications (#1590) are fixed-field,
// activation-owned native alerts whose clicks return only a tag. Not yet here:
// the operations whose answers hold a credential the renderer may not see (an
// operator's re-authentication, a password change, an invite code, a device
// link, which main will carry in later children).
import type { NativeNotification, NotificationPermissionState, Platform } from '../types';
import { bridge } from './access';
import { credentials } from './credentials';
import { http } from './http';
import { DesktopShell } from './Shell';
import { sockets } from './sockets';
import { nativeVoice } from './voice';
import { serverOrigin } from './state';
import type { ConchDesktopBridge } from './bridge';

const nativeByTag = new Map<string, NativeNotification>();
const listening = new WeakSet<ConchDesktopBridge>();
const supportByBridge = new WeakMap<ConchDesktopBridge, boolean>();

function notificationPermission(): NotificationPermissionState {
  const b = bridge();
  if (b === undefined) return 'unsupported';
  const cached = supportByBridge.get(b);
  if (cached !== undefined) return cached ? 'granted' : 'unsupported';
  let supported = false;
  try {
    supported = b.notificationSupported();
  } catch {
    // A refused or malformed answer is unsupported for this document.
  }
  supportByBridge.set(b, supported);
  return supported ? 'granted' : 'unsupported';
}

function showNotification(title: string, body: string, tag: string, channel: string): NativeNotification {
  const b = bridge();
  if (b === undefined) throw new TypeError('the desktop bridge is not available');
  const notification: NativeNotification = {
    onclick: null,
    close: () => {
      if (nativeByTag.get(tag) === notification) nativeByTag.delete(tag);
      void b.notifyClose({ tag }).catch(() => undefined);
    },
  };
  nativeByTag.set(tag, notification);
  if (!listening.has(b)) {
    listening.add(b);
    b.onNotification((event) => {
      const target = nativeByTag.get(event.tag);
      if (target !== undefined && target.onclick !== null) target.onclick.call(target, new Event('click'));
    });
  }
  void b.notify({ tag, title, body, channel }).then((shown) => {
    if (!shown && nativeByTag.get(tag) === notification) nativeByTag.delete(tag);
  }, () => {
    if (nativeByTag.get(tag) === notification) nativeByTag.delete(tag);
  });
  return notification;
}

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
      supported: () => notificationPermission() === 'granted',
      permission: notificationPermission,
      request: () => Promise.resolve(notificationPermission()),
      show: showNotification,
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
  nativeVoice,
};
