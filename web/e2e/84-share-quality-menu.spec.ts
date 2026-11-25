// The share quality menu beside Share screen (issue #2188) must clear the
// edges of the viewport and the dock that holds it, even on a short screen.
import type { BrowserContextOptions } from '@playwright/test';
import { drawDesktopStrip, expect, phones, test } from './fixtures';
import { enter, joinVoice } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

// fits: the whole menu shows without scrolling. On a phone the voice view puts
// the button about halfway down, so neither side holds all nine options: the
// menu takes the roomier side and scrolls, and every option must be reachable.
const layouts = [
  { name: '1440×900 browser', member: 'alice', context: { viewport: { width: 1440, height: 900 } }, desktopStrip: false, fits: true },
  { name: '1215×900 with the desktop strip', member: 'carol', context: { viewport: { width: 1215, height: 900 } }, desktopStrip: true, fits: true },
  { name: '390×844 phone', member: 'bob', context: phones[0][1], desktopStrip: false, fits: false },
] as const satisfies readonly { name: string; member: 'alice' | 'bob' | 'carol'; context: BrowserContextOptions; desktopStrip: boolean; fits: boolean }[];

test.describe('the share quality menu', { tag: '@voice' }, () => {
  let vw: VoiceWorld | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the share quality menu suite needs Docker for the pinned LiveKit image');
      info.skip(true, 'Docker is not available; the share quality menu suite needs it');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
  });

  test.afterAll(() => { vw?.stop(); });

  for (const { name, member, context, desktopStrip, fits } of layouts) {
    test(`P2188 every option is reachable at ${name}`, async ({ browser }) => {
      if (vw === undefined) throw new Error('no voice world');
      const person = await enter(browser, vw, vw[member], { context });
      try {
        const { page } = person;
        if (desktopStrip) await drawDesktopStrip(page);
        await joinVoice(person, 'Join voice');
        const voice = page.getByRole('region', { name: 'Voice' });
        const opener = voice.getByRole('button', { name: 'Share quality' });
        const menu = voice.getByRole('menu', { name: 'Share quality' });
        const last = menu.getByRole('menuitemradio', { name: '60 fps' });
        await opener.click();

        await expect(menu).toBeInViewport({ ratio: 1 });
        if (fits) {
          expect(await menu.evaluate((el) => el.scrollHeight - el.clientHeight)).toBe(0);
        } else {
          await menu.hover();
          await page.mouse.wheel(0, 1000);
        }
        await expect(last).toBeInViewport({ ratio: 1 });

        const anchor = await opener.boundingBox();
        const box = await menu.boundingBox();
        if (anchor === null || box === null) throw new Error('the menu and its button must have boxes');
        if (anchor.y + anchor.height + box.height > context.viewport.height) {
          expect(box.y + box.height).toBeLessThanOrEqual(anchor.y);
        }
        // A real pointer hit checks that another part of the dock does not cover the last option.
        await last.click();
        await expect(last).toBeChecked();
      } finally {
        await person.close();
      }
    });
  }

  // #2194: choosing an option never moves the options under the pointer. Opening
  // upward (1440×900) pins the menu's bottom edge, so any change of its height
  // would shift every option.
  const stable = [
    { name: '1440×900 browser, opening upward', member: 'alice', context: { viewport: { width: 1440, height: 900 } } },
    { name: '390×844 phone, opening downward', member: 'bob', context: phones[0][1] },
  ] as const satisfies readonly { name: string; member: 'alice' | 'bob'; context: BrowserContextOptions }[];

  for (const { name, member, context } of stable) {
    test(`P2194 choosing an option does not move the menu at ${name}`, async ({ browser }) => {
      if (vw === undefined) throw new Error('no voice world');
      const person = await enter(browser, vw, vw[member], { context });
      try {
        const { page } = person;
        await joinVoice(person, 'Join voice');
        const voice = page.getByRole('region', { name: 'Voice' });
        const menu = voice.getByRole('menu', { name: 'Share quality' });
        await voice.getByRole('button', { name: 'Share quality' }).click();
        const items = menu.getByRole('menuitemradio');
        const count = await items.count();
        const boxes = async () => {
          const all: { x: number; y: number; width: number; height: number }[] = [];
          for (let i = 0; i < count; i++) {
            const item = items.nth(i);
            await item.scrollIntoViewIfNeeded();
            const box = await item.boundingBox();
            if (box === null) throw new Error('every option must have a box');
            all.push(box);
          }
          return all;
        };
        const choose = async (option: string | RegExp) => { await menu.getByRole('menuitemradio', { name: option }).first().click(); };
        const same = async (before: Awaited<ReturnType<typeof boxes>>, why: string) => {
          const after = await boxes();
          for (const [i, b] of before.entries()) {
            for (const k of ['x', 'y', 'width', 'height'] as const) {
              expect(Math.abs((after[i]?.[k] ?? Number.NaN) - b[k]), `${why}: option ${String(i)} ${k}`).toBeLessThanOrEqual(0.5);
            }
          }
        };

        await choose(/^1080p/);
        await choose('30 fps');
        // Scrolling a phone's menu changes where the boxes are, so measure at the top.
        await menu.evaluate((el) => { el.scrollTop = 0; });
        const before = await boxes();

        await choose(/^Auto/);
        await expect(menu.getByRole('menuitemradio', { name: /^Auto/ })).toBeChecked();
        await menu.evaluate((el) => { el.scrollTop = 0; });
        await same(before, 'entering Auto');

        await choose(/^1080p/);
        await menu.evaluate((el) => { el.scrollTop = 0; });
        await same(before, 'leaving Auto');

        await choose(/^Source/);
        await choose('60 fps');
        await expect(menu.getByText(/Source at 60 fps is 4K/)).toBeVisible();
        await menu.evaluate((el) => { el.scrollTop = 0; });
        await same(before, 'Source at 60 fps');

        // A second click 150 ms after the first lands on Auto again, not on what
        // moved under the pointer.
        // Real mouse clicks at one fixed point, as a hand does; a locator click
        // would wait for the option to settle and follow it.
        await choose(/^1080p/);
        await menu.evaluate((el) => { el.scrollTop = 0; });
        const auto = menu.getByRole('menuitemradio', { name: /^Auto/ });
        const at = await auto.boundingBox();
        if (at === null) throw new Error('Auto must have a box');
        const x = at.x + at.width / 2;
        const y = at.y + at.height / 2;
        await page.mouse.click(x, y);
        await page.waitForTimeout(150);
        await page.mouse.click(x, y);
        await expect(auto).toBeChecked();
      } finally {
        await person.close();
      }
    });
  }

  test('P2188 a short phone menu scrolls by mouse and keyboard', async ({ browser }) => {
    if (vw === undefined) throw new Error('no voice world');
    const context = { ...phones[0][1], viewport: { width: 390, height: 420 } };
    const person = await enter(browser, vw, vw.bob, { context });
    try {
      await joinVoice(person, 'Join voice');
      const { page } = person;
      const voice = page.getByRole('region', { name: 'Voice' });
      const opener = voice.getByRole('button', { name: 'Share quality' });
      const menu = voice.getByRole('menu', { name: 'Share quality' });
      const last = menu.getByRole('menuitemradio', { name: '60 fps' });
      await opener.press('ArrowDown');

      await expect(menu).toBeInViewport({ ratio: 1 });
      expect(await menu.evaluate((el) => el.scrollHeight - el.clientHeight)).toBeGreaterThan(0);
      await menu.hover();
      await page.mouse.wheel(0, 1000);
      await expect(last).toBeInViewport({ ratio: 1 });
      await last.click();
      await expect(last).toBeChecked();

      await page.keyboard.press('Home');
      const auto = menu.getByRole('menuitemradio', { name: /^Auto/ });
      await expect(auto).toBeFocused();
      await expect(auto).toBeInViewport({ ratio: 1 });
      await page.keyboard.press('End');
      await expect(last).toBeFocused();
      await expect(last).toBeInViewport({ ratio: 1 });
      await page.keyboard.press('ArrowUp');
      const thirty = menu.getByRole('menuitemradio', { name: '30 fps' });
      await expect(thirty).toBeFocused();
      await page.keyboard.press('Enter');
      await expect(thirty).toBeChecked();
      await page.keyboard.press('Escape');
      await expect(menu).toBeHidden();
      await expect(opener).toBeFocused();
    } finally {
      await person.close();
    }
  });
});
