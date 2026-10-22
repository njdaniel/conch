// Issue #645 adds: signing out ends the same session before it revokes the
// credential, so nothing is answered 404. Issue #622: a call whose answer is a status alone (a 204) must finish
// cleanly in Chromium. A body the page never reads is logged as a failed
// request (net::ERR_ABORTED); the request helper reads it to the end. This
// spec closes an invite and signs out, and fails on any request to conchd
// that the browser reports as failed. The person is made through the API, and
// no invite is redeemed and no sign-in link used, so nothing of the shared
// per-address budget is spent.
import type { Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import type { InvitePermissionRequestV1 } from '../src/schema/generated';
import { roster } from './api';
import { expect, signIn, test } from './fixtures';
import { call, ok, type World } from './world';

async function done(world: World, token: string, method: 'POST' | 'PUT', path: string, body?: unknown): Promise<void> {
  const reply = await call(world, token, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
}

// watch records every request to conchd that failed, and every answer of 400
// or more (the browser logs both as console errors).
function watch(page: Page, world: World): string[] {
  const bad: string[] = [];
  page.on('requestfailed', (req) => {
    const url = new URL(req.url());
    if (url.origin !== world.origin) return;
    bad.push(`${req.method()} ${url.pathname} ${req.failure()?.errorText ?? ''}`);
  });
  page.on('response', (res) => {
    const url = new URL(res.url());
    if (url.origin !== world.origin || res.status() < 400) return;
    bad.push(`${res.request().method()} ${url.pathname} ${String(res.status())}`);
  });
  return bad;
}

// aPerson makes a member of #general, with a credential, through the API.
async function aPerson(world: World, secrets: string[], prefix: string): Promise<{ id: number; name: string; token: string }> {
  const op = world.tokens.operator;
  const person: CreatePrincipalRequest = { kind: 'human', name: `${prefix}-${String(Date.now())}` };
  const id = (await ok(world, op, 'POST', '/v0/principals', createPrincipalResponse, person)).principal.id;
  const label: CreateCredentialRequestV1 = { label: person.name };
  const { token } = await ok(world, op, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, label);
  secrets.push(token);
  await done(world, op, 'PUT', `/v1/channels/general/members/${String(id)}`);
  return { id, name: person.name, token };
}

test('S645 signing out everywhere ends the session first and logs no failed request or 4xx', async ({ meet, secrets, world }) => {
  const who = await aPerson(world, secrets, 's645');
  const { page } = await meet();
  const bad = watch(page, world);
  await signIn(page, world, who.token, who.name);
  await expect
    .poll(async () => (await roster(world, world.tokens.guest, 'general')).filter((s) => s.name === who.name && s.state === 'online').length)
    .toBe(1);

  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Sign out everywhere', exact: true }).click();
  await page.getByRole('group', { name: /^Sign out on every device/ }).getByRole('button', { name: 'Yes, sign out everywhere', exact: true }).click();
  await expect(page.getByLabel('Credential')).toBeVisible();
  expect((await call(world, who.token, 'GET', '/v1/whoami')).status).toBe(401);

  await page.waitForTimeout(500);
  expect(bad, 'requests to conchd that failed or were refused').toEqual([]);
  // Another person's members column shows the session offline.
  await expect
    .poll(async () => (await roster(world, world.tokens.guest, 'general')).filter((s) => s.name === who.name && s.state === 'online').length, { timeout: 30_000 })
    .toBe(0);
});

test('S622 closing an invite and signing out leave no failed request', async ({ meet, secrets, world }) => {
  const op = world.tokens.operator;
  const person: CreatePrincipalRequest = { kind: 'human', name: `s622-${String(Date.now())}` };
  const id = (await ok(world, op, 'POST', '/v0/principals', createPrincipalResponse, person)).principal.id;
  const label: CreateCredentialRequestV1 = { label: person.name };
  const { token } = await ok(world, op, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, label);
  secrets.push(token);
  const mayInvite: InvitePermissionRequestV1 = { may_invite: true };
  await done(world, op, 'PUT', `/v1/principals/${String(id)}/invite-permission`, mayInvite);
  await done(world, op, 'PUT', `/v1/channels/general/members/${String(id)}`);

  const { page } = await meet();
  const failed = watch(page, world);
  await signIn(page, world, token, person.name);
  // The page's own session is online before it signs out (issue #645).
  await expect
    .poll(async () => (await roster(world, world.tokens.guest, 'general')).filter((s) => s.name === person.name && s.state === 'online').length)
    .toBe(1);

  // Close an invite: made in the panel, then closed.
  const panel = page.getByRole('region', { name: 'Invites' });
  await panel.getByRole('button', { name: 'Invite someone' }).click();
  await panel.getByRole('checkbox', { name: '#general' }).check();
  await panel.getByRole('button', { name: 'Make invite' }).click();
  const list = panel.getByRole('list', { name: 'Your invites' });
  await expect(list.getByRole('listitem')).toHaveCount(1);
  await list.getByRole('button', { name: 'Close' }).click();
  await expect(list.getByRole('listitem').first()).toContainText('Closed');

  // Sign out, in Settings.
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByRole('group', { name: /^Sign out of this browser\?/ }).getByRole('button', { name: 'Yes, sign out', exact: true }).click();
  await expect(page.getByLabel('Credential')).toBeVisible();
  expect((await call(world, token, 'GET', '/v1/whoami')).status).toBe(401);

  // The browser logs a failed request a few milliseconds after the response.
  await page.waitForTimeout(500);
  expect(failed, 'requests to conchd that failed or were refused').toEqual([]);
  // The session still ended on the server: another person's members column
  // shows the person offline.
  await expect
    .poll(async () => (await roster(world, world.tokens.guest, 'general')).filter((s) => s.name === person.name && s.state === 'online').length, { timeout: 30_000 })
    .toBe(0);
});
