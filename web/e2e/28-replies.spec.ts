import type { Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { joinSettingsResponseV1, type PutJoinSettingsRequestV1 } from '../src/schema/guest';
import { postMessageResponseV2 } from '../src/schema/message';
import { listNetsResponseV1 } from '../src/schema/net';
import { history, post, reply } from './api';
import { channelButton, expect, expectFits, phones, signIn, statusLine, test } from './fixtures';
import { call, ok, principalId, type World } from './world';

let next = 0;
const unique = (label: string) => `r842-${label}-${Date.now().toString(36)}-${String(++next)}`;

async function joining(world: World, request: PutJoinSettingsRequestV1) {
  await ok(world, world.tokens.operator, 'PUT', '/v1/join/settings', joinSettingsResponseV1, request);
}


async function openThread(page: Page, root: number) {
  await page.getByRole('button', { name: `Reply to message ${String(root)}`, exact: true }).click();
  return page.getByRole('region', { name: `Thread around message ${String(root)}` });
}

test('desktop reply and history use the same one-level thread', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const root = await post(world, world.tokens.browser, 'general', unique('root'));
  const body = unique('desktop reply');
  let thread = await openThread(page, root.id);
  const composer = thread.getByRole('textbox', { name: `Reply to message #${String(root.id)}` });
  await composer.fill(body);
  await composer.press('Enter');
  await expect(thread.getByText(body, { exact: true })).toBeVisible();
  await expect(thread.getByText('Replying to #general; the audience cannot be changed.')).toBeVisible();
  await expect(thread.getByRole('button', { name: `Reply to message ${String(root.id + 1)}` })).toHaveCount(0);

  await page.reload();
  thread = await openThread(page, root.id);
  await expect(thread.getByText(body, { exact: true })).toBeVisible();
  // After a reload the thread holds the reply once. A server restart is not
  // done here: the shared server is restarted only by 09-restart.spec.ts, and
  // a reply surviving a reconnect and refetch once is a unit test
  // (replies.test.tsx).
  await expect(thread.getByText(body, { exact: true })).toHaveCount(1);
});

for (const [size, options] of phones) {
  test(`reply and root view work by touch at ${size}`, async ({ meet, world }) => {
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    const root = await post(world, world.tokens.browser, 'general', unique(`root ${size}`));
    const thread = await openThread(page, root.id);
    const body = unique(`touch ${size}`);
    await thread.getByRole('textbox', { name: `Reply to message #${String(root.id)}` }).fill(body);
    await thread.getByRole('button', { name: 'Send reply' }).click();
    await expect(thread.getByText(body, { exact: true })).toBeVisible();
    await expectFits(page, `reply ${size}`);
    await thread.getByRole('button', { name: 'Close thread' }).click();
    await expect(page.getByRole('button', { name: `Reply to message ${String(root.id)}`, exact: true })).toBeFocused();
  });
}

