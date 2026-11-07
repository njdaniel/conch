import type { Page } from '@playwright/test';
import { post, reply } from './api';
import { credentialField, expect, expectFits, messages, phoneSizes, say, signIn, statusLine, test } from './fixtures';
import { call, type World } from './world';

const accents = ['green', 'blue', 'violet', 'pink', 'orange', 'cyan'] as const;
const presets = [
  { value: 'light', bg: 'rgb(245, 247, 246)', surface: 'rgb(255, 255, 255)', text: 'rgb(23, 33, 27)' },
  { value: 'dark', bg: 'rgb(17, 20, 22)', surface: 'rgb(23, 27, 30)', text: 'rgb(232, 236, 233)' },
  { value: 'high-contrast', bg: 'rgb(0, 0, 0)', surface: 'rgb(0, 0, 0)', text: 'rgb(255, 255, 255)' },
] as const;

function rgb(hex: string): string {
  if (/^#[\da-f]{3}$/.test(hex)) return rgb(`#${hex.slice(1).replace(/./g, (c) => c + c)}`);
  const channels = [1, 3, 5].map((at) => Number.parseInt(hex.slice(at, at + 2), 16));
  return `rgb(${channels.join(', ')})`;
}

async function accentColors(page: Page): Promise<{ readonly token: string; readonly brand: string }> {
  return page.evaluate(() => ({
    token: getComputedStyle(document.documentElement).getPropertyValue('--accent').trim(),
    brand: getComputedStyle(document.querySelector('.brand') as HTMLElement, '::before').backgroundColor,
  }));
}

test('every appearance preset, accent and density applies and survives reload', async ({ meet, world }) => {
  const { page } = await meet({ colorScheme: 'dark' });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const marker = `appearance-${Date.now().toString(36)}`;
  await say(page, 'general', marker);
  const row = messages(page).getByRole('listitem').filter({ hasText: marker });
  await expect(row).toBeVisible();
  await page.getByRole('button', { name: 'Settings' }).click();
  const drawer = page.getByRole('dialog', { name: 'Settings' });
  const theme = drawer.getByLabel('Theme preset');
  const accent = drawer.getByLabel('Accent color');
  const density = drawer.getByLabel('Chat density');

  for (const preset of presets) {
    await theme.selectOption(preset.value);
    await expect(page.locator('html')).toHaveAttribute('data-theme', preset.value);
    await expect(page.locator('body')).toHaveCSS('background-color', preset.bg);
    await expect(page.locator('body')).toHaveCSS('color', preset.text);
    await expect(page.locator('.topbar')).toHaveCSS('background-color', preset.surface);

    const seen = new Set<string>();
    for (const choice of accents) {
      await accent.selectOption(choice);
      await expect(page.locator('html')).toHaveAttribute('data-accent', choice);
      const colors = await accentColors(page);
      expect(colors.token).toMatch(/^#[\da-f]{6}$/);
      expect(colors.brand).toBe(rgb(colors.token));
      await expect(page.locator('.channel-list > li > button[aria-current="true"]')).toHaveCSS('border-left-color', rgb(colors.token));
      seen.add(colors.brand);
    }
    expect(seen.size, `${preset.value} has six distinct computed accents`).toBe(accents.length);

    await density.selectOption('compact');
    await expect(row).toHaveCSS('padding-top', '2.4px');
    await expect(row).toHaveCSS('column-gap', '8px');
    await density.selectOption('comfortable');
    await expect(row).toHaveCSS('padding-top', '6.4px');
    await expect(row).toHaveCSS('column-gap', '12px');
  }

  await theme.selectOption('system');
  await expect(page.locator('html')).not.toHaveAttribute('data-theme');
  await expect(page.locator('body')).toHaveCSS('background-color', presets[1].bg);
  await page.emulateMedia({ colorScheme: 'light' });
  await expect(page.locator('body')).toHaveCSS('background-color', presets[0].bg);

  await theme.selectOption('high-contrast');
  await accent.selectOption('cyan');
  await density.selectOption('compact');
  await page.reload();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'high-contrast');
  await expect(page.locator('html')).toHaveAttribute('data-accent', 'cyan');
  await expect(page.locator('html')).toHaveAttribute('data-density', 'compact');
  await expect(page.locator('body')).toHaveCSS('background-color', presets[2].bg);
  await expect(row).toHaveCSS('padding-top', '2.4px');
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(theme).toHaveValue('high-contrast');
  await expect(accent).toHaveValue('cyan');
  await expect(density).toHaveValue('compact');

  await theme.selectOption('light');
  await page.emulateMedia({ reducedMotion: 'reduce', forcedColors: 'active' });
  await expect(drawer).toHaveCSS('animation-name', 'none');
  const forced = await page.evaluate(() => ({
    bg: getComputedStyle(document.documentElement).getPropertyValue('--bg').trim(),
    text: getComputedStyle(document.documentElement).getPropertyValue('--text').trim(),
    focus: getComputedStyle(document.documentElement).getPropertyValue('--focus').trim(),
  }));
  expect(forced.bg).not.toBe('#f5f7f6');
  expect(forced.text).not.toBe('#17211b');
  expect(forced.focus).not.toBe('#0958a8');
});

const defaults = { theme: 'dark', accent: 'green', density: 'comfortable' } as const;

async function rootAttributes(page: Page): Promise<{ readonly theme: string | null; readonly accent: string | null; readonly density: string | null }> {
  return page.evaluate(() => {
    const root = document.documentElement;
    return { theme: root.getAttribute('data-theme'), accent: root.getAttribute('data-accent'), density: root.getAttribute('data-density') };
  });
}

test('the dark default is painted before any JavaScript runs, even on a light system', async ({ meet, world }) => {
  const { page } = await meet({ colorScheme: 'light' });
  let release: () => void = () => undefined;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(/\/assets\/[^/]+\.js$/, async (route) => {
    await held;
    await route.continue();
  });
  await page.goto(world.appUrl, { waitUntil: 'commit' });
  try {
    // The page script has not run: the loading screen is still up.
    await expect(page.locator('#root .boot')).toBeVisible();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    await expect.poll(() => page.evaluate(() => getComputedStyle(document.body).backgroundColor), { message: 'the first paint is dark' }).toBe(presets[1].bg);
    await page.waitForTimeout(300);
    await expect(page.locator('body')).toHaveCSS('background-color', presets[1].bg);
    await expect(page.locator('#root .boot')).toBeVisible();
  } finally {
    release();
  }
});

test('the Appearance settings work from the keyboard alone', async ({ meet, world }) => {
  const { page } = await meet({ colorScheme: 'dark' });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await page.getByRole('button', { name: 'Settings' }).focus();
  await page.keyboard.press('Enter');
  const drawer = page.getByRole('dialog', { name: 'Settings' });
  await expect(drawer).toBeVisible();
  const theme = drawer.getByLabel('Theme preset');
  for (let presses = 0; presses < 60; presses++) {
    if (await theme.evaluate((el) => el === document.activeElement)) break;
    await page.keyboard.press('Tab');
  }
  await expect(theme).toBeFocused();
  await page.keyboard.press('ArrowUp');
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await expect(page.locator('body')).toHaveCSS('background-color', presets[0].bg);
  await page.keyboard.press('Tab');
  await expect(drawer.getByLabel('Accent color')).toBeFocused();
  await page.keyboard.press('ArrowDown');
  await expect(page.locator('html')).toHaveAttribute('data-accent', 'blue');
  await page.keyboard.press('Tab');
  await expect(drawer.getByLabel('Chat density')).toBeFocused();
  await page.keyboard.press('ArrowDown');
  await expect(page.locator('html')).toHaveAttribute('data-density', 'compact');
  // The focus ring is drawn on the focused control in the chosen preset.
  const ring = await page.evaluate(() => {
    const el = document.activeElement;
    return el === null ? '' : getComputedStyle(el).outlineStyle;
  });
  expect(ring).not.toBe('none');
  await page.keyboard.press('Shift+Tab');
  await expect(drawer.getByLabel('Accent color')).toBeFocused();
});

for (const [size, options] of [phoneSizes[0], phoneSizes[2]]) {
  test(`at ${size} the presets apply and nothing overflows`, async ({ meet, world }) => {
    const { page } = await meet({ ...options, colorScheme: 'dark' });
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await page.getByRole('button', { name: 'Settings' }).tap();
    const drawer = page.getByRole('dialog', { name: 'Settings' });
    for (const preset of presets) {
      await drawer.getByLabel('Theme preset').selectOption(preset.value);
      await expect(page.locator('body')).toHaveCSS('background-color', preset.bg);
      for (const density of ['compact', 'comfortable']) {
        await drawer.getByLabel('Chat density').selectOption(density);
        await expectFits(page, `${size} ${preset.value} ${density}`);
        // Settled: the drawer slides in, so its box is read until it is still.
        await expect
          .poll(async () => {
            const box = await drawer.boundingBox();
            return box === null ? 'none' : box.x >= -0.5 && box.x + box.width <= options.viewport.width + 0.5 ? 'on screen' : 'off screen';
          }, { message: 'the drawer fits the screen' })
          .toBe('on screen');
      }
    }
  });
}

test('at 200% zoom the presets apply and nothing overflows', async ({ meet, world }) => {
  // 200% zoom of a 1280x720 window is a 640x360 CSS-pixel layout.
  const { page } = await meet({ viewport: { width: 640, height: 360 }, colorScheme: 'dark' });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await page.getByRole('button', { name: 'Settings' }).click();
  const drawer = page.getByRole('dialog', { name: 'Settings' });
  for (const preset of presets) {
    await drawer.getByLabel('Theme preset').selectOption(preset.value);
    await expect(page.locator('body')).toHaveCSS('background-color', preset.bg);
    for (const density of ['compact', 'comfortable']) {
      await drawer.getByLabel('Chat density').selectOption(density);
      await expectFits(page, `200% ${preset.value} ${density}`);
    }
  }
  await drawer.getByLabel('Theme preset').selectOption('dark');
  await page.keyboard.press('Escape');
  // CSS zoom on the root, as a browser's own zoom does for a page.
  await page.evaluate(() => {
    document.documentElement.style.setProperty('zoom', '2');
  });
  const wide = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth, inner: window.innerWidth }));
  expect(wide.scroll).toBeLessThanOrEqual(wide.inner);
});

