// Issue #622: a call whose answer is a status alone (a 204) must finish
// cleanly in Chromium. A body the page never reads is logged as a failed
// request (net::ERR_ABORTED); the request helper reads it to the end. This
// spec closes an invite and signs out, and fails on any request to conchd
// that the browser reports as failed. The person is made through the API, and
// no invite is redeemed and no sign-in link used, so nothing of the shared
// per-address budget is spent.
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import type { InvitePermissionRequestV1 } from '../src/schema/generated';
import { expect, signIn, test } from './fixtures';
import { call, ok, type World } from './world';

async function done(world: World, token: string, method: 'POST' | 'PUT', path: string, body?: unknown): Promise<void> {
  const reply = await call(world, token, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
}

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
  const failed: string[] = [];
  page.on('requestfailed', (req) => {
    const url = new URL(req.url());
    if (url.origin !== world.origin) return;
    failed.push(`${req.method()} ${url.pathname} ${req.failure()?.errorText ?? ''}`);
  });
  await signIn(page, world, token, person.name);

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
  expect(failed, 'requests to conchd the browser reported as failed').toEqual([]);
});
