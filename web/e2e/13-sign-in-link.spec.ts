// Issue #493: a person who joined by invite and signed out comes back into
// the same account through a one-time link an operator makes for them, on
// another device. The operator makes it through the API, as
// `conch admin principal sign-in-link` does. Runs after 12-phone, so the
// people it adds to general touch no other spec.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { makeInvite } from './api';
import { accountPassword, channelButton, expect, fillAccount, showView, signInWithPassword, statusLine, test } from './fixtures';
import { createSignInLinkResponseV1, type CreateSignInLinkRequestV1, type CreateSignInLinkResponseV1 } from '../src/schema/signin';
import { createPrincipalResponse, type CreatePrincipalRequest } from '../src/schema/credential';
import { call, inviteUrl, ok, principalId, type World } from './world';

const desktop: BrowserContextOptions = { viewport: { width: 1280, height: 800 } };
const phone: BrowserContextOptions = { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true };

const unique = (label: string) => `b493-${label}-${String(Date.now())}`;

// signInUrl is the link `conch admin principal sign-in-link` prints.
const signInUrl = (world: World, signInCode: string) => `${world.appUrl}#/signin/${encodeURIComponent(signInCode)}`;

function makeSignInLink(world: World, principal: number, request: CreateSignInLinkRequestV1 = {}): Promise<CreateSignInLinkResponseV1> {
  return ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(principal)}/sign-in-links`, createSignInLinkResponseV1, request);
}

// keepToken reads the credential this page keeps and hands it to the
// fixture's end-of-test scan.
async function keepToken(page: Page, secrets: string[]): Promise<string> {
  const token = await page.evaluate(() => window.localStorage.getItem('conch.web.token'));
  if (token === null) throw new Error('no credential was kept');
  secrets.push(token);
  return token;
}

// expectCodeGone checks that the code is not in the address, the page, the
// storage or the addresses the page asked for. (The navigation timing entry
// keeps the address the document loaded from, as for an invite: 11-invites
// says why, and it is left out here too.)
async function expectCodeGone(page: Page, signInCode: string): Promise<void> {
  const where = await page.evaluate(() =>
    [
      location.href,
      document.documentElement.outerHTML,
      JSON.stringify(performance.getEntries().filter((e) => e.entryType !== 'navigation').map((e) => e.name)),
      ...[window.localStorage, window.sessionStorage].flatMap((s) => Object.keys(s).map((k) => `${k}=${s.getItem(k) ?? ''}`)),
    ].join('\n'),
  );
  expect(where, 'the code in the address, the page, the storage or performance.getEntries()').not.toContain(signInCode);
}

test('B493 a joiner signs out, is warned first, and comes back into the same account through an operator link on another device', async ({ meet, secrets, world }) => {
  // A tester joins by invite.
  const invite = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general'], voice: false });
  const name = unique('back');
  const laptop = await meet(desktop);
  await laptop.page.goto(inviteUrl(world, invite.code));
  await fillAccount(laptop.page, name);
  await expect(statusLine(laptop.page)).toContainText('Connected');
  const firstToken = await keepToken(laptop.page, secrets);
  const id = await principalId(world, firstToken);

  // Sign out, in Settings (issue #536), says what it costs before it does it,
  // and Cancel keeps it all. A joiner has a password now (issue #713), so
  // coming back is signing in with it; the operator's link below is the other
  // way, for someone who cannot.
  await laptop.page.getByRole('button', { name: 'Settings' }).click();
  await laptop.page.getByRole('button', { name: 'Sign out', exact: true }).click();
  const asked = laptop.page.getByRole('group', { name: /^Sign out of this browser\?/ });
  await expect(asked).toContainText('sign in again with your name and password');
  // Scoped to the question: the members panel may list runs whose names
  // also say "cancel".
  await asked.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect((await call(world, firstToken, 'GET', '/v1/whoami')).status).toBe(200);
  await laptop.page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await asked.getByRole('button', { name: 'Yes, sign out', exact: true }).click();
  await expect(laptop.page.getByLabel('Credential')).toBeVisible();
  expect((await call(world, firstToken, 'GET', '/v1/whoami')).status).toBe(401);

  // The operator makes a sign-in link; the tester opens it on a phone.
  const link = await makeSignInLink(world, id);
  expect(link.sign_in_link.principal_id).toBe(id);
  const handset = await meet(phone);
  await handset.page.goto(signInUrl(world, link.code));
  await expect(handset.page.getByRole('heading', { name: 'Sign in to Conch' })).toBeVisible();
  await expect(handset.page.getByText(/signs you back into your account/)).toContainText(name);
  expect(handset.page.url()).not.toContain('#');
  await handset.page.getByRole('button', { name: `Sign in as ${name}` }).click();
  await expect(statusLine(handset.page)).toContainText(name);
  await expect(statusLine(handset.page)).toContainText('Connected');
  await showView(handset.page, 'Channels');
  await expect(channelButton(handset.page, 'general')).toBeVisible();

  // The same account, on a new credential that lasts.
  const secondToken = await keepToken(handset.page, secrets);
  expect(secondToken).not.toBe(firstToken);
  expect(await principalId(world, secondToken)).toBe(id);
  await expectCodeGone(handset.page, link.code);

  // The link worked once: opened again, in another browser, it says so.
  const again = await meet(desktop);
  await again.page.goto(signInUrl(world, link.code));
  await expect(again.page.getByRole('heading', { name: 'This sign-in link no longer works' })).toBeVisible();
  await expect(again.page.getByRole('button', { name: /^Sign in as/ })).toHaveCount(0);
  await expectCodeGone(again.page, link.code);
});

// G7a (issue #714, design §6.1): a member who forgot their password gets a
// reset link from an operator, who made it after checking who they are
// (here through the API, as `conch admin principal sign-in-link --purpose
// reset` does). It replaces the password and signs nobody in; every sign-in
// of the account ends, the old password stops working, and the new one
// signs in from a fresh browser.
test('G7a a reset link replaces a forgotten password, ends every sign-in, and the new password signs in', async ({ meet, secrets, world }) => {
  const fresh = 'the new passphrase this member chose after a reset';
  secrets.push(accountPassword, fresh);
  const invite = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general'], voice: false });
  const name = unique('reset');
  const laptop = await meet(desktop);
  await laptop.page.goto(inviteUrl(world, invite.code));
  await fillAccount(laptop.page, name);
  await expect(statusLine(laptop.page)).toContainText('Connected');
  const firstToken = await keepToken(laptop.page, secrets);
  const id = await principalId(world, firstToken);

  const link = await makeSignInLink(world, id, { purpose: 'reset' });
  expect(link.sign_in_link.purpose).toBe('reset');
  const handset = await meet(phone);
  await handset.page.goto(signInUrl(world, link.code));
  await expect(handset.page.getByRole('heading', { name: 'Choose a new password' })).toBeVisible();
  await expect(handset.page.getByText(/Choose a new password for/)).toContainText(name);
  // Opening the link used nothing: the old sign-in still works.
  expect((await call(world, firstToken, 'GET', '/v1/whoami')).status).toBe(200);
  await handset.page.getByLabel('New password', { exact: true }).fill(fresh);
  await handset.page.getByLabel('Type it again').fill(fresh);
  await handset.page.getByRole('button', { name: 'Set the new password' }).click();
  await expect(handset.page.getByRole('heading', { name: 'Your password is changed' })).toBeVisible();
  // It signed nobody in, and ended every sign-in of the account.
  expect(await handset.page.evaluate(() => window.localStorage.length + window.sessionStorage.length)).toBe(0);
  expect((await call(world, firstToken, 'GET', '/v1/whoami')).status).toBe(401);
  await expectCodeGone(handset.page, link.code);

  // The old password no longer signs in; the new one does.
  await handset.page.getByRole('button', { name: 'Go to sign-in' }).click();
  await signInWithPassword(handset.page, name, accountPassword);
  await expect(handset.page.getByText('The name or password is not right.')).toBeVisible();
  await signInWithPassword(handset.page, name, fresh);
  await expect(statusLine(handset.page)).toContainText(name);
  await expect(statusLine(handset.page)).toContainText('Connected');
  await keepToken(handset.page, secrets);

  // The link worked once.
  const again = await meet(desktop);
  await again.page.goto(signInUrl(world, link.code));
  await expect(again.page.getByRole('heading', { name: 'This sign-in link no longer works' })).toBeVisible();
});

// The person is made by the operator here: the spec is about the link, and
// an invite would only add a redemption that has nothing to do with it.
test('B493 an expired sign-in link says it no longer works', async ({ meet, world }) => {
  const request: CreatePrincipalRequest = { kind: 'human', name: unique('late') };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;

  const soon = new Date(Date.now() + 2_000).toISOString();
  const link = await makeSignInLink(world, id, { expires_at: soon });
  await new Promise((resolve) => setTimeout(resolve, 3_000));
  const late = await meet(desktop);
  await late.page.goto(signInUrl(world, link.code));
  await expect(late.page.getByRole('heading', { name: 'This sign-in link no longer works' })).toBeVisible();
  await expect(late.page.getByRole('button', { name: /^Sign in as/ })).toHaveCount(0);
  await expectCodeGone(late.page, link.code);
});