test('voice and approval controls take each preset\'s colours', async ({ meet, world }) => {
  const { page } = await meet({ colorScheme: 'dark' });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  // The shared server has no LiveKit and no pending approval, so the page
  // shows neither control: build them with the page's own classes.
  await page.evaluate(() => {
    const card = document.createElement('article');
    card.className = 'approval-card';
    card.id = 'probe-approval';
    const voice = document.createElement('section');
    voice.className = 'voice';
    voice.id = 'probe-voice';
    const talk = document.createElement('button');
    talk.className = 'talk';
    talk.textContent = 'Talk';
    voice.append(talk);
    document.body.append(card, voice);
  });
  const read = () =>
    page.evaluate(() => {
      const css = getComputedStyle(document.documentElement);
      const card = getComputedStyle(document.getElementById('probe-approval') as HTMLElement);
      const talk = getComputedStyle(document.querySelector('#probe-voice .talk') as HTMLElement);
      const warn = css.getPropertyValue('--warn').trim();
      return {
        raised: css.getPropertyValue('--raised').trim(),
        accent: css.getPropertyValue('--accent').trim(),
        warn,
        cardBg: card.backgroundColor,
        cardEdge: card.borderLeftColor,
        talkEdge: talk.borderTopColor,
      };
    });
  const seen = new Set<string>();
  for (const preset of presets) {
    for (const accent of ['green', 'violet'] as const) {
      await page.evaluate(
        ([theme, tint]) => {
          document.documentElement.setAttribute('data-theme', theme as string);
          document.documentElement.setAttribute('data-accent', tint as string);
        },
        [preset.value, accent],
      );
      const got = await read();
      expect(got.cardBg, `${preset.value} approval card background`).toBe(rgb(got.raised));
      expect(got.cardEdge, `${preset.value} approval card edge`).toBe(rgb(got.warn));
      expect(got.talkEdge, `${preset.value}/${accent} talk button edge`).toBe(rgb(got.accent));
      seen.add(`${preset.value}/${got.cardBg}/${got.talkEdge}`);
    }
  }
  expect(seen.size, 'each preset and accent paints differently').toBe(6);
});

