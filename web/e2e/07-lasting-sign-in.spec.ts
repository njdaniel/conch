// A7 (issue #439): a member's browser sign-in lasts beyond the tab, and sign
// out everywhere ends it everywhere. Each test signs in a person of its own,
// made here by the operator, so revoking their credentials touches nothing
// another spec uses.
import type { Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { expect, signIn, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

interface Newcomer {
  readonly id: number;
  readonly name: string;
  readonly tokens: readonly string[];
}

// newcomer makes a person with `count` renewable credentials, as an invite
// would give them.
async function newcomer(world: World, label: string, count: number): Promise<Newcomer> {
  const name = `a7-${label}-${String(Date.now())}`;
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const made = await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request);
  const tokens: string[] = [];
  for (let i = 0; i < count; i++) {
    const issue: CreateCredentialRequestV1 = { label: `browser ${String(i + 1)}`, renew_lifetime_seconds: 90 * 24 * 60 * 60 };
    const issued = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, createCredentialResponseV1, issue);
    expect(issued.credential.renews).toBe(true);
    tokens.push(issued.token);
  }
  return { id: made.principal.id, name, tokens };
}

// signedIn waits for the lobby, signed in as name.
async function signedIn(page: Page, name: string): Promise<void> {
  await expect(statusLine(page)).toContainText(name);
  await expect(statusLine(page)).toContainText('Connected');
}

// The fixture checks the world's credentials; these are this spec's own.
async function expectNotShown(page: Page, tokens: readonly string[]): Promise<void> {
  if (page.isClosed()) return;
  const shown = await page.evaluate(() => [location.href, document.documentElement.outerHTML, JSON.stringify(performance.getEntries().map((e) => e.name))].join('\n'));
  for (const token of tokens) expect(shown, 'a credential in the address, the page or performance.getEntries()').not.toContain(token);
}

test('A7 a member signs in, closes the page and the browser, and is still signed in; a second tab is too', async ({ browser, meet, world }) => {
  const person = await newcomer(world, 'lasting', 1);
  const token = person.tokens[0] ?? '';
  const { context, page } = await meet();
  await signIn(page, world, token, person.name);

  // Closing the page, then opening a new one: still signed in, no field.
  await page.close();
  const again = await context.newPage();
  await again.goto(world.appUrl);
  await signedIn(again, person.name);

  // A second tab of the same browser is signed in too.
  const second = await context.newPage();
  await second.goto(world.appUrl);
  await signedIn(second, person.name);

  // Closing the whole browser keeps it: a new browser context with what the
  // first one stored is signed in at once.
  const kept = await context.storageState();
  const reopened = await browser.newContext({ storageState: kept });
  try {
    const page3 = await reopened.newPage();
    await page3.goto(world.appUrl);
    await signedIn(page3, person.name);
    await expectNotShown(page3, person.tokens);
  } finally {
    await reopened.close();
  }
  for (const p of [again, second]) await expectNotShown(p, person.tokens);
});

test('A7 sign out everywhere ends every sign-in: the other tab lands on sign-in, the other browser is refused', async ({ meet, world }) => {
  const person = await newcomer(world, 'everywhere', 2);
  const [first, other] = [person.tokens[0] ?? '', person.tokens[1] ?? ''];

  // One browser with two tabs, and a second browser on the other credential.
  const here = await meet();
  await signIn(here.page, world, first, person.name);
  const tab2 = await here.context.newPage();
  await tab2.goto(world.appUrl);
  await signedIn(tab2, person.name);
  const elsewhere = await meet();
  await signIn(elsewhere.page, world, other, person.name);

  await here.page.getByRole('button', { name: 'Sign out everywhere' }).click();
  await expect(here.page.getByRole('alert')).toHaveText('Signed out everywhere: every sign-in of yours has ended');
  await expect(here.page.getByLabel('Credential')).toBeVisible();

  // The other tab follows at once, without a request of its own.
  await expect(tab2.getByLabel('Credential')).toBeVisible();
  await expect(tab2.getByRole('alert')).toHaveText('Signed out in another tab');

  // Both credentials are dead on the server, and the other browser is
  // refused on its next request: a reload lands it on sign-in.
  for (const token of person.tokens) expect((await call(world, token, 'GET', '/v1/whoami')).status).toBe(401);
  await elsewhere.page.reload();
  await expect(elsewhere.page.getByLabel('Credential')).toBeVisible();
  await expect(elsewhere.page.getByRole('alert')).toHaveText('Signed out: the credential is no longer valid');

  // Nothing of the credential is left in either browser.
  for (const p of [here.page, tab2, elsewhere.page]) {
    expect(await p.evaluate(() => window.localStorage.getItem('conch.web.token') ?? window.sessionStorage.getItem('conch.web.token'))).toBeNull();
    await expectNotShown(p, person.tokens);
  }
});

test('A7 sign out revokes this credential on the server, and its next request is refused', async ({ meet, world }) => {
  const person = await newcomer(world, 'signout', 2);
  const [first, other] = [person.tokens[0] ?? '', person.tokens[1] ?? ''];
  const { page } = await meet();
  await signIn(page, world, first, person.name);

  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await expect(page.getByLabel('Credential')).toBeVisible();
  expect((await call(world, first, 'GET', '/v1/whoami')).status).toBe(401);
  // Only that one: the person's other credential still works.
  expect((await call(world, other, 'GET', '/v1/whoami')).status).toBe(200);
  await expectNotShown(page, person.tokens);
});
