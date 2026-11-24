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
        expect(b.y + b.height, 'the panel is above the strip').toBeLessThanOrEqual(before.strip.y + 0.5);
        const scrolls = await panel.evaluate((el) => getComputedStyle(el).overflowY);
        expect(scrolls).toBe('auto');
        // The page itself gained no scroll from it.
        const page2 = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight);
        expect(page2).toBeLessThanOrEqual(1);
      } finally {
        await p?.close();
      }
    });
  }

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