test('switching accounts never leaves the previous account\'s appearance on the page', async ({ meet, world }) => {
  const { page } = await meet({ colorScheme: 'dark' });
  const stamp = Date.now().toString(36);
  const account = async (label: string) => {
    const made = (await call(world, world.tokens.operator, 'POST', '/v0/principals', { kind: 'human', name: `ap-${label}-${stamp}` })).body as { principal: { id: number } };
    const issue = async () => ((await call(world, world.tokens.operator, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, { label: 'browser' })).body as { token: string }).token;
    return { name: `ap-${label}-${stamp}`, tokens: [await issue(), await issue()] };
  };
  const first = await account('a');
  const second = await account('b');
  const signOut = async () => {
    await page.getByRole('button', { name: 'Settings' }).click();
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await page.getByRole('button', { name: 'Yes, sign out' }).click();
    await expect(await credentialField(page)).toBeVisible();
  };
  const choose = async (theme: string, accent: string, density: string) => {
    await page.getByRole('button', { name: 'Settings' }).click();
    const drawer = page.getByRole('dialog', { name: 'Settings' });
    await drawer.getByLabel('Theme preset').selectOption(theme);
    await drawer.getByLabel('Accent color').selectOption(accent);
    await drawer.getByLabel('Chat density').selectOption(density);
    await page.keyboard.press('Escape');
  };

  await signIn(page, world, first.tokens[0] ?? '', first.name);
  expect(await rootAttributes(page), 'a new account starts at the defaults').toEqual(defaults);
  await choose('light', 'blue', 'compact');
  expect(await rootAttributes(page)).toEqual({ theme: 'light', accent: 'blue', density: 'compact' });
  await signOut();
  expect(await rootAttributes(page), 'signed out, the first account\'s choices are gone').toEqual(defaults);

  await signIn(page, world, second.tokens[0] ?? '', second.name);
  expect(await rootAttributes(page), 'the second account does not inherit the first\'s').toEqual(defaults);
  await expect(page.locator('body')).toHaveCSS('background-color', presets[1].bg);
  await choose('high-contrast', 'cyan', 'comfortable');
  await signOut();
  expect(await rootAttributes(page)).toEqual(defaults);

  await signIn(page, world, first.tokens[1] ?? '', first.name);
  expect(await rootAttributes(page), 'the first account gets its own choices back').toEqual({ theme: 'light', accent: 'blue', density: 'compact' });
  await expect(page.locator('body')).toHaveCSS('background-color', presets[0].bg);
  await expect(statusLine(page)).toContainText(first.name);
});

