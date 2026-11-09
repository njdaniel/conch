// The packaged smoke test of the renderer's two new allowances and their
// refusals (issue #1580, D4-03 of #772; Q-D4-1 and Q-D4-4 on #1578). Needs a
// display (CI: xvfb-run) and the unpacked build in dist/, like shell.spec.ts.
//
// Under Xvfb there is no companion grant and no screen-cast portal, so what is
// asserted here is the refusals: a LiveKit-shaped request is cancelled by
// main's request filter, and getDisplayMedia rejects with no grant and with no
// gesture. The grant itself and a real portal pick are the manual check of
// #1580 and the full run of #1584.
import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { launch, type Launched } from './launch.ts';

test.describe('the renderer’s network and capture allowances, with no companion grant', () => {
  let app: Launched;
  let browser: Browser;
  let page: Page;

  test.beforeAll(async () => {
    app = launch({ debugPort: true });
    browser = await chromium.connectOverCDP(await app.cdpUrl());
    const context = browser.contexts()[0];
    if (context === undefined) throw new Error('the app has no browser context');
    page = context.pages()[0] ?? (await context.waitForEvent('page'));
    await page.waitForLoadState('load');
  });

  test.afterAll(async () => {
    await browser.close();
    await app.stop();
  });

  test('cancels a LiveKit-shaped https request and wss connection in main', async () => {
    const outcome = await page.evaluate(`Promise.all([
      fetch('https://lk.example.test/twirp/livekit.RoomService/ListRooms', { method: 'POST' }).then(() => 'sent', () => 'refused'),
      new Promise((resolve) => {
        const ws = new WebSocket('wss://lk.example.test/rtc?access_token=x');
        ws.onopen = () => resolve('open');
        ws.onerror = () => resolve('refused');
      }),
    ])`);
    expect(outcome).toEqual(['refused', 'refused']);
    // The CSP lets wss: and https: as far as main (Q-D4-1); main's filter
    // names the refusal, scheme and host only.
    await expect.poll(() => app.output()).toContain('conch-desktop: blocked a request to https://lk.example.test');
    expect(app.output()).toContain('conch-desktop: blocked a request to wss://lk.example.test');
    expect(app.output()).not.toContain('access_token');
  });

  test('rejects getDisplayMedia with no gesture', async () => {
    const result = await page.evaluate("navigator.mediaDevices.getDisplayMedia({ video: true }).then(() => 'granted', (e) => e.name)");
    expect(result).not.toBe('granted');
  });

  test('rejects getDisplayMedia right after a click when no companion grant is live, and says why', async () => {
    await page.evaluate(`window.__share = 'pending'; document.addEventListener('mouseup', () => {
      navigator.mediaDevices.getDisplayMedia({ video: true, audio: true }).then(() => { window.__share = 'granted'; }, (e) => { window.__share = e.name; });
    }, { once: true });`);
    await page.mouse.click(5, 5);
    await expect.poll(() => page.evaluate('window.__share')).not.toBe('pending');
    expect(await page.evaluate('window.__share')).not.toBe('granted');
    // The reason is main's, and it is not the picker's: nothing was opened.
    await expect.poll(() => app.output()).toMatch(/display capture refused: (no grant|share unavailable: no screen-cast portal)/);
  });

  test('keeps the microphone and camera denied', async () => {
    const result = await page.evaluate("navigator.mediaDevices.getUserMedia({ audio: true, video: true }).then(() => 'granted', (e) => e.name)");
    expect(result).not.toBe('granted');
  });
});
