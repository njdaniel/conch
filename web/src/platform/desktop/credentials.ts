// The desktop renderer's CredentialStore. The renderer holds no credential:
// main keeps the bearer in its own memory (desktop/main/api/session.ts). What
// this store keeps is only a stand-in, so that the client's own logic (is
// someone signed in, which place, was it replaced) runs unchanged. The stand-in
// is a fixed word, not a secret, and means "main's session".
//
// The one real credential that passes through is a pasted one (Sign in with a
// credential). Main checks it and sets it aside; when the client keeps it
// (write), this store hands it to main to put in force (keepCredential) and
// keeps the stand-in, which is what the client pins. So the pasted credential
// is not held here, and an operator's that the person declined at the warning
// is never put in force: a reload does not find it.
//
// Nothing here touches web storage. Preferences are kept in memory for the
// session: saved profiles scope them in #945.
import type { CredentialStore, Held, Place } from '../types';
import { bridge } from './access';

export const STAND_IN = 'conch-desktop-session';

let held: Held | null = null;
const preferences = new Map<string, string>();
// The last hand-over of a pasted credential to main, which every call through
// the bridge waits for, so that none runs before it is in force.
let keeping: Promise<unknown> = Promise.resolve();

// isSessionToken says whether a token the client passes is main's session (the
// stand-in). Anything else is a candidate that main has not seen.
export function isSessionToken(token: string): boolean {
  return token === STAND_IN;
}

// kept resolves once a pasted credential the client kept is in force in main
// (or was refused there, and the next call is unauthorized).
export function kept(): Promise<unknown> {
  return keeping;
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
    if (token !== STAND_IN) {
      const b = bridge();
      if (b === undefined) return null;
      keeping = b.keepCredential(token).catch(() => false);
    }
    held = { token: STAND_IN, place };
    return held;
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
