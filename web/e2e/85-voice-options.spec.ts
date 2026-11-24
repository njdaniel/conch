// Voice options is a panel over the page (issue #2189, UI-talk-2): opening it
// does not move the conversation or the voice strip, the panel lies inside the
// window (scrolling in itself if it is taller), Escape closes it and returns
// focus to its button, a press outside closes it, and More about sharing
// still opens it at the share notes. Measured as geometry, in the pinned
// LiveKit of voice-world.ts: without Docker it fails when CI is set and is
// skipped otherwise.
import type { Locator, Page } from '@playwright/test';
import { drawDesktopStrip, expect, test } from './fixtures';
import { enter, joinVoice, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';

const voice = (page: Page) => page.getByRole('region', { name: 'Voice' });

// box is the element's rectangle as the page lays it out now.
async function box(locator: Locator, what: string) {
  const b = await locator.boundingBox();
  if (b === null) throw new Error(`${what} has no box`);
  return b;
}

// settled waits for the element's box to stop changing: presence frames land
// after the join and redraw the roster, which is not what is measured here.
async function settled(locator: Locator, what: string): Promise<void> {
  let last = '';
  let same = 0;
  await expect(async () => {
    const now = JSON.stringify(await box(locator, what));
    same = now === last ? same + 1 : 0;
    last = now;
    expect(same, `${what} is still changing`).toBeGreaterThanOrEqual(4);
  }).toPass({ timeout: 10_000, intervals: [250] });
}

const sizes = [
  ['desktop 1440×900', 1440, 900, true],
  ['desktop 1215×900', 1215, 900, true],
  ['phone 390×844', 390, 844, false],
] as const;

test.describe('Voice options', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the Voice options suite needs Docker for the pinned LiveKit image');
      info.skip(true, 'Docker is not available; the Voice options suite needs it');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });

  test.afterAll(() => {
    stopWorld?.();
  });

  for (const [name, width, height, desktop] of sizes) {
    test(`P2189 ${name}: opening Voice options moves nothing under it and the panel is in the window`, async ({ browser }) => {
      let p: Someone | undefined;
      try {
        p = await enter(browser, vw, vw.alice, { context: { viewport: { width, height } } });
        const page = p.page;
        if (desktop) await drawDesktopStrip(page);
        await joinVoice(p, 'Join voice');
        const strip = voice(page);
        const conversation = page.locator('.conversation');
        const button = strip.getByRole('button', { name: 'Voice options' });
        const panel = page.locator('#voice-options');

        await expect(strip.getByRole('list', { name: / in voice$/ })).toContainText(p.member.name, { timeout: 30_000 });
        await settled(strip, 'the strip');
        const before = { conversation: await box(conversation, 'the conversation'), strip: await box(strip, 'the strip') };
        await button.click();
        await expect(button).toHaveAttribute('aria-expanded', 'true');
        await expect(panel).toBeVisible();
        // Nothing under it moved (the layout settles a frame after the click).
        await expect(async () => {
          expect(await box(conversation, 'the conversation')).toEqual(before.conversation);
          expect(await box(strip, 'the strip')).toEqual(before.strip);
        }).toPass({ timeout: 3000 });

        // The panel lies inside the window; taller content scrolls inside it.
        const b = await box(panel, 'the panel');
        expect(b.x, 'the panel starts inside the window').toBeGreaterThanOrEqual(0);
        expect(b.y, 'the panel starts below the window top').toBeGreaterThanOrEqual(0);
        expect(b.x + b.width, 'the panel ends inside the window width').toBeLessThanOrEqual(width + 0.5);
        expect(b.y + b.height, 'the panel ends inside the window height').toBeLessThanOrEqual(height + 0.5);
        // Where there is room it is above the strip; a phone's strip is too
        // near the top, and the panel is a sheet over the window instead.
        if (desktop) expect(b.y + b.height, 'the panel is above the strip').toBeLessThanOrEqual(before.strip.y + 0.5);
        else await expect(panel).toHaveClass(/sheet/);
        // The page itself gained no scroll from it.
        const page2 = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight);
        expect(page2).toBeLessThanOrEqual(1);
      } finally {
        await p?.close();
      }
    });
  }

  // A window too short for the panel: it scrolls inside itself, a control low
  // in it can be reached and used, and the page and the strip do not move.
  test('P2189 a panel taller than its room scrolls inside itself and moves nothing', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 520 } } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      const strip = voice(page);
      const panel = page.locator('#voice-options');
      await expect(strip.getByRole('list', { name: / in voice$/ })).toContainText(p.member.name, { timeout: 30_000 });
      // This short window scrolls as a document: bring the button into view
      // first, so the click itself does not scroll what is measured.
      await strip.getByRole('button', { name: 'Voice options' }).scrollIntoViewIfNeeded();
      await settled(strip, 'the strip');
      const before = await box(strip, 'the strip');
      await strip.getByRole('button', { name: 'Voice options' }).click();
      await expect(panel).toBeVisible();
      const scrollBefore = await page.evaluate(() => window.scrollY);
      const room = await panel.evaluate((el) => ({ client: el.clientHeight, content: el.scrollHeight }));
      expect(room.content, 'the content is taller than the panel').toBeGreaterThan(room.client);
      // The last control in the panel is reached by scrolling the panel, and works.
      const last = panel.locator('input, select, button').last();
      // Scrolled by the wheel over the panel, as a person does.
      const over = await box(panel, 'the panel');
      await page.mouse.move(over.x + over.width / 2, over.y + over.height / 2);
      await page.mouse.wheel(0, 5000);
      await expect.poll(() => panel.evaluate((el) => el.scrollTop + el.clientHeight >= el.scrollHeight - 1)).toBe(true);
      expect(await panel.evaluate((el) => el.scrollTop), 'the panel scrolled').toBeGreaterThan(0);
      const lb = await box(last, 'the last control');
      const pb = await box(panel, 'the panel');
      expect(lb.y).toBeGreaterThanOrEqual(pb.y - 0.5);
      expect(lb.y + lb.height).toBeLessThanOrEqual(pb.y + pb.height + 0.5);
      const type = await last.getAttribute('type');
      if (type === 'range') {
        const was = await last.inputValue();
        await last.click();
        await page.keyboard.press('ArrowRight');
        expect(await last.inputValue(), 'the slider moved').not.toBe(was);
      } else {
        await last.click();
      }
      expect(await page.evaluate(() => window.scrollY), 'the page did not scroll').toBe(scrollBefore);
      expect(await box(strip, 'the strip')).toEqual(before);
    } finally {
      await p?.close();
    }
  });

  // On a phone the section has little room above it, and a dock scrolled to
  // reach the button can leave its top off the window: the panel is then a
  // sheet over the window, and a control in it is in view and usable.
  test('P2189 on a phone with a scrolled dock the panel is a sheet in the window and usable', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 390, height: 520 }, hasTouch: true, isMobile: true } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      const strip = voice(page);
      const button = strip.getByRole('button', { name: 'Voice options' });
      await button.scrollIntoViewIfNeeded();
      await page.locator('.dock').evaluate((el) => {
        el.scrollTop = el.scrollHeight;
      });
      await button.tap();
      const panel = page.locator('#voice-options');
      await expect(panel).toBeVisible();
      await expect(panel).toHaveClass(/sheet/);
      const b = await box(panel, 'the panel');
      expect(b.x).toBeGreaterThanOrEqual(0);
      expect(b.y).toBeGreaterThanOrEqual(0);
      expect(b.x + b.width).toBeLessThanOrEqual(390.5);
      expect(b.y + b.height).toBeLessThanOrEqual(520.5);
      expect(b.height, 'a sheet is at most about 80% of the window').toBeLessThanOrEqual(520 * 0.8 + 1);
      const open = panel.getByRole('radio', { name: 'Open microphone' });
      await open.scrollIntoViewIfNeeded();
      await open.tap();
      await expect(open).toBeChecked();
    } finally {
      await p?.close();
    }
  });

  test('P2189 Escape closes it and returns focus to its button; a press outside closes it; the button toggles it', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 900 } } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      const button = voice(page).getByRole('button', { name: 'Voice options' });
      const panel = page.locator('#voice-options');

      await button.click();
      await expect(panel).toBeVisible();
      await panel.getByRole('radio', { name: 'Open microphone' }).focus();
      await page.keyboard.press('Escape');
      await expect(panel).toBeHidden();
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      await expect(button).toBeFocused();

      await button.click();
      await expect(panel).toBeVisible();
      // The panel can cover the conversation; press the strip's own state line.
      await voice(page).locator('.voice-phase').click();
      await expect(panel).toBeHidden();

      await button.click();
      await expect(panel).toBeVisible();
      await button.click();
      await expect(panel).toBeHidden();
    } finally {
      await p?.close();
    }
  });
});
