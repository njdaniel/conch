// Issue #1534: when the server is upgraded under an open page, a polite notice
// offers a reload. It sits inside the window's height: the composer is still
// on screen and the document does not scroll. The upgrade is faked by
// answering /healthz with another version.
import type { BrowserContextOptions } from '@playwright/test';
import { expect, openChannel, phoneSizes, signIn, test } from './fixtures';

const sizes: readonly (readonly [string, BrowserContextOptions])[] = [
  ['390×844', phoneSizes[2][1]],
  ['320×640', phoneSizes[0][1]],
  ['1440×900', { viewport: { width: 1440, height: 900 } }],
];

for (const [size, options] of sizes) {
  test(`P1534 at ${size} the reload notice and the composer are both on screen and the page does not scroll`, async ({ meet, world }) => {
    const { page } = await meet(options);
    let version = 'v-before';
    await page.route('**/healthz', async (route) => {
      await route.fulfill({ json: { status: 'ok', version, db: 'ok', voice: 'not_configured' } });
    });
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await openChannel(page, 'general');
    const composer = page.getByLabel('Message #general');
    await expect(composer).toBeVisible();

    version = 'v-after';
    await page.evaluate(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    const notice = page.getByRole('status').filter({ hasText: 'Conch was updated. Reload to get the new version.' });
    await expect(notice).toBeVisible();
    await expect(notice.getByRole('button', { name: 'Reload' })).toBeVisible();

    const screen = page.viewportSize();
    if (screen === null) throw new Error('no viewport');
    for (const [what, el] of [['the notice', notice], ['the composer', composer]] as const) {
      const box = await el.boundingBox();
      if (box === null) throw new Error(`${what} has no box`);
      expect(box.y, what).toBeGreaterThanOrEqual(0);
      expect(box.y + box.height, `${what} bottom`).toBeLessThanOrEqual(screen.height + 0.5);
    }
    const { scroll, inner } = await page.evaluate(() => ({ scroll: document.documentElement.scrollHeight, inner: window.innerHeight }));
    expect(scroll, 'the document scrolls').toBeLessThanOrEqual(inner);
  });
}
