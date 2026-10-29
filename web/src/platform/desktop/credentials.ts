// The desktop renderer's CredentialStore. The renderer holds no credential:
// main keeps the bearer in its own memory (desktop/main/api/session.ts). What
// this store keeps is only a stand-in, so that the client's own logic (is
// someone signed in, which place, was it replaced) runs unchanged. The stand-in
// is a fixed word, not a secret, and means "main's session".
//
// The one real credential that passes through is a pasted one (Sign in with a
// credential): the person typed it into this page, main takes it on that
// call, and the client holds it as it always has until it is dropped. It is
// never written anywhere by this store but this module's memory, which is
// emptied by drop.
//
// Nothing here touches web storage. Preferences are kept in memory for the
// session: saved profiles scope them in #945.
import type { CredentialStore, Held, Place } from '../types';

export const STAND_IN = 'conch-desktop-session';

let held: Held | null = null;
const preferences = new Map<string, string>();

// isSessionToken says whether a token the client passes is main's session: the
// stand-in, or the pasted credential this store is holding. Anything else is a
// candidate that main has not seen.
export function isSessionToken(token: string): boolean {
  return token === STAND_IN || (held !== null && held.token === token);
}

// signedIn records that main holds a session, for a client that has just
// loaded. A member's lasts (as the browser's lasts); an operator's and a
// guest's belong to the one window.
export function signedIn(lasting: boolean): void {
  held = { token: STAND_IN, place: lasting ? 'lasting' : 'tab' };
}

export const credentials: CredentialStore = {
  read: () => held,
  isStored: (candidate: Held) => held !== null && held.token === candidate.token && held.place === candidate.place,
  write(token: string, place: Place) {
    held = { token, place };
    return true;
  },
  drop(candidate: Held) {
    if (held !== null && held.token === candidate.token && held.place === candidate.place) held = null;
  },
  onLastingChange: () => () => undefined,
  readPreference: (key: string) => preferences.get(key) ?? null,
  writePreference(key: string, value: string) {
    preferences.set(key, value);
  },
};
