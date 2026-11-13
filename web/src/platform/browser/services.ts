// The browser's Services: the clipboard, the Notification API, the page's own
// origin and the attributes of a link that leaves the page. Each looks the
// browser's API up when called, not when loaded, so a page without one (or a
// test that installs one) is read as it stands.
import type { NativeNotification, NotificationPermissionState, Services } from '../types';
import { push } from './push';

function supported(): boolean {
  return typeof window.Notification === 'function';
}

export const services: Services = {
  origin: () => window.location.origin,

  push,

  // No clipboard here (a page not served over https) throws or rejects; the
  // caller says so and offers the text to copy by hand.
  copyText: (text) => navigator.clipboard.writeText(text),

  notifications: {
    supported,
    permission(): NotificationPermissionState {
      return supported() ? window.Notification.permission : 'unsupported';
    },
    request: () => window.Notification.requestPermission(),
    show(title, body, tag): NativeNotification {
      return new window.Notification(title, { body, tag });
    },
  },

  externalLink: { target: '_blank', rel: 'noopener noreferrer' },

  // A custom-scheme link from a click: the browser asks the system for its
  // handler, or does nothing when there is none. '_self' so that no empty
  // tab is left behind, and the page itself stays where it is.
  openDesktopApp: (link) => {
    window.open(link, '_self');
  },

  browserVoice: true,
};
