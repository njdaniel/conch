import type { Page } from '@playwright/test';
import { expect, messages, say, signIn, test } from './fixtures';

const accents = ['green', 'blue', 'violet', 'pink', 'orange', 'cyan'] as const;
const presets = [
  { value: 'light', bg: 'rgb(245, 247, 246)', surface: 'rgb(255, 255, 255)', text: 'rgb(23, 33, 27)' },
  { value: 'dark', bg: 'rgb(17, 20, 22)', surface: 'rgb(23, 27, 30)', text: 'rgb(232, 236, 233)' },
  { value: 'high-contrast', bg: 'rgb(0, 0, 0)', surface: 'rgb(0, 0, 0)', text: 'rgb(255, 255, 255)' },
] as const;

function rgb(hex: string): string {
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
