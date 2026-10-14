// P3-02: a message is shown once to everyone who may see it, and the history
// agrees with what the pages show.
import { history, post } from './api';
import { channelButton, expect, messages, say, signIn, test } from './fixtures';

test('P3-02 a message sent in the browser appears once in both contexts and once in the history', async ({ meet, world }) => {
  const browser = (await meet()).page;
  const guest = (await meet()).page;
  await signIn(browser, world, world.tokens.browser, 'browser-test');
  await signIn(guest, world, world.tokens.guest, 'guest-test');
  await expect(channelButton(guest, 'general')).toBeVisible();

  const body = 'p3-02 from the browser, sent by a person';
  await say(browser, 'general', body);

  await expect(messages(browser).getByText(body)).toHaveCount(1);
  await expect(messages(guest).getByText(body)).toHaveCount(1);

  const stored = (await history(world, world.tokens.nick, 'general')).filter((m) => m.body === body);
  expect(stored).toHaveLength(1);

  // Still once after a reload: the history read and the live feed do not add up
  // to two.
  await browser.reload();
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
  await expect(messages(page).getByText(body)).toHaveCount(1);
  expect((await history(world, world.tokens.browser, 'general')).filter((m) => m.body === body)).toHaveLength(1);
});
