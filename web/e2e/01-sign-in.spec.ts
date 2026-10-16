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
  expect(await page.evaluate(() => window.localStorage.length)).toBe(0);
});

// A7 (issue #439): an operator's credential is still kept for one tab only,
// after the warning. The test does not sign out: that would revoke the
// world's operator credential, which later specs use.
test('A7 an operator is warned, and the credential stays with that one tab', async ({ meet, world }) => {
  const { context, page } = await meet();
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { name: 'This is an operator credential' })).toBeVisible();
  // Nothing is kept until the warning is accepted.
  expect(await page.evaluate(() => window.sessionStorage.length + window.localStorage.length)).toBe(0);
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
  expect(await page.evaluate(() => window.localStorage.length)).toBe(0);
  expect(await page.evaluate(() => window.sessionStorage.getItem('conch.web.token') !== null)).toBe(true);

  // Another tab of the same browser is not signed in.
  const other = await context.newPage();
  await other.goto(world.appUrl);
  await expect(other.getByLabel('Credential')).toBeVisible();
  await other.close();
});
