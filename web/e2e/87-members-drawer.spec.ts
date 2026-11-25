// Issue #2196: below the wide breakpoint the Members panel is drawn over the
// page with a backdrop, and it is a modal drawer: opened by keyboard, Tab and
// Shift+Tab never leave it (the rest of the page is inert, through the lobby's
// one inert owner), Escape closes it and focus returns to the Members button.
// At a wide window it is a side column and focus leaves it as before.
import type { Page } from '@playwright/test';
import { expect, openChannel, signIn, test } from './fixtures';

const members = (page: Page) => page.getByRole('button', { name: 'Members' });
const list = (page: Page) => page.getByRole('region', { name: 'Members list' });

// inDrawer says whether focus is inside the members panel now.
const inDrawer = (page: Page) => page.evaluate(() => document.getElementById('members-body')?.contains(document.activeElement) === true);

for (const [name, width, height] of [
  ['phone 390×844', 390, 844],
  ['tablet 1050×800', 1050, 800],
] as const) {
  test(`P2196 ${name}: Tab and Shift+Tab never leave the Members drawer, and Escape returns focus to Members`, async ({ meet, world }) => {
    const { page } = await meet({ viewport: { width, height } });
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await openChannel(page, 'general');
    await members(page).focus();
    await page.keyboard.press('Enter');
    await expect(list(page)).toBeFocused();
    for (const key of ['Tab', 'Shift+Tab']) {
      for (let i = 0; i < 30; i++) {
        await page.keyboard.press(key);
        expect(await inDrawer(page), `${key} #${String(i + 1)} left the drawer, at ${await page.evaluate(() => document.activeElement?.outerHTML.slice(0, 120) ?? 'nothing')}`).toBe(true);
      }
    }
    await page.keyboard.press('Escape');
    await expect(list(page)).toBeHidden();
    await expect(members(page)).toBeFocused();
  });
}

test('P2196 1440×900: the side column is not modal, and Shift+Tab leaves it', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await openChannel(page, 'general');
  await expect(list(page)).toBeVisible();
  await expect(page.locator('.centre')).not.toHaveAttribute('inert');
  await expect(page.locator('.topbar')).not.toHaveAttribute('inert');
  // Shift+Tab from the column's first stop goes to what precedes it on the
  // page (the column's length depends on how many people the world holds).
  await list(page).focus();
  await page.keyboard.press('Shift+Tab');
  expect(await inDrawer(page), 'Shift+Tab stayed in the side column').toBe(false);
  await expect(page.locator('body')).not.toBeFocused();
});
