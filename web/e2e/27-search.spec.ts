import { listNetsResponseV1 } from '../src/schema/net';
import { post } from './api';
import { channelButton, expect, signIn, test } from './fixtures';
import { call, ok, principalId } from './world';

const unique = (label: string) => `s841-${label}-${Date.now().toString(36)}`;

test('search paginates ordered scoped results, opens context, and loses access on revocation', async ({ meet, world }) => {
  const guest = await principalId(world, world.tokens.guest);
  const membership = await call(world, world.tokens.operator, 'PUT', `/v1/channels/nets-demo/members/${String(guest)}`);
  expect(membership.status).toBe(204);
  const netMembership = await call(world, world.tokens.operator, 'PUT', `/v1/channels/nets-demo/nets/blue/members/${String(guest)}`, { role: 'member' });
  expect(netMembership.status).toBe(204);
  const { page } = await meet();
  await signIn(page, world, world.tokens.guest, 'jamie-test');
  await channelButton(page, 'nets-demo').click();
  const tag = unique('scoped');
  const first = await post(world, world.tokens.browser, 'nets-demo', `${tag} normal 1`);
  for (let id = 2; id <= 101; id++) await post(world, world.tokens.browser, 'nets-demo', `${tag} normal ${String(id)}`);

  const nets = await ok(world, world.tokens.browser, 'GET', '/v1/channels/nets-demo/nets', listNetsResponseV1);
  const blue = nets.nets.find((net) => net.name === 'blue');
  if (blue === undefined) throw new Error('blue net missing');
  const observer = await principalId(world, world.tokens.observer);
  for (const audience of [{ kind: 'net', net_id: blue.id }, { kind: 'principals', principal_ids: [guest] }]) {
    const response = await call(world, world.tokens.browser, 'POST', '/v2/channels/nets-demo/messages', { body: `${tag} visible scoped`, audience });
    expect(response.status).toBe(201);
  }
  const hidden = await call(world, world.tokens.browser, 'POST', '/v2/channels/nets-demo/messages', {
    body: `${tag} hidden whisper`,
    audience: { kind: 'principals', principal_ids: [observer] },
  });
  expect(hidden.status).toBe(201);

  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await page.getByRole('searchbox', { name: 'Messages in #nets-demo' }).fill(tag);
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  const results = page.getByRole('list', { name: 'Search results' });
  await expect(results.getByRole('listitem')).toHaveCount(100);
  await expect(results.getByRole('listitem').first()).toContainText(`${tag} visible scoped`);
  await expect(results).not.toContainText(`${tag} hidden whisper`);
  await page.getByRole('button', { name: 'Load older results' }).click();
  await expect(results.getByRole('listitem')).toHaveCount(103);
  await expect(results.getByRole('listitem').last()).toContainText(`${tag} normal 1`);

  await results.locator(`[data-message-id="${String(first.id)}"]`).getByRole('button', { name: /^Open context for .+'s message, \d\d:\d\d$/ }).click();
  const context = page.getByRole('list', { name: 'Message context' });
  await expect(context.locator(`[data-message-id="${String(first.id)}"]`)).toContainText(`${tag} normal 1`);
  await expect(context.locator(`[data-message-id="${String(first.id + 1)}"]`)).toContainText(`${tag} normal 2`);
  await page.getByRole('button', { name: 'Back to results' }).click();
  await expect(results.getByRole('listitem')).toHaveCount(103);
  await expect(page.getByRole('searchbox')).toHaveValue(tag);

  const removed = await call(world, world.tokens.operator, 'DELETE', `/v1/channels/nets-demo/members/${String(guest)}`);
  expect(removed.status).toBe(204);
  await expect(page.getByRole('region', { name: 'Search #nets-demo' })).toHaveCount(0);
  await expect(channelButton(page, 'nets-demo')).toHaveCount(0);
  const restored = await call(world, world.tokens.operator, 'PUT', `/v1/channels/nets-demo/members/${String(guest)}`);
  expect(restored.status).toBe(204);
  const restoredNet = await call(world, world.tokens.operator, 'PUT', `/v1/channels/nets-demo/nets/blue/members/${String(guest)}`, { role: 'member' });
  expect(restoredNet.status).toBe(204);
});
