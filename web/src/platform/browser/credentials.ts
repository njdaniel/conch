// The browser's CredentialStore, and the only module that touches the
// credential's storage. Where the token lives depends on whose it is (issue
// #439, decision O1): a member's lasts, in localStorage, so it survives
// closing the browser and every tab of the origin shares it until it is signed
// out or stops working; an operator's and a temporary guest's stay with the
// one tab, in sessionStorage. The page has no third-party script
// (script-src 'self'), which is what makes local storage acceptable.
//
// The storage may be refused (blocked site data) at any call; none throws.
import type { CredentialStore, Held, Place } from '../types';

const tokenKey = 'conch.web.token';

// The tab's own credential comes first: an operator who continued in this tab
// is that operator here, whatever member has signed in elsewhere.
const places: readonly Place[] = ['tab', 'lasting'];

// storageAt may throw (blocked site data); every caller is inside a try.
function storageAt(place: Place): Storage {
  return place === 'tab' ? window.sessionStorage : window.localStorage;
}

export const credentials: CredentialStore = {
  read() {
    for (const place of places) {
      try {
        const token = storageAt(place).getItem(tokenKey);
        if (token !== null) return { token, place };
      } catch {
        // This storage is refused; the other may still hold one.
      }
    }
    return null;
  },

  isStored(held: Held) {
    try {
      return storageAt(held.place).getItem(tokenKey) === held.token;
    } catch {
      return false;
    }
  },

  write(token, place) {
    try {
      storageAt(place).setItem(tokenKey, token);
      return { token, place };
    } catch {
      return null;
    }
  },

  // Another tab may have signed in again since this one read the credential,
  // and a late 401 for the old one must not sign the new one out.
  drop(held) {
    try {
      const storage = storageAt(held.place);
      if (storage.getItem(tokenKey) === held.token) storage.removeItem(tokenKey);
    } catch {
      // Nothing stored that could be removed.
    }
  },

  // The browser fires the storage event only in the other tabs, never in the
  // one that made the change.
  onLastingChange(callback) {
    const listener = (event: StorageEvent) => {
      // key null is the whole storage cleared.
      if (event.key !== null && event.key !== tokenKey) return;
      try {
        if (event.storageArea !== window.localStorage) return;
      } catch {
        return;
      }
      callback();
    };
    window.addEventListener('storage', listener);
    return () => {
      window.removeEventListener('storage', listener);
    };
  },

  readPreference(key) {
    try {
      return storageAt('lasting').getItem(key);
    } catch {
      return null;
    }
  },

  writePreference(key, value) {
    try {
      storageAt('lasting').setItem(key, value);
    } catch {
      // Storage can be unavailable; the in-memory preference still applies.
    }
  },
};
