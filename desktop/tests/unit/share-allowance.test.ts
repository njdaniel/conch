import { describe, expect, it } from 'vitest';

import { ShareAllowance, SHARE_ARM_MS, SHARE_CAPTURE_MS } from '../../main/share-allowance.ts';

// Part of the capture window, and the rest of it: a confirmation made PART in
// must not restart the window, so PART + REST + 1 is just past its end.
const PART = Math.floor(SHARE_CAPTURE_MS / 2);
const REST = SHARE_CAPTURE_MS - PART;

function make(): { share: ShareAllowance; clock: { now: number }; document: { n: number } } {
  const clock = { now: 5_000 };
  const document = { n: 1 };
  return { share: new ShareAllowance({ now: () => clock.now, generation: () => document.n }), clock, document };
}

const win = {};
const other = {};

describe('the share allowance', () => {
  it('arms two uses, one of each kind, each used up when taken', () => {
    const { share } = make();
    expect(share.arm(win)).toBe(true);
    expect(share.peekCapture(win)).toBe(true);
    expect(share.peekCapture(win)).toBe(true);
    expect(share.takeGrant(win)).toBe(true);
    expect(share.takeGrant(win)).toBe(false);
    expect(share.takeCapture(win)).toBe(true);
    expect(share.takeCapture(win)).toBe(false);
    expect(share.peekCapture(win)).toBe(false);
  });

  it('taking one kind leaves the other', () => {
    const { share } = make();
    share.arm(win);
    expect(share.takeCapture(win)).toBe(true);
    expect(share.takeGrant(win)).toBe(true);
  });

  it('arms nothing for no contents, and answers nothing without arming', () => {
    const { share } = make();
    expect(share.arm(null)).toBe(false);
    expect(share.takeGrant(win)).toBe(false);
    expect(share.takeCapture(win)).toBe(false);
    expect(share.takeGrant(null)).toBe(false);
  });

  it.each([
    ['just inside the window', SHARE_ARM_MS, true],
    ['just past it', SHARE_ARM_MS + 1, false],
  ])('the grant use expires from the arm: %s', (_name, wait, good) => {
    const { share, clock } = make();
    share.arm(win);
    clock.now += wait;
    expect(share.takeGrant(win)).toBe(good);
  });

  it.each([
    ['at the arm', 0, true],
    ['just inside the window', SHARE_CAPTURE_MS, true],
    ['just past it', SHARE_CAPTURE_MS + 1, false],
  ])('the warm path: the capture use expires from the arm, no grant taken: %s', (_name, wait, good) => {
    const { share, clock } = make();
    share.arm(win);
    clock.now += wait;
    expect(share.peekCapture(win)).toBe(good);
    expect(share.takeCapture(win)).toBe(good);
  });

  it.each([
    ['just inside the window', SHARE_CAPTURE_MS, true],
    ['just past it', SHARE_CAPTURE_MS + 1, false],
  ])('the cold path: the capture window runs from the grant being issued: %s', (_name, wait, good) => {
    const { share, clock, document } = make();
    share.arm(win);
    clock.now += 9_000; // a slow round trip, inside SHARE_ARM_MS
    expect(share.takeGrant(win)).toBe(true);
    share.confirmGrant(win, document.n);
    // The window now runs from the confirmation, not the arm: at the edge it
    // is 9 s + SHARE_CAPTURE_MS after the arm, well past SHARE_CAPTURE_MS.
    expect(9_000 + wait).toBeGreaterThan(SHARE_CAPTURE_MS);
    clock.now += wait;
    expect(share.peekCapture(win)).toBe(good);
    expect(share.takeCapture(win)).toBe(good);
  });

  it('a grant taken but never confirmed does not move the window', () => {
    const { share, clock } = make();
    share.arm(win);
    share.takeGrant(win);
    clock.now += SHARE_CAPTURE_MS + 1;
    expect(share.peekCapture(win)).toBe(false);
  });

  it('a confirmation with no grant use spent (the warm path) does not move the window', () => {
    const { share, clock, document } = make();
    share.arm(win);
    clock.now += PART;
    share.confirmGrant(win, document.n);
    clock.now += REST + 1;
    expect(share.peekCapture(win)).toBe(false);
  });

  it('confirms once, and only for its own contents and document', () => {
    const { share, clock, document } = make();
    share.arm(win);
    share.takeGrant(win);
    clock.now += PART;
    share.confirmGrant(other, document.n);
    share.confirmGrant(win, document.n + 1);
    clock.now += REST + 1;
    expect(share.peekCapture(win)).toBe(false);
    share.arm(win);
    share.takeGrant(win);
    clock.now += PART;
    share.confirmGrant(win, document.n);
    clock.now += PART;
    share.confirmGrant(win, document.n); // a second confirmation does not extend it
    clock.now += SHARE_CAPTURE_MS - PART + 1;
    expect(share.peekCapture(win)).toBe(false);
  });

  it('a clock that went backwards finds nothing', () => {
    const { share, clock } = make();
    share.arm(win);
    clock.now -= 1;
    expect(share.takeGrant(win)).toBe(false);
    expect(share.peekCapture(win)).toBe(false);
  });

  it('is for the contents it was armed for', () => {
    const { share } = make();
    share.arm(win);
    expect(share.peekCapture(other)).toBe(false);
    expect(share.takeGrant(other)).toBe(false);
    // Another window asking does not spend or clear it.
    expect(share.takeCapture(win)).toBe(true);
  });

  it('is for the document it was armed in', () => {
    const { share, document } = make();
    share.arm(win);
    document.n++;
    expect(share.peekCapture(win)).toBe(false);
    expect(share.takeGrant(win)).toBe(false);
    document.n--;
    expect(share.takeCapture(win)).toBe(false);
  });

  it('clear drops it, and a new arm replaces it with both uses fresh', () => {
    const { share } = make();
    share.arm(win);
    share.clear();
    expect(share.takeGrant(win)).toBe(false);
    share.arm(win);
    share.takeGrant(win);
    share.takeCapture(win);
    share.arm(win);
    expect(share.takeGrant(win)).toBe(true);
    expect(share.takeCapture(win)).toBe(true);
  });
});