// A long history in a channel of its own, so the log scrolls.
async function longHistory(world: World, page: Page): Promise<string> {
  const tag = `dens-${Date.now().toString(36)}`;
  for (let i = 0; i < 80; i++) await post(world, world.tokens.nick, 'general', `${tag} line ${String(i).padStart(3, '0')} ${'word '.repeat(i % 7 === 0 ? 40 : 4)}`);
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(messages(page).getByRole('listitem').filter({ hasText: `${tag} line 079` })).toBeVisible();
  return tag;
}

const gap = (page: Page) => page.evaluate(() => {
  const log = document.querySelector('[role="log"][aria-label="Messages"]') as HTMLElement;
  return log.scrollHeight - log.scrollTop - log.clientHeight;
});

async function setDensity(page: Page, density: 'compact' | 'comfortable'): Promise<void> {
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('dialog', { name: 'Settings' }).getByLabel('Chat density').selectOption(density);
  await page.keyboard.press('Escape');
}

test('a reader at the bottom stays at the newest message across a density change', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1100, height: 600 }, colorScheme: 'dark' });
  const tag = await longHistory(world, page);
  await setDensity(page, 'compact');
  await expect.poll(() => gap(page)).toBeLessThanOrEqual(2);
  await setDensity(page, 'comfortable');
  await expect.poll(() => gap(page), { message: 'still at the bottom after Compact to Comfortable' }).toBeLessThanOrEqual(2);
  await expect(messages(page).getByRole('listitem').filter({ hasText: `${tag} line 079` })).toBeInViewport();
  await setDensity(page, 'compact');
  await expect.poll(() => gap(page), { message: 'still at the bottom after Comfortable to Compact' }).toBeLessThanOrEqual(2);
});

