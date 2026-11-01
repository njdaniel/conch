// P3-02: a message is shown once to everyone who may see it, and the history
// agrees with what the pages show.
import type { Page } from '@playwright/test';
import { history, post } from './api';
import { channelButton, expect, messages, say, signIn, test } from './fixtures';
import type { World } from './world';

// settled waits until the page has caught up with the server: a message posted
// now is on the page, so the replay that follows a reload has landed too. A
// count taken before that could be one of the history alone.
async function settled(world: World, page: Page, sentinel: string): Promise<void> {
  await post(world, world.tokens.nick, 'general', sentinel);
  await expect(messages(page).getByText(sentinel)).toHaveCount(1);
}

test('P3-02 a message sent in the browser appears once in both contexts and once in the history', async ({ meet, world }) => {
  const browser = (await meet()).page;
  const guest = (await meet()).page;
  await signIn(browser, world, world.tokens.browser, 'browser-test');
  await signIn(guest, world, world.tokens.guest, 'jamie-test');
  await expect(channelButton(guest, 'general')).toBeVisible();

  const body = 'p3-02 from the browser, sent by a person';
  await say(browser, 'general', body);

  await expect(messages(guest).getByText(body)).toHaveCount(1);
  // The sender's own pending line counts as one before the server has answered;
  // look again now that a second context has the stored message, so that a
  // duplicate from the reply and the socket frame would show.
  await expect(messages(browser).getByText(body)).toHaveCount(1);

  const stored = (await history(world, world.tokens.nick, 'general')).filter((m) => m.body === body);
  expect(stored).toHaveLength(1);

  // Still once after a reload: the history read and the live feed do not add up
  // to two.
  await browser.reload();
  await settled(world, browser, 'p3-02 sentinel after the first reload');
  await expect(messages(browser).getByText(body)).toHaveCount(1);
  await expect(messages(guest).getByText(body)).toHaveCount(1);
});

test('P3-02 a message posted through the API as nick-test appears once in the browser', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');

  const body = 'p3-02 from nick-test, posted through the API';
  await post(world, world.tokens.nick, 'general', body);

  await expect(messages(page).getByText(body)).toHaveCount(1);
  await expect(messages(page).getByRole('listitem').filter({ hasText: body })).toContainText('nick-test');

  await page.reload();
  await settled(world, page, 'p3-02 sentinel after the second reload');
  await expect(messages(page).getByText(body)).toHaveCount(1);
  expect((await history(world, world.tokens.browser, 'general')).filter((m) => m.body === body)).toHaveLength(1);
});

test('F14 a typed draft survives a reload, and sending it leaves an empty box after the next', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const box = page.getByLabel('Message #general');
  await box.fill('a draft I was typing');

  await page.reload();
  await expect(page.getByLabel('Message #general')).toHaveValue('a draft I was typing');

  const body = 'f14 sent after a reload';
  await page.getByLabel('Message #general').fill(body);
  await page.getByRole('button', { name: 'Send' }).click();
  await expect(messages(page).getByText(body)).toHaveCount(1);
  await page.reload();
  await expect(page.getByLabel('Message #general')).toHaveValue('');
});

test('F26 a posted URL is a link that opens safely, and a javascript: string is not', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');

  await say(page, 'general', 'f26 read https://example.com/a?b=1.');
  const line = messages(page).getByRole('listitem').filter({ hasText: 'f26 read' });
  const link = line.getByRole('link', { name: 'https://example.com/a?b=1' });
  await expect(link).toHaveAttribute('href', 'https://example.com/a?b=1');
  await expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  await expect(link).toHaveAttribute('target', '_blank');
  await expect(line).toContainText('https://example.com/a?b=1.');

  await say(page, 'general', 'f26 never javascript:alert(1) or <b>tags</b>');
  const plain = messages(page).getByRole('listitem').filter({ hasText: 'f26 never' });
  await expect(plain).toContainText('javascript:alert(1) or <b>tags</b>');
  await expect(plain.getByRole('link')).toHaveCount(0);
});
