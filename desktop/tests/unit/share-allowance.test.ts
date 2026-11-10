import { describe, expect, it } from 'vitest';

import { ShareAllowance, SHARE_ARM_MS } from '../../main/share-allowance.ts';

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
  ])('expires: %s', (_name, wait, good) => {
    const { share, clock } = make();
    share.arm(win);
    clock.now += wait;
    expect(share.peekCapture(win)).toBe(good);
    expect(share.takeGrant(win)).toBe(good);
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
