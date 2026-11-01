import type { Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { postMessageResponseV2 } from '../src/schema/message';
import { listNetsResponseV1 } from '../src/schema/net';
import { history, post, reply } from './api';
import { channelButton, expect, expectFits, openChannel, phoneSizes, phones, signIn, test } from './fixtures';
import { call, ok, principalId } from './world';

let next = 0;
const unique = (label: string) => `r842-${label}-${Date.now().toString(36)}-${String(++next)}`;


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
  const composer = thread.getByRole('textbox', { name: /^Reply to (?!message )/ });
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
    await thread.getByRole('textbox', { name: /^Reply to (?!message )/ }).fill(body);
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

test('a reply from a phone shows its root and the root counts it on the desktop', async ({ meet, world }) => {
  const desktop = await meet({ viewport: { width: 1440, height: 900 } });
  await signIn(desktop.page, world, world.tokens.browser, 'browser-test');
  const phone = await meet(phones[0][1]);
  await signIn(phone.page, world, world.tokens.browser, 'browser-test');
  const rootBody = unique('anyone up for game night');
  const root = await post(world, world.tokens.browser, 'general', rootBody);
  const body = unique('I am in');
  const thread = await openThread(phone.page, root.id);
  await thread.getByRole('textbox', { name: /^Reply to (?!message )/ }).fill(body);
  await thread.getByRole('button', { name: 'Send reply' }).click();
  await expect(thread.getByText(body, { exact: true })).toBeVisible();

  const log = desktop.page.locator('.log');
  await expect(log.locator(`[data-message-id="${String(root.id)}"]`).getByRole('button', { name: '1 reply' })).toBeVisible();
  const row = log.locator('.message', { hasText: body });
  await expect(row.getByText(new RegExp(`^Replying to .+: ${rootBody}$`))).toBeVisible();
});

for (const [size, options] of phoneSizes.filter(([size]) => size === '390×844' || size === '320×640')) {
  test(`the reply form and buttons fit at ${size}`, async ({ meet, world }) => {
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    const root = await post(world, world.tokens.browser, 'general', unique(`fit ${size}`));
    const replyButton = page.getByRole('button', { name: `Reply to message ${String(root.id)}`, exact: true });
    const target = await replyButton.boundingBox();
    expect(target?.width ?? 0).toBeGreaterThanOrEqual(44);
    expect(target?.height ?? 0).toBeGreaterThanOrEqual(44);
    const thread = await openThread(page, root.id);
    const box = await thread.boundingBox();
    for (const control of [thread.getByRole('textbox', { name: /^Reply to (?!message )/ }), thread.getByRole('button', { name: 'Send reply' })]) {
      const rect = await control.boundingBox();
      expect((rect?.x ?? 0) + (rect?.width ?? 0)).toBeLessThanOrEqual((box?.x ?? 0) + (box?.width ?? 0));
    }
  });
}

// A half-typed reply is kept across a reload, by thread root (issue #1080).
const kept = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }],
  ['390×844', phones[0][1]],
] as const;
for (const [size, options] of kept) {
  test(`a half-typed reply comes back after a reload, and is gone once sent at ${size}`, async ({ meet, world }) => {
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    const root = await post(world, world.tokens.browser, 'general', unique(`kept ${size}`));
    const body = unique(`half-typed reply ${size}`);
    let thread = await openThread(page, root.id);
    await thread.getByRole('textbox', { name: /^Reply to (?!message )/ }).fill(body);

    await page.reload();
    await openChannel(page, 'general');
    thread = page.getByRole('region', { name: `Thread around message ${String(root.id)}` });
    await expect(thread.getByRole('textbox', { name: /^Reply to (?!message )/ })).toHaveValue(body);
    await thread.getByRole('button', { name: 'Send reply' }).click();
    await expect(thread.getByText(body, { exact: true })).toBeVisible();

    await page.reload();
    await openChannel(page, 'general');
    await expect(page.getByRole('region', { name: `Thread around message ${String(root.id)}` })).toHaveCount(0);
    thread = await openThread(page, root.id);
    await expect(thread.getByRole('textbox', { name: /^Reply to (?!message )/ })).toHaveValue('');
  });
}

test('a kept reply to a whisper root goes out with the root audience', async ({ meet, world }) => {
  const guest = await principalId(world, world.tokens.guest);
  const restored = await call(world, world.tokens.operator, 'PUT', `/v1/channels/nets-demo/members/${String(guest)}`);
  expect(restored.status).toBe(204);
  const whisperRoot = (await ok(world, world.tokens.browser, 'POST', '/v2/channels/nets-demo/messages', postMessageResponseV2, {
    body: `${unique('kept whisper')} root`, audience: { kind: 'principals', principal_ids: [guest] },
  })).message;
  const body = unique('kept whisper reply');
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await openChannel(page, 'nets-demo');
  let thread = await openThread(page, whisperRoot.id);
  await thread.getByRole('textbox', { name: /^Reply to (?!message )/ }).fill(body);

  await page.reload();
  await openChannel(page, 'nets-demo');
  thread = page.getByRole('region', { name: `Thread around message ${String(whisperRoot.id)}` });
  await expect(thread.getByRole('textbox', { name: /^Reply to (?!message )/ })).toHaveValue(body);
  await expect(thread.getByText(/Replying to → whisper to 1; the audience cannot be changed/)).toBeVisible();
  await thread.getByRole('button', { name: 'Send reply' }).click();
  await expect(thread.getByText(body, { exact: true })).toBeVisible();
  const sent = (await history(world, world.tokens.browser, 'nets-demo')).find((message) => message.body === body);
  expect(sent?.thread_root_id).toBe(whisperRoot.id);
  expect(sent?.audience).toEqual(whisperRoot.audience);
});
