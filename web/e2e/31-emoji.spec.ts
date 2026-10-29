// Issue #763: the composer's emoji picker and :shortcode: completion, at a
// desktop window and at the four phone sizes. The message that is sent holds
// the catalog's fully-qualified key, and the picker fits the screen and is
// reachable without a keyboard.
import { phoneSizes, expect, expectFits, messages, signIn, test } from './fixtures';
import type { Page } from '@playwright/test';

let next = 0;
const unique = (label: string) => `e763-${label}-${Date.now().toString(36)}-${String(++next)}`;

const composer = (page: Page) => page.getByRole('textbox', { name: 'Message #general' });

test('desktop: the picker inserts at the caret, and :short completes by keyboard', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const tag = unique('desktop');
  const box = composer(page);
  await box.fill(`${tag} ab`);
  await box.evaluate((el: HTMLTextAreaElement) => { el.setSelectionRange(el.value.length - 1, el.value.length - 1); });

  await page.getByRole('button', { name: 'Add emoji' }).click();
  const dialog = page.getByRole('dialog', { name: 'Emoji' });
  await expect(dialog.getByRole('searchbox', { name: 'Search emoji' })).toBeFocused();
  await dialog.getByRole('searchbox').fill('red heart');
  await dialog.getByRole('button', { name: 'red heart', exact: true }).click();
  await expect(dialog).toBeHidden();
  await expect(box).toBeFocused();
  await expect(box).toHaveValue(`${tag} a❤️b`);

  // Escape closes the picker and returns focus to its button.
  await page.getByRole('button', { name: 'Add emoji' }).click();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(page.getByRole('button', { name: 'Add emoji' })).toBeFocused();

  // :fire completes with the arrow keys and Enter, and Enter then sends.
  await box.fill(`${tag} go :fire`);
  const list = page.getByRole('listbox', { name: 'Emoji suggestions' });
  await expect(list).toBeVisible();
  await expect(list.getByRole('option').first()).toContainText(':fire:');
  await page.keyboard.press('Enter');
  await expect(list).toBeHidden();
  await expect(box).toHaveValue(`${tag} go 🔥`);
  await page.keyboard.press('Enter');
  await expect(messages(page).getByText(`${tag} go 🔥`, { exact: true })).toBeVisible();
});

for (const [size, options] of phoneSizes) {
  test(`the picker fits and works by touch at ${size}`, async ({ meet, world }) => {
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    const tag = unique(size);
    const box = composer(page);
    await box.fill(`${tag} `);

    await page.getByRole('button', { name: 'Add emoji' }).tap();
    const dialog = page.getByRole('dialog', { name: 'Emoji' });
    await expect(dialog).toBeVisible();
    await expectFits(page, 'the composer with the emoji picker open');
    // The whole picker is on the screen: no part of it is wider than the window.
    const width = page.viewportSize()?.width ?? 0;
    const frame = await dialog.boundingBox();
    expect(frame?.x ?? -1).toBeGreaterThanOrEqual(0);
    expect((frame?.x ?? 0) + (frame?.width ?? width + 1)).toBeLessThanOrEqual(width);
    // A cell is a touch target a finger can hit.
    const cell = dialog.getByRole('button', { name: 'grinning face', exact: true });
    await expect(cell).toBeVisible();
    expect((await cell.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(32);

    await dialog.getByRole('searchbox').fill('thumbs up');
    await dialog.getByRole('button', { name: 'thumbs up', exact: true }).tap();
    await expect(dialog).toBeHidden();
    await expect(box).toHaveValue(`${tag} 👍`);

    // Completion's list fits too, and is chosen by touch.
    await box.fill(`${tag} :party_pop`);
    const list = page.getByRole('listbox', { name: 'Emoji suggestions' });
    await expect(list).toBeVisible();
    await expectFits(page, 'the composer with suggestions open');
    await list.getByRole('option').first().tap();
    await expect(box).toHaveValue(`${tag} 🎉`);

    await page.getByRole('button', { name: 'Send' }).tap();
    await expect(messages(page).getByText(`${tag} 🎉`, { exact: true })).toBeVisible();
  });
}
