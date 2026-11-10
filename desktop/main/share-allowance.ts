// The share allowance (issue #1580, D4-03 of #772; the Share button's cold
// path, #1582). Input: the page's shareArm() call, which has already taken the
// person's click or key (ipc.ts), and the two later questions that follow from
// the one click: the companion's grant, and the screen capture's display-media
// request (display-capture.ts). Output: yes or no to each, once. Owns: the one
// allowance, if there is one.
//
// Why it exists. One click on Share is followed by two things that each need
// the person's act: the companion's grant (companionGrant, a round trip to
// conchd) and then getDisplayMedia (the picker). The act is one-shot, so on a
// cold start the grant would spend it and the picker would find nothing. So
// the page calls shareArm() first, synchronously at the start of the click
// handler: that takes the act, and arms an allowance with two uses, one for
// the grant and one for the capture. Nothing else arms capture: a raw click
// or an Enter that sends a chat message no longer does (S3 of #1621).
//
// The allowance is:
//  - for one web contents and one document generation: a reload, a crash or a
//    window that is not the one armed finds nothing;
//  - two uses, one of each kind, each used up when taken: the grant use is
//    good for SHARE_ARM_MS from the arm, the capture use for SHARE_CAPTURE_MS
//    from the later of the arm and the grant being issued from it;
//  - alone: arming again replaces it;
//  - cleared by clear(), which app.ts calls on every companion end (leave,
//    helper, renewal, sign-out, switch, quit, crash, reload, lock, suspend and
//    the page's own companionEnd).
// The allowance is not a grant: the capture still needs a live companion
// grant, the portal, and every other condition of display-capture.ts.
//
// What this is, and is not (S1 of #1621). The 10 s capture window keeps an
// honest page from arming capture by accident and leaving it lying about. It
// is not a defence against a compromised page: such a page can call
// shareArm() within 1 s of any act (the act's own freshness window), and
// arming is all it needs. The consent is the OS portal's picker, which the
// page cannot click through; the allowance only stops an honest page.

// How long after shareArm() the grant use may still be taken: the cold path's
// start, before the grant's round trip to conchd.
export const SHARE_ARM_MS = 10_000;

// How long the capture use is good for, counted from the later of the arm and
// the grant being issued from this allowance (confirmGrant). So the warm path
// (a grant already live) has this long from the arm, and the cold path this
// long after the grant round trip. The person choosing in the portal's picker
// is not counted (the capture use is taken when the pick starts). A
// compromised page can still arm within 1 s of any act: the OS portal picker
// is the consent, and this only stops an honest page arming by accident.
// 10 s, not 3 (#1632): on the cold path the companion's LiveKit connect and
// the claim run between the grant and the pick, and over a TURN/TCP fallback
// they can take longer than 3 s, which made sharing impossible.
export const SHARE_CAPTURE_MS = 10_000;

interface Held {
  readonly contents: object;
  readonly document: number;
  readonly at: number;
  grant: boolean;
  capture: boolean;
  // When main confirmed the grant issued from this allowance, or null.
  issuedAt: number | null;
}

export interface ShareAllowanceDeps {
  readonly now: () => number;
  // The owned window's current document generation.
  readonly generation: () => number;
}

export class ShareAllowance {
  readonly #deps: ShareAllowanceDeps;
  #held: Held | null = null;

  constructor(deps: ShareAllowanceDeps) {
    this.#deps = deps;
  }

  // arm replaces the allowance with a fresh one for contents and the current
  // document. The caller has checked who is calling and taken the act.
  arm(contents: object | null): boolean {
    if (contents === null) return false;
    this.#held = { contents, document: this.#deps.generation(), at: this.#deps.now(), grant: true, capture: true, issuedAt: null };
    return true;
  }

  clear(): void {
    this.#held = null;
  }

  // #live is the allowance for contents if it is still the same contents and
  // the same document, and the clock has not gone backwards. One for another
  // document is dropped. Each use checks its own time.
  #live(contents: object | null): Held | null {
    const held = this.#held;
    if (held === null) return null;
    if (held.document !== this.#deps.generation() || this.#deps.now() < held.at) {
      this.#held = null;
      return null;
    }
    return contents !== null && held.contents === contents ? held : null;
  }

  // takeGrant uses up the grant use: for companionGrant. Good for
  // SHARE_ARM_MS from the arm.
  takeGrant(contents: object | null): boolean {
    const held = this.#live(contents);
    if (held === null || !held.grant || this.#deps.now() - held.at > SHARE_ARM_MS) return false;
    held.grant = false;
    return true;
  }

  // confirmGrant records that main issued the companion grant that took the
  // grant use, in this document: the capture window runs from here. Nothing
  // happens if the grant use was not spent (a warm path) or the allowance is
  // another's. It is the only way the window moves later than the arm.
  confirmGrant(contents: object | null, document: number): void {
    const held = this.#live(contents);
    if (held === null || held.grant || held.issuedAt !== null || held.document !== document) return;
    held.issuedAt = this.#deps.now();
  }

  // #captureGood says whether the capture use is there and inside
  // SHARE_CAPTURE_MS of the later of the arm and the grant's issue.
  #captureGood(held: Held | null): held is Held {
    if (held === null || !held.capture) return false;
    const since = Math.max(held.at, held.issuedAt ?? held.at);
    const age = this.#deps.now() - since;
    return age >= 0 && age <= SHARE_CAPTURE_MS;
  }

  // peekCapture says whether the capture use is there, using nothing up: for
  // the permission request that comes ahead of the display-media request.
  peekCapture(contents: object | null): boolean {
    return this.#captureGood(this.#live(contents));
  }

  // takeCapture uses up the capture use: for the display-media request, as
  // the pick starts.
  takeCapture(contents: object | null): boolean {
    const held = this.#live(contents);
    if (!this.#captureGood(held)) return false;
    held.capture = false;
    return true;
  }
}
