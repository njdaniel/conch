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
//  - good for SHARE_ARM_MS, then gone;
//  - two uses, one of each kind, each used up when taken;
//  - alone: arming again replaces it;
//  - cleared by clear(), which app.ts calls on every companion end (leave,
//    helper, renewal, sign-out, switch, quit, crash, reload, lock, suspend and
//    the page's own companionEnd).
// The allowance is not a grant: the capture still needs a live companion
// grant, the portal, and every other condition of display-capture.ts.

// How long after shareArm() the grant and the capture may still follow: the
// grant's round trip to conchd, and the person choosing in the portal's
// picker is not counted (the capture use is taken when the pick starts).
export const SHARE_ARM_MS = 10_000;

interface Held {
  readonly contents: object;
  readonly document: number;
  readonly at: number;
  grant: boolean;
  capture: boolean;
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
    this.#held = { contents, document: this.#deps.generation(), at: this.#deps.now(), grant: true, capture: true };
    return true;
  }

  clear(): void {
    this.#held = null;
  }

  // #live is the allowance for contents if it is still good: the same
  // contents, the same document, and inside SHARE_ARM_MS. One that is not is
  // dropped.
  #live(contents: object | null): Held | null {
    const held = this.#held;
    if (held === null) return null;
    const age = this.#deps.now() - held.at;
    if (age < 0 || age > SHARE_ARM_MS || held.document !== this.#deps.generation()) {
      this.#held = null;
      return null;
    }
    return contents !== null && held.contents === contents ? held : null;
  }

  // takeGrant uses up the grant use: for companionGrant.
  takeGrant(contents: object | null): boolean {
    const held = this.#live(contents);
    if (held === null || !held.grant) return false;
    held.grant = false;
    return true;
  }

  // peekCapture says whether the capture use is there, using nothing up: for
  // the permission request that comes ahead of the display-media request.
  peekCapture(contents: object | null): boolean {
    return this.#live(contents)?.capture === true;
  }

  // takeCapture uses up the capture use: for the display-media request, as
  // the pick starts.
  takeCapture(contents: object | null): boolean {
    const held = this.#live(contents);
    if (held === null || !held.capture) return false;
    held.capture = false;
    return true;
  }
}
