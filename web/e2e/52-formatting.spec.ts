import type { Locator, Page } from '@playwright/test';
import { history } from './api';
import { expect, messages, signIn, test } from './fixtures';

async function select(box: Locator, start: number, end: number): Promise<void> {
  await box.evaluate((element, range) => {
    const input = element as HTMLTextAreaElement;
    input.focus();
    input.setSelectionRange(range.start, range.end);
  }, { start, end });
}

test('F1496 two accounts send and read formatted text and a keyboard-accessible spoiler', async ({ meet, world }) => {
  const sender = (await meet()).page;
  const recipient = (await meet()).page;
  await signIn(sender, world, world.tokens.browser, 'browser-test');
  await signIn(recipient, world, world.tokens.guest, 'jamie-test');

  const tag = `f1496-${Date.now().toString(36)}`;
  const formatted = `${tag} **bold**`;
  const box = sender.getByLabel('Message #general');
  await box.fill(`${tag} bold`);
  await select(box, tag.length + 1, tag.length + 5);
  await box.press('Control+B');
  await expect(box).toHaveValue(formatted);
  await box.press('Enter');

  for (const page of [sender, recipient]) {
    const line = messages(page).getByRole('listitem').filter({ hasText: tag });
    await expect(line.locator('strong')).toHaveText('bold');
    await expect(line).not.toContainText('**bold**');
  }
  expect((await history(world, world.tokens.guest, 'general')).filter((message) => message.body === formatted)).toHaveLength(1);

  const spoilerTag = `${tag}-spoiler`;
  const secret = `secret-${Date.now().toString(36)}`;
  await box.fill(`${spoilerTag} ${secret}`);
  await select(box, spoilerTag.length + 1, spoilerTag.length + 1 + secret.length);
  await sender.getByRole('button', { name: 'Spoiler', exact: true }).click();
  await expect(box).toHaveValue(`${spoilerTag} ||${secret}||`);
  await box.press('Enter');

  const received = messages(recipient).getByRole('listitem').filter({ hasText: spoilerTag });
  const reveal = received.getByRole('button', { name: 'Reveal spoiler' });
  await expect(reveal).toHaveAttribute('aria-expanded', 'false');
  await expect(messages(recipient).getByText(secret)).toHaveCount(0);
  await expect(messages(sender).getByText(secret)).toHaveCount(0);

  await reveal.focus();
  await reveal.press('Enter');
  await expect(received.getByText(secret)).toBeVisible();
  const hide = received.getByRole('button', { name: 'Hide spoiler' });
  await expect(hide).toHaveAttribute('aria-expanded', 'true');
  await hide.press('Space');
  await expect(received.getByRole('button', { name: 'Reveal spoiler' })).toBeFocused();
  await expect(messages(recipient).getByText(secret)).toHaveCount(0);
  expect((await history(world, world.tokens.guest, 'general')).filter((message) => message.body === `${spoilerTag} ||${secret}||`)).toHaveLength(1);
});

async function boxHeight(page: Page): Promise<number> {
  return page.locator('.composer-box').evaluate((el) => el.getBoundingClientRect().height);
}

test('F1496 the toolbar adds no height to the composer, and is one tab stop', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const toolbar = page.getByRole('toolbar', { name: 'Formatting' });
  await expect(toolbar).toBeVisible();
  const with_ = await boxHeight(page);
  const raw = page.locator('.composer-formatting');
  await raw.evaluate((el) => { el.style.display = 'none'; });
  expect(with_).toBe(await boxHeight(page));
  await raw.evaluate((el) => { el.style.display = ''; });
  const stops = await toolbar.evaluate((el) => [...el.querySelectorAll('button')].filter((b) => b.tabIndex === 0).length);
  expect(stops).toBe(1);
  await toolbar.getByRole('button', { name: 'Bold' }).focus();
  await page.keyboard.press('ArrowRight');
  await expect(toolbar.getByRole('button', { name: 'Italic' })).toBeFocused();
});

test('F1496 on a phone the toolbar is closed until Formatting opens it', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 390, height: 844 } });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const toggle = page.getByRole('button', { name: 'Formatting', exact: true });
  const toolbar = page.getByRole('toolbar', { name: 'Formatting' });
  await expect(toggle).toBeVisible();
  await expect(toolbar).toBeHidden();
  const closed = await boxHeight(page);
  await toggle.click();
  await expect(toolbar).toBeVisible();
  expect(await boxHeight(page)).toBe(closed);
  await page.keyboard.press('Escape');
  await expect(toolbar).toBeHidden();
  await toggle.click();
  await toolbar.getByRole('button', { name: 'Bold' }).click();
  await expect(toolbar).toBeHidden();
});