test('net and whisper replies keep their authorized audience', async ({ meet, secrets, world }) => {
  const guest = await principalId(world, world.tokens.guest);
  for (const path of [`/v1/channels/nets-demo/members/${String(guest)}`, `/v1/channels/nets-demo/nets/blue/members/${String(guest)}`]) {
    const restored = await call(world, world.tokens.operator, 'PUT', path, path.includes('/nets/') ? { role: 'member' } : undefined);
    expect(restored.status).toBe(204);
  }
  const tag = unique('scoped');
  const nets = await ok(world, world.tokens.browser, 'GET', '/v1/channels/nets-demo/nets', listNetsResponseV1);
  const blue = nets.nets.find((net) => net.name === 'blue');
  if (blue === undefined) throw new Error('blue net missing');
  const netRoot = (await ok(world, world.tokens.browser, 'POST', '/v2/channels/nets-demo/messages', postMessageResponseV2, {
    body: `${tag} net root`, audience: { kind: 'net', net_id: blue.id },
  })).message;
  const netReply = await reply(world, world.tokens.browser, 'nets-demo', netRoot.id, `${tag} net reply`);
  const whisperRoot = (await ok(world, world.tokens.browser, 'POST', '/v2/channels/nets-demo/messages', postMessageResponseV2, {
    body: `${tag} whisper root`, audience: { kind: 'principals', principal_ids: [guest] },
  })).message;
  const whisperReply = await reply(world, world.tokens.browser, 'nets-demo', whisperRoot.id, `${tag} whisper reply`);

  const { page } = await meet();
  await signIn(page, world, world.tokens.guest, 'jamie-test');
  await channelButton(page, 'nets-demo').click();
  const netThread = await openThread(page, netRoot.id);
  await expect(netThread.getByText(netReply.body, { exact: true })).toBeVisible();
  await expect(netThread.getByText('Replying to → net blue; the audience cannot be changed.')).toBeVisible();
  await netThread.getByRole('button', { name: 'Close thread' }).click();
  const whisperThread = await openThread(page, whisperRoot.id);
  await expect(whisperThread.getByText(whisperReply.body, { exact: true })).toBeVisible();
  await expect(whisperThread.getByText(/Replying to → whisper to 1; the audience cannot be changed/)).toBeVisible();

  // An outsider of a person of its own, a member of the channel but on no
  // net and in no whisper: the fixture people's credentials may have been
  // revoked by the specs before this one.
  const op = world.tokens.operator;
  const name = unique('outsider').toLowerCase();
  const person: CreatePrincipalRequest = { kind: 'human', name };
  const outsiderId = (await ok(world, op, 'POST', '/v0/principals', createPrincipalResponse, person)).principal.id;
  const label: CreateCredentialRequestV1 = { label: name };
  const { token: outsiderToken } = await ok(world, op, 'POST', `/v1/principals/${String(outsiderId)}/credentials`, createCredentialResponseV1, label);
  secrets.push(outsiderToken);
  const joined = await call(world, op, 'PUT', `/v1/channels/nets-demo/members/${String(outsiderId)}`);
  expect(joined.status).toBe(204);
  const outsider = await history(world, outsiderToken, 'nets-demo');
  expect(outsider.some((message) => [netRoot.id, netReply.id, whisperRoot.id, whisperReply.id].includes(message.id))).toBe(false);
});

test('a reply cannot move an old root across a guest watermark', async ({ meet, secrets, world }) => {
  await joining(world, { guests_enabled: true, open_joining: 'both', open_channels: ['general'], max_guests_per_address: 50 });
  try {
    const tag = unique('guest');
    const before = await post(world, world.tokens.browser, 'general', `${tag} before root`);
    const beforeReply = await reply(world, world.tokens.browser, 'general', before.id, `${tag} before reply`);
    const { page } = await meet();
    await page.goto(world.appUrl);
    await page.getByRole('button', { name: 'Join as guest', exact: true }).click();
    await page.getByLabel('Name to show (optional)').fill('Reply guest');
    await page.getByRole('button', { name: 'Join as guest', exact: true }).click();
    await expect(page.getByRole('region', { name: 'Guest visit' })).toContainText('Temporary guest');
    await expect(statusLine(page)).toContainText('Connected');
    const token = await page.evaluate(() => window.sessionStorage.getItem('conch.web.token'));
    if (token !== null) secrets.push(token);
    expect((await history(world, token ?? '', 'general')).some((message) => [before.id, beforeReply.id].includes(message.id))).toBe(false);
    await expect(page.getByText(before.body, { exact: true })).toHaveCount(0);
    await expect(page.getByText(beforeReply.body, { exact: true })).toHaveCount(0);

    const during = await post(world, token ?? '', 'general', `${tag} during root`);
    const duringReply = await reply(world, world.tokens.browser, 'general', during.id, `${tag} during reply`);
    const thread = await openThread(page, during.id);
    await expect(thread.getByText(duringReply.body, { exact: true })).toBeVisible();
    await page.reload();
    const restored = await openThread(page, during.id);
    await expect(restored.getByText(duringReply.body, { exact: true })).toBeVisible();
  } finally {
    await joining(world, { open_joining: 'off', guests_enabled: false, open_channels: [] });
  }
});