test('a reader scrolled up keeps the same message in view across a density change', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1100, height: 600 }, colorScheme: 'dark' });
  const tag = await longHistory(world, page);
  for (const [from, to] of [['comfortable', 'compact'], ['compact', 'comfortable']] as const) {
    await setDensity(page, from);
    // Scroll to a line in the middle and put it at the top of the view.
    const mid = messages(page).getByRole('listitem').filter({ hasText: `${tag} line 040` });
    await mid.evaluate((el) => {
      const log = el.closest('[role="log"]') as HTMLElement;
      log.scrollTop += el.getBoundingClientRect().top - log.getBoundingClientRect().top;
    });
    await expect.poll(() => gap(page)).toBeGreaterThan(100);
    const offset = () => mid.evaluate((el) => {
      const log = el.closest('[role="log"]') as HTMLElement;
      return el.getBoundingClientRect().top - log.getBoundingClientRect().top;
    });
    await page.waitForTimeout(100);
    const before = await offset();
    expect(before).toBeGreaterThanOrEqual(-2);
    await setDensity(page, to);
    await expect(mid).toBeInViewport();
    await expect.poll(offset, { message: `line 040 stays put going ${from} to ${to}` }).toBeLessThanOrEqual(before + 6);
    expect(await offset()).toBeGreaterThanOrEqual(before - 6);
    expect(await gap(page), 'not yanked to the bottom').toBeGreaterThan(50);
  }
});

test('with a thread expanded, a density change keeps the main log\'s own row on top', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1100, height: 600 }, colorScheme: 'dark' });
  const tag = `thr-${Date.now().toString(36)}`;
  const root = await post(world, world.tokens.nick, 'general', `${tag} root`);
  for (let i = 0; i < 50; i++) await post(world, world.tokens.nick, 'general', `${tag} filler ${String(i).padStart(2, '0')} ${'word '.repeat(i % 5 === 0 ? 30 : 3)}`);
  // The reply comes late in the log, and again inside its root's thread, far above.
  const answer = await reply(world, world.tokens.nick, 'general', root.id, `${tag} answer`);
  for (let i = 0; i < 20; i++) await post(world, world.tokens.nick, 'general', `${tag} after ${String(i).padStart(2, '0')}`);
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const mainRow = page.locator(`[role="log"][aria-label="Messages"] > .messages > li[data-message-id="${String(answer.id)}"]`);
  await expect(mainRow).toBeAttached();
  await page.locator(`[role="log"][aria-label="Messages"] > .messages > li[data-message-id="${String(root.id)}"]`).getByRole('button', { name: /^Reply to / }).click();
  const thread = page.getByRole('region', { name: /^Thread around / });
  await expect(thread.getByText(`${tag} answer`, { exact: true })).toBeVisible();
  await expect(page.locator(`[data-message-id="${String(answer.id)}"]`)).toHaveCount(2);
  await mainRow.evaluate((el) => {
    const log = el.closest('[role="log"]') as HTMLElement;
    log.scrollTop += el.getBoundingClientRect().top - log.getBoundingClientRect().top;
  });
  await page.waitForTimeout(150);
  const offset = () => mainRow.evaluate((el) => {
    const log = el.closest('[role="log"]') as HTMLElement;
    return el.getBoundingClientRect().top - log.getBoundingClientRect().top;
  });
  const before = await offset();
  expect(before).toBeGreaterThanOrEqual(-2);
  expect(before).toBeLessThan(40);
  for (const density of ['compact', 'comfortable'] as const) {
    await setDensity(page, density);
    await expect.poll(offset, { message: `the main-log occurrence stays on top at ${density}` }).toBeLessThanOrEqual(before + 6);
    expect(await offset()).toBeGreaterThanOrEqual(before - 6);
  }
});
