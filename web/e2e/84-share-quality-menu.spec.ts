// The share quality menu beside Share screen (issue #2188) must clear the
// edges of the viewport and the dock that holds it, even on a short screen.
import type { BrowserContextOptions } from '@playwright/test';
import { drawDesktopStrip, expect, phones, test } from './fixtures';
import { enter, joinVoice } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

function lum([r, g, b]: readonly [number, number, number]): number {
  const f = (v: number) => { const c = v / 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; };
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
}
function contrast(a: readonly [number, number, number], b: readonly [number, number, number]): number {
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

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
        const opener = voice.getByRole('button', { name: 'Share quality' });
        // Joining voice settles the page above the button a moment later (a
        // 44 px row can appear); the menu is anchored to the button, so wait
        // until the button has held still for 1.2 s before measuring anything.
        const settled = async () => {
          let last = Number.NaN;
          let still = 0;
          await expect.poll(async () => {
            const y = (await opener.boundingBox())?.y ?? Number.NaN;
            still = y === last ? still + 1 : 0;
            last = y;
            await page.waitForTimeout(400);
            return still;
          }, { timeout: 20_000 }).toBeGreaterThanOrEqual(3);
        };
        await settled();
        await opener.click();
        // Each option's rect in the viewport, where the pointer is, read in one
        // step. Nothing scrolls during the test: choices are made by a dispatched
        // click, which does not scroll, and the menu's scrollTop is part of what
        // must stay the same. (A rect relative to the menu would pass even when
        // the whole menu grows upward under the pointer.)
        const boxes = () => menu.evaluate((el) => ({
          scrollTop: el.scrollTop,
          rects: Array.from(el.querySelectorAll('[role^="menuitem"]')).map((item) => {
            const r = item.getBoundingClientRect();
            return { x: r.left, y: r.top, width: r.width, height: r.height };
          }),
        }));
        const choose = async (option: string | RegExp) => { await menu.getByRole('menuitemradio', { name: option }).first().evaluate((el) => { (el as HTMLElement).click(); }); };
        const same = async (before: Awaited<ReturnType<typeof boxes>>, why: string) => {
          const after = await boxes();
          expect(after.scrollTop, `${why}: the menu scrolled`).toBe(before.scrollTop);
          expect(after.rects).toHaveLength(before.rects.length);
          for (const [i, b] of before.rects.entries()) {
            for (const k of ['x', 'y', 'width', 'height'] as const) {
              expect(Math.abs((after.rects[i]?.[k] ?? Number.NaN) - b[k]), `${why}: option ${String(i)} ${k}`).toBeLessThanOrEqual(0.5);
            }
          }
        };

        await choose(/^1080p/);
        await choose('30 fps');
        const before = await boxes();
        // On a desktop-wide screen every label, Source (up to 4K) included, fits on one line.
        if (context.viewport.width >= 1000) for (const r of before.rects) expect(r.height).toBeLessThan(40);

        await choose(/^Auto/);
        await expect(menu.getByRole('menuitemradio', { name: /^Auto/ })).toBeChecked();
        await same(before, 'entering Auto');

        await choose(/^1080p/);
        await same(before, 'leaving Auto');

        await choose(/^Source/);
        await choose('60 fps');
        await expect(menu.getByText(/Source at 60 fps is 4K/)).toBeVisible();
        await same(before, 'Source at 60 fps');

        // A second click 150 ms after the first lands on Auto again, not on what
        // moved under the pointer.
        // Real mouse clicks at one fixed point, as a hand does; a locator click
        // would wait for the option to settle and follow it.
        await choose(/^1080p/);
        await settled();
        const auto = menu.getByRole('menuitemradio', { name: /^Auto/ });
        const at = await auto.boundingBox();
        if (at === null) throw new Error('Auto must have a box');
        const x = at.x + at.width / 2;
        const y = at.y + at.height / 2;
        const underPointer = () => auto.evaluate((el, [px, py]) => el.contains(document.elementFromPoint(px ?? 0, py ?? 0)), [x, y]);
        await page.mouse.click(x, y);
        await page.waitForTimeout(150);
        expect(await underPointer(), 'Auto is still under the pointer').toBe(true);
        await page.mouse.click(x, y);
        await expect(auto).toBeChecked();
      } finally {
        await person.close();
      }
    });
  }

  test('P2194 on a phone the checkmark clears the row label', async ({ browser }) => {
    if (vw === undefined) throw new Error('no voice world');
    const person = await enter(browser, vw, vw.bob, { context: phones[0][1] });
    try {
      const { page } = person;
      await joinVoice(person, 'Join voice');
      const voice = page.getByRole('region', { name: 'Voice' });
      await voice.getByRole('button', { name: 'Share quality' }).click();
      const row = voice.getByRole('menu', { name: 'Share quality' }).locator('[aria-checked="true"]').first();
      await row.scrollIntoViewIfNeeded();
      // The mark's right edge: its left offset plus its glyph, from the row's own box.
      const [markRight, labelLeft] = await row.evaluate((el) => {
        const r = el.getBoundingClientRect();
        const m = getComputedStyle(el, '::before');
        const label = el.querySelector('.item-text')?.getBoundingClientRect().left ?? Number.NaN;
        return [r.left + el.clientLeft + Number.parseFloat(m.insetInlineStart) + Number.parseFloat(m.width), label];
      });
      expect(labelLeft).toBeGreaterThanOrEqual(markRight);
    } finally {
      await person.close();
    }
  });

  test('P2194 under forced colors only the checked option shows a checkmark', async ({ browser }) => {
    if (vw === undefined) throw new Error('no voice world');
    const person = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 900 } } });
    try {
      const { page } = person;
      await joinVoice(person, 'Join voice');
      await page.emulateMedia({ forcedColors: 'active' });
      const voice = page.getByRole('region', { name: 'Voice' });
      const menu = voice.getByRole('menu', { name: 'Share quality' });
      await voice.getByRole('button', { name: 'Share quality' }).click();
      const items = menu.getByRole('menuitemradio');
      const marks = () => items.evaluateAll((els) => els.map((el) => [el.getAttribute('aria-checked'), getComputedStyle(el, '::before').content, getComputedStyle(el, '::before').visibility]));
      for (const [checked, content, visibility] of await marks()) {
        if (checked === 'true') {
          expect(content).toContain('✓');
          expect(visibility).toBe('visible');
        } else {
          expect(content).not.toContain('✓');
        }
      }
      expect((await marks()).filter(([c]) => c === 'true').length).toBe(2);
      // Computed colours ignore the backplate forced colors paints behind text,
      // so read the pixels: a row's label must show ink against its ground in
      // every state a row can be drawn in.
      const row = (name: string | RegExp) => menu.getByRole('menuitemradio', { name }).first();
      const inkOf = async (clip: { x: number; y: number; width: number; height: number }) => {
        const shot = Array.from((await page.screenshot({ clip, animations: 'disabled' })) as unknown as Uint8Array);
        // Decode in the page (a canvas), so the test needs no image library.
        const entries = await page.evaluate(async (bytes) => {
          const bmp = await createImageBitmap(new Blob([new Uint8Array(bytes)], { type: 'image/png' }));
          const cv = new OffscreenCanvas(bmp.width, bmp.height);
          const ctx = cv.getContext('2d');
          if (ctx === null) throw new Error('no 2d context');
          ctx.drawImage(bmp, 0, 0);
          const data = ctx.getImageData(0, 0, bmp.width, bmp.height).data;
          const tally = new Map<string, number>();
          for (let i = 0; i < data.length; i += 4) {
            const key = `${String(data[i])},${String(data[i + 1])},${String(data[i + 2])}`;
            tally.set(key, (tally.get(key) ?? 0) + 1);
          }
          return Array.from(tally.entries());
        }, shot);
        const count = new Map<string, number>(entries);
        const sorted = [...count.entries()].sort((x, y) => y[1] - x[1]);
        const ground = (sorted[0]?.[0] ?? '0,0,0').split(',').map(Number) as [number, number, number];
        let best = 1;
        let strong = 0;
        for (const [key, n] of sorted) {
          const c = contrast(key.split(',').map(Number) as [number, number, number], ground);
          if (c > best) best = c;
          if (c >= 4.5) strong += n;
        }
        return { best, strong, colours: sorted.length };
      };
      const readable = async (what: string, target: ReturnType<typeof row>, mark: boolean) => {
        const parts = await target.evaluate((el) => {
          const r = el.getBoundingClientRect();
          const label = el.querySelector('.item-text')?.getBoundingClientRect();
          return { row: { x: r.left, y: r.top, right: r.right, height: r.height }, labelLeft: label?.left ?? r.left };
        });
        const text = await inkOf({ x: parts.labelLeft, y: parts.row.y + 4, width: parts.row.right - parts.labelLeft - 4, height: parts.row.height - 8 });
        expect(text.colours, `${what}: label shows more than one colour`).toBeGreaterThanOrEqual(2);
        expect(text.best, `${what}: label contrast`).toBeGreaterThanOrEqual(4.5);
        expect(text.strong, `${what}: label ink pixels`).toBeGreaterThan(8);
        if (mark) {
          const ticks = await inkOf({ x: parts.row.x + 8, y: parts.row.y + 6, width: 18, height: parts.row.height - 12 });
          expect(ticks.strong, `${what}: checkmark ink pixels`).toBeGreaterThan(5);
        }
      };
      await row(/^1080p/).click();
      await row('30 fps').click();
      await row(/^1080p/).hover();
      await readable('hovered checked row', row(/^1080p/), true);
      await row(/^720p/).hover();
      await readable('hovered unchecked row', row(/^720p/), false);
      await page.mouse.move(0, 0);
      await page.keyboard.press('Escape');
      await expect(menu).toBeHidden();
      const opener = voice.getByRole('button', { name: 'Share quality' });
      await opener.focus();
      await page.keyboard.press('Enter');
      await page.keyboard.press('ArrowUp');
      const focused = menu.locator('[role^="menuitem"]:focus');
      await expect(focused).toHaveCount(1);
      await page.mouse.move(0, 0);
      await readable('keyboard-focused row', focused, false);
      await row(/^Auto/).click();
      await row('15 fps').hover({ force: true });
      await readable('hovered disabled frame rate in Auto', row('15 fps'), false);
    } finally {
      await person.close();
    }
  });

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
