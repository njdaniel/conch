// Whether this tab is sharing its screen (issue #714): the answer the pages
// that show a secret once ask before they reveal it. A file of its own, so
// no share another test left running counts here.
import { describe, expect, it } from 'vitest';
import { holdShare } from './heldshare';
import { onTabSharing, tabSharing } from './share';

describe('tabSharing', () => {
  it('is false with no share, true from the claim to the end of the stop, and tells each change', async () => {
    expect(tabSharing()).toBe(false);
    const told: boolean[] = [];
    const stop = onTabSharing(() => {
      told.push(tabSharing());
    });
    const share = await holdShare();
    // The picker is open: what it is choosing may show this page.
    expect(tabSharing()).toBe(true);
    const second = await holdShare();
    expect(tabSharing()).toBe(true);
    await share.stop();
    // Another share of the tab is still under way.
    expect(tabSharing()).toBe(true);
    await second.stop();
    expect(tabSharing()).toBe(false);
    expect(told).toEqual([true, false]);
    stop();
    const third = await holdShare();
    expect(told).toEqual([true, false]);
    await third.stop();
  });
});
