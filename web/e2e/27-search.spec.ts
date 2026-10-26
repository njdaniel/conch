import { listNetsResponseV1 } from '../src/schema/net';
import { joinSettingsResponseV1, type PutJoinSettingsRequestV1 } from '../src/schema/guest';
import { post } from './api';
import { channelButton, expect, signIn, statusLine, test } from './fixtures';
import { call, ok, principalId, type World } from './world';

const unique = (label: string) => `s841-${label}-${Date.now().toString(36)}`;

async function joining(world: World, request: PutJoinSettingsRequestV1) {
  await ok(world, world.tokens.operator, 'PUT', '/v1/join/settings', joinSettingsResponseV1, request);
}

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

  await page.getByRole('button', { name: `Open context for message ${String(first.id)}`, exact: true }).click();
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

test('guest search starts at the visit watermark and excludes earlier history', async ({ meet, secrets, world }) => {
  await joining(world, { guests_enabled: true, open_joining: 'both', open_channels: ['general'], max_guests_per_address: 50 });
  try {
    const tag = unique('guest');
    await post(world, world.tokens.browser, 'general', `${tag} before visit`);
    const { page } = await meet();
    await page.goto(world.appUrl);
    await page.getByRole('button', { name: 'Join as guest', exact: true }).click();
    await page.getByLabel('Name to show (optional)').fill('Search guest');
    await page.getByRole('button', { name: 'Join as guest', exact: true }).click();
    await expect(page.getByRole('region', { name: 'Guest visit' })).toContainText('Temporary guest');
    await expect(statusLine(page)).toContainText('Connected');
    const token = await page.evaluate(() => window.sessionStorage.getItem('conch.web.token'));
    if (token !== null) secrets.push(token);
    await post(world, token ?? '', 'general', `${tag} during visit`);

    await page.getByRole('button', { name: 'Search', exact: true }).click();
    await page.getByRole('searchbox', { name: 'Messages in #general' }).fill(tag);
    await page.getByRole('button', { name: 'Search', exact: true }).click();
    const results = page.getByRole('list', { name: 'Search results' });
    await expect(results.getByRole('listitem')).toHaveCount(1);
    await expect(results).toContainText(`${tag} during visit`);
    await expect(results).not.toContainText(`${tag} before visit`);
  } finally {
    await joining(world, { open_joining: 'off', guests_enabled: false, open_channels: [] });
  }
});
