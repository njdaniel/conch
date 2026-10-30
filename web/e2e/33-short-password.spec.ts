// Issue #977 (with #975, the server half): there is no minimum password
// length beyond a non-empty one. A person joins by invite with a four-letter
// password, signs out, signs in with it, changes it to another short one in
// Settings, and has it replaced through an operator's reset link. Neither
// form says anything of 15 characters. The letters are unlikely in the
// page's own words, so the end-of-test secret scan means something.
import type { Page } from '@playwright/test';
import { makeInvite } from './api';
import { expect, fillAccount, signInWithPassword, statusLine, test } from './fixtures';
import { createSignInLinkResponseV1 } from '../src/schema/signin';
import { call, inviteUrl, ok, principalId } from './world';

const first = 'qzxj';
const second = 'vkbw';
const third = 'jqzv';

async function keepToken(page: Page, secrets: string[]): Promise<string> {
  const token = await page.evaluate(() => window.localStorage.getItem('conch.web.token'));
  if (token === null) throw new Error('no credential was kept');
  secrets.push(token);
  return token;
}

test('G977 a four-letter password creates an account, signs in, changes and resets', async ({ meet, secrets, world }) => {
  secrets.push(first, second, third);
  const invite = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general'], voice: false });
  const name = `g977-${String(Date.now())}`;
  const { page } = await meet();
  await page.goto(inviteUrl(world, invite.code));
  await expect(page.getByText(/at least 15|15 characters/)).toHaveCount(0);
  await fillAccount(page, name, first);
  await expect(statusLine(page)).toContainText('Connected');
  const firstToken = await keepToken(page, secrets);
  const id = await principalId(world, firstToken);

  // Sign out, then sign in with the short password.
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByRole('group', { name: /^Sign out of this browser\?/ }).getByRole('button', { name: 'Yes, sign out', exact: true }).click();
  await expect(page.getByLabel('Credential')).toBeVisible();
  await signInWithPassword(page, name, first);
  await expect(statusLine(page)).toContainText(name);
  await expect(statusLine(page)).toContainText('Connected');
  const secondToken = await keepToken(page, secrets);

  // Change it, in Settings, to another short one.
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Password', exact: true }).click();
  const region = page.getByRole('region', { name: 'Password' });
  await expect(region.getByText(/at least 15|15 characters/)).toHaveCount(0);
  await region.getByLabel('Current password').fill(first);
  await region.getByLabel('New password', { exact: true }).fill(second);
  await region.getByLabel('Type it again').fill(second);
  const changed = page.waitForResponse((r) => r.url().endsWith('/v1/password/mine') && r.request().method() === 'PUT');
  await region.getByRole('button', { name: 'Change password' }).click();
  expect((await changed).status()).toBe(200);
  // The change ended the sign-in it was made from (it is rotated). What the
  // page shows after is not this spec's concern, so it is not read here: the
  // server's answers are.
  expect((await call(world, secondToken, 'GET', '/v1/whoami')).status).toBe(401);
  // The old one no longer signs in, the new one does.
  const elsewhere = await meet();
  await elsewhere.page.goto(world.appUrl);
  await signInWithPassword(elsewhere.page, name, first);
  await expect(elsewhere.page.getByText('The name or password is not right.')).toBeVisible();
  await signInWithPassword(elsewhere.page, name, second);
  await expect(statusLine(elsewhere.page)).toContainText(name);

  // An operator's reset link sets a third.
  const link = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/sign-in-links`, createSignInLinkResponseV1, { purpose: 'reset' });
  const handset = await meet();
  await handset.page.goto(`${world.appUrl}#/signin/${encodeURIComponent(link.code)}`);
  await handset.page.getByLabel('New password', { exact: true }).fill(third);
  await handset.page.getByLabel('Type it again').fill(third);
  await handset.page.getByRole('button', { name: 'Set the new password' }).click();
  await expect(handset.page.getByRole('heading', { name: 'Your password is changed' })).toBeVisible();
  await handset.page.getByRole('button', { name: 'Go to sign-in' }).click();
  await signInWithPassword(handset.page, name, third);
  await expect(statusLine(handset.page)).toContainText(name);
  await expect(statusLine(handset.page)).toContainText('Connected');
  await keepToken(handset.page, secrets);
});
