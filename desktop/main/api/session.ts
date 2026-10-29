// The one signed-in identity of this window, in main's memory and nowhere else
// (issue #942: session-only; the vault of #944 is wired by #945). Input: a
// server origin and, after a sign-in, a bearer credential. Output: a capture
// of the identity for each request, and a verdict, when the answer comes back,
// on whether it is still the identity that asked.
//
// The generation counts identities: it increases whenever the server address
// changes, a credential is adopted, or the credential is forgotten, and each
// increase aborts every request still in flight. A response for an older
// generation is dropped in main (linux-desktop.md §4: "every async completion
// carries a generation check").
//
// The token is a private field and has no getter that returns it other than
// capture(), which only the request executor calls.
import { randomBytes, timingSafeEqual } from 'node:crypto';

export interface Capture {
  readonly generation: number;
  readonly origin: string | null;
  readonly token: string | null;
  readonly signal: AbortSignal;
}

// A pending operator sign-in: the challenge a right password earned, which the
// renderer sees only as a handle.
interface PendingChallenge {
  readonly challenge: string;
  readonly expiresAt: number;
}

const MAX_PENDING = 4;

// A pasted credential that whoami accepted but that is not yet in force: it is
// put in force only when the page keeps it (keepCredential), which an operator's
// waits for until the person has passed the operator warning. Declined, or never
// kept, it does nothing and lapses.
interface Pasted {
  readonly token: string;
  readonly expiresAt: number;
}

export class Session {
  #generation = 0;
  #origin: string | null = null;
  #token: string | null = null;
  #controller = new AbortController();
  readonly #challenges = new Map<string, PendingChallenge>();
  #pasted: Pasted | null = null;

  get generation(): number {
    return this.#generation;
  }

  get origin(): string | null {
    return this.#origin;
  }

  get signedIn(): boolean {
    return this.#token !== null;
  }

  // bump ends the current identity: in-flight requests are aborted and the
  // challenges and the pasted credential of the old one are forgotten.
  #bump(): void {
    this.#generation++;
    this.#controller.abort();
    this.#controller = new AbortController();
    this.#challenges.clear();
    this.#pasted = null;
  }

  // setOrigin selects the server. Changing it is a change of identity; setting
  // the same one again is not.
  setOrigin(origin: string): void {
    if (origin === this.#origin) return;
    this.#bump();
    this.#origin = origin;
    this.#token = null;
  }

  // adopt keeps the credential a sign-in gave, as a new identity.
  adopt(token: string): void {
    this.#bump();
    this.#token = token;
  }

  // forget drops the credential and ends everything in flight. It returns the
  // credential it dropped, for the one revocation sign-out sends.
  forget(): string | null {
    const token = this.#token;
    this.#bump();
    this.#token = null;
    return token;
  }

  // holdPasted sets a checked, pasted credential aside, not in force. A later
  // paste replaces it.
  holdPasted(token: string, expiresAt: number): void {
    this.#pasted = { token, expiresAt };
  }

  // takePasted says whether token is the credential holdPasted set aside, still
  // within its time, and consumes it either way it matches. The caller adopts it.
  takePasted(token: string, now: number): boolean {
    const pasted = this.#pasted;
    if (pasted === null) return false;
    const a = Buffer.from(pasted.token);
    const b = Buffer.from(token);
    if (a.length !== b.length || !timingSafeEqual(a, b)) return false;
    this.#pasted = null;
    return now < pasted.expiresAt;
  }

  capture(): Capture {
    return { generation: this.#generation, origin: this.#origin, token: this.#token, signal: this.#controller.signal };
  }

  // current says whether a capture is still the identity in force.
  current(capture: Capture): boolean {
    return capture.generation === this.#generation;
  }

  // keepChallenge holds a challenge and returns the handle the renderer
  // gets in its place.
  keepChallenge(challenge: string, expiresAt: number): string {
    while (this.#challenges.size >= MAX_PENDING) {
      const oldest = this.#challenges.keys().next();
      if (oldest.done === true) break;
      this.#challenges.delete(oldest.value);
    }
    const handle = `challenge-${randomBytes(16).toString('hex')}`;
    this.#challenges.set(handle, { challenge, expiresAt });
    return handle;
  }

  // challenge is the real challenge for a handle, while it lasts.
  challenge(handle: string, now: number): string | null {
    const pending = this.#challenges.get(handle);
    if (pending === undefined) return null;
    if (now >= pending.expiresAt) {
      this.#challenges.delete(handle);
      return null;
    }
    return pending.challenge;
  }

  dropChallenge(handle: string): void {
    this.#challenges.delete(handle);
  }
}
