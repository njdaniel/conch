// P3-01: the browser is one of the people, with a credential of its own.
import { channelButton, expect, signIn, statusLine, test } from './fixtures';
import { principalId } from './world';

test('P3-01 browser-test signs in by pasting the credential and sees the fixture channels', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');

  // The channels browser-test is a member of; test-private is not one of them.
  await expect(channelButton(page, 'general')).toBeVisible();
  await expect(channelButton(page, 'nets-demo')).toBeVisible();
  await expect(channelButton(page, 'test-private')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();

  // The page says who the server says this is, not a name of its own.
  expect(await principalId(world, world.tokens.browser)).toBeGreaterThan(0);
  await expect(statusLine(page)).toContainText(world.origin);
});

test('P3-01 a wrong credential shows the failure line and leaves no token in the page', async ({ meet, world }) => {
  const { page } = await meet();
  const wrong = 'not-a-credential-p3-01-wrong-token';
  await page.goto(world.appUrl);
  const field = page.getByLabel('Credential');
  await field.fill(wrong);
  await page.getByRole('button', { name: 'Sign in' }).click();

  await expect(page.getByRole('alert')).toHaveText('That credential was not accepted');
  // Still the sign-in page, with the field emptied.
  await expect(page.getByRole('button', { name: 'Sign in' })).toBeVisible();
  await expect(field).toHaveValue('');

  // Nothing of it is left: not in the page, the address, the tab's storage or
  // the requests the page made (performance.getEntries() is checked, for every
  // credential, when the test ends).
  expect(await page.evaluate(() => document.documentElement.outerHTML)).not.toContain(wrong);
  expect(page.url()).not.toContain(wrong);
  expect(await page.evaluate(() => window.sessionStorage.length)).toBe(0);
});
