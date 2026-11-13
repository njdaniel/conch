// The browser's background push (issue #1826): the service worker the build
// serves at /app/sw.js, and the Push API's subscription made through it.
// Each call looks the browser's API up when made, so a browser without one is
// read as it stands. The worker holds no credential and the page hands the
// subscription to conchd (api.ts), not this module.
import type { PushService, PushSubscriptionTarget } from '../types';

// The worker is a file of the build, beside index.html: its scope is the
// client's own, never the whole origin, so it can see no other page. It is
// vite.config's base, written out: this module is also compiled where Vite's
// import.meta.env is not typed (the desktop's e2e typecheck).
export const workerScope = '/app/';
const workerUrl = `${workerScope}sw.js`;

function supported(): boolean {
  return typeof navigator !== 'undefined' && 'serviceWorker' in navigator && typeof window.PushManager === 'function' && typeof window.Notification === 'function';
}

// fromBase64url decodes the server's key (base64url; padding is ignored).
export function fromBase64url(text: string): Uint8Array<ArrayBuffer> {
  const raw = atob(text.replace(/=+$/, '').replace(/-/g, '+').replace(/_/g, '/'));
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes;
}

// toBase64url is the form the registration takes: no padding.
export function toBase64url(bytes: ArrayBuffer | null): string {
  if (bytes === null) return '';
  let raw = '';
  for (const byte of new Uint8Array(bytes)) raw += String.fromCharCode(byte);
  return btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function sameKey(held: ArrayBuffer | null, wanted: Uint8Array): boolean {
  if (held === null) return false;
  const bytes = new Uint8Array(held);
  return bytes.length === wanted.length && bytes.every((byte, i) => byte === wanted[i]);
}

export const push: PushService = {
  supported,

  async subscribe(applicationServerKey): Promise<PushSubscriptionTarget> {
    const wanted = fromBase64url(applicationServerKey);
    await navigator.serviceWorker.register(workerUrl, { scope: workerScope, updateViaCache: 'none' });
    const ready = await navigator.serviceWorker.ready;
    let subscription = await ready.pushManager.getSubscription();
    // A subscription made with another key cannot be reused: the push service
    // would refuse the server's wake.
    if (subscription !== null && !sameKey(subscription.options.applicationServerKey, wanted)) {
      await subscription.unsubscribe();
      subscription = null;
    }
    subscription ??= await ready.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: wanted });
    const p256dh = toBase64url(subscription.getKey('p256dh'));
    const auth = toBase64url(subscription.getKey('auth'));
    if (p256dh === '' || auth === '') throw new Error('the subscription holds no keys');
    return { endpoint: subscription.endpoint, p256dh, auth };
  },

  async subscribed(): Promise<boolean> {
    if (!supported()) return false;
    const registration = await navigator.serviceWorker.getRegistration(workerScope);
    return (await registration?.pushManager.getSubscription()) != null;
  },

  async unsubscribe(): Promise<void> {
    if (!supported()) return;
    const registration = await navigator.serviceWorker.getRegistration(workerScope);
    const subscription = await registration?.pushManager.getSubscription();
    await subscription?.unsubscribe();
  },
};
