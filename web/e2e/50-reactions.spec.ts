// #766: these checks use actual REST responses and event sockets. No mocked
// reaction state; the shared server is not restarted while other specs use it.
import type { Locator, Page } from '@playwright/test';
import { createChannelResponseV1 } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreatePrincipalRequest } from '../src/schema/credential';
import { postMessageResponseV2 } from '../src/schema/message';
import { readStateV1 } from '../src/schema/readstate';
import { listNetsResponseV1 } from '../src/schema/net';
import { reactionSummaryV1, listReactorsResponseV1, reactionSummariesResponseV1 } from '../src/schema/reaction';
import { post, readState } from './api';
import { expect, expectFits, openChannel, phones, signIn, test } from './fixtures';
import { call, ok, type World } from './world';
let serial = 0;
const unique = (label: string) => `r766-${label}-${Date.now().toString(36)}-${String(++serial)}`;
const row = (page: Page, id: number) => page.getByRole('log', { name: 'Messages' }).locator(`[data-message-id="${String(id)}"]`).first();
const chip = (item: Locator, count: number) => item.getByRole('button', { name: `thumbs up: ${String(count)} reaction${count === 1 ? '' : 's'}`, exact: true });
async function pick(page: Page, item: Locator, touch = false) {
  const add = item.getByRole('button', { name: /^Add reaction to / });
  await expect(add).toBeEnabled();
  if (touch) await add.tap();
  else {
    // Enter the message's F15 stop, then its reaction controls without adding
    // a Tab stop before the existing More actions / Reply controls.
    await item.focus();
    await page.keyboard.press('ArrowRight');
    await expect(add).toBeFocused();
    await page.keyboard.press('Enter');
  }
  const picker = page.getByRole('dialog', { name: 'Emoji', exact: true });
  await expect(picker.getByRole('searchbox')).toBeFocused();
  await picker.getByRole('searchbox').fill('thumbs up');
  if (touch) await picker.getByRole('button', { name: 'thumbs up', exact: true }).tap();
  else await picker.getByRole('searchbox').press('Enter');
  await expect(add).toBeFocused();
}
const path = (channel: string, id: number, emoji?: string) => `/v2/channels/${encodeURIComponent(channel)}/messages/${String(id)}/reactions${emoji === undefined ? '' : `/${encodeURIComponent(emoji)}`}`;

// Save handles before app start so both sockets can be interrupted like a
// network loss, using the same approach as the isolation/restart specs.
function trackSockets() {
  const sockets: WebSocket[] = [];
  const Original = window.WebSocket;
  const Wrapped = function (url: string | URL, protocols?: string | string[]) {
    const socket = new Original(url, protocols); sockets.push(socket); return socket;
  } as unknown as typeof WebSocket;
  Wrapped.prototype = Original.prototype;
  window.WebSocket = Wrapped;
  Object.assign(window, { reactionSockets: sockets });
}

test('two clients converge on add/remove/count/list/reload/reconnect without moving read marks', async ({ meet, world, secrets }) => {
  // Earlier specs may revoke shared human credentials. Own both readers here.
  const author = await human(world, secrets, 'general');
  const reader = await human(world, secrets, 'general');
  const first = await meet({ viewport: { width: 1440, height: 1000 } });
  const second = await meet(phones[0][1]);
  await first.page.addInitScript(trackSockets);
  await signIn(first.page, world, author.token, author.principal.name);
  await signIn(second.page, world, reader.token, reader.principal.name);
  const message = await post(world, author.token, 'general', unique('two-clients'));
  const a = row(first.page, message.id); const b = row(second.page, message.id);
  await pick(first.page, a);
  await expect(chip(a, 1)).toHaveAttribute('aria-pressed', 'true');
  await expect(chip(b, 1)).toHaveAttribute('aria-pressed', 'false');
  await ok(world, author.token, 'PUT', '/v1/channels/general/read-state', readStateV1, { last_read_id: message.id });
  const before = await readState(world, author.token);
  await chip(b, 1).tap();
  await expect(chip(a, 2)).toBeVisible(); await expect(chip(b, 2)).toHaveAttribute('aria-pressed', 'true');
  expect(await readState(world, author.token)).toEqual(before);
  // The active message's reaction order is Add, chip, Who reacted.
  await first.page.keyboard.press('ArrowRight');
  await expect(chip(a, 2)).toBeFocused();
  await first.page.keyboard.press('ArrowRight');
  const who = a.getByRole('button', { name: 'Who reacted with thumbs up' });
  await expect(who).toBeFocused();
  await first.page.keyboard.press('Enter');
  const reactors = first.page.getByRole('dialog', { name: 'Who reacted with thumbs up' });
  await expect(reactors.getByRole('list')).toContainText(author.principal.name);
  await expect(reactors.getByRole('list')).toContainText(reader.principal.name);
  await first.page.keyboard.press('Escape');
  await expect(who).toBeFocused();
  await first.page.keyboard.press('Escape');
  await expect(a).toBeFocused();
  await first.page.reload();
  await expect(chip(a, 2)).toHaveAttribute('aria-pressed', 'true');

  // Drop the first reader, change rows while offline, and require hydration
  // of the missing removal (the server does not replay reaction events).
  await first.context.setOffline(true);
  await first.page.evaluate(() => {
    const sockets = (window as unknown as { reactionSockets: WebSocket[] }).reactionSockets;
    for (const socket of sockets) socket.close();
  });
  await chip(b, 2).tap();
  await expect(chip(b, 1)).toHaveAttribute('aria-pressed', 'false');
  await first.context.setOffline(false);
  await expect(chip(a, 1)).toHaveAttribute('aria-pressed', 'true');
  await a.focus();
  await first.page.keyboard.press('ArrowRight');
  await expect(a.getByRole('button', { name: /^Add reaction to / })).toBeFocused();
  await first.page.keyboard.press('ArrowRight');
  await expect(chip(a, 1)).toBeFocused();
  // Hold a failed removal so focus is checked during the optimistic update
  // as well as after rollback. The server's reaction state stays unchanged.
  let failRemoval!: () => void;
  const removalHeld = new Promise<void>((resolve) => { failRemoval = resolve; });
  await first.page.route(`**${path('general', message.id, '👍')}`, async (route) => {
    expect(route.request().method()).toBe('DELETE');
    await removalHeld;
    await route.abort('failed');
  }, { times: 1 });
  await first.page.keyboard.press('Enter');
  await expect(a.getByRole('button', { name: /thumbs up:/ })).toHaveCount(0);
  const add = a.getByRole('button', { name: /^Add reaction to / });
  await expect(add).toBeFocused();
  failRemoval();
  await expect(a.getByText('Reaction could not be saved. Try again.')).toBeVisible();
  await expect(chip(a, 1)).toBeEnabled();
  await expect(add).toBeFocused();
  await first.page.keyboard.press('ArrowRight');
  await expect(chip(a, 1)).toBeFocused();
  await first.page.keyboard.press('Enter');
  await expect(a.getByRole('button', { name: /thumbs up:/ })).toHaveCount(0);
  await expect(b.getByRole('button', { name: /thumbs up:/ })).toHaveCount(0);
  await expect(add).toBeFocused();
  await first.page.keyboard.press('Escape');
  await expect(a).toBeFocused();
  await second.page.reload();
  await expect(b.getByRole('button', { name: /^Add reaction to / })).toBeEnabled();
  await expect(b.getByRole('button', { name: /thumbs up:/ })).toHaveCount(0);
  await expectFits(second.page, 'reaction controls on a phone');
});

async function human(world: World, secrets: string[], channel: string) {
  const name = unique('reader');
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const principal = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal;
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(principal.id)}/credentials`, createCredentialResponseV1, { label: name });
  secrets.push(token);
  expect((await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(principal.id)}`)).status).toBe(204);
  return { principal, token };
}

test('server-authorized private, net, whisper and disabled guest boundaries', async ({ meet, world, secrets }) => {
  const privateAuthor = await human(world, secrets, 'test-private');
  const privateMessage = await post(world, privateAuthor.token, 'test-private', unique('private'));
  const outsider = await human(world, secrets, 'nets-demo');
  const author = await human(world, secrets, 'nets-demo');
  const recipient = await human(world, secrets, 'nets-demo');
  expect((await call(world, world.tokens.operator, 'PUT', `/v1/channels/nets-demo/nets/blue/members/${String(author.principal.id)}`, { role: 'member' })).status).toBe(204);
  const nets = await ok(world, author.token, 'GET', '/v1/channels/nets-demo/nets', listNetsResponseV1);
  const blue = nets.nets.find((net) => net.name === 'blue');
  if (blue === undefined) throw new Error('blue net missing');
  const net = (await ok(world, author.token, 'POST', '/v2/channels/nets-demo/messages', postMessageResponseV2, { body: unique('net'), audience: { kind: 'net', net_id: blue.id } })).message;
  const whisper = (await ok(world, author.token, 'POST', '/v2/channels/nets-demo/messages', postMessageResponseV2, { body: unique('whisper'), audience: { kind: 'principals', principal_ids: [recipient.principal.id] } })).message;
  for (const message of [net, whisper]) {
    await ok(world, author.token, 'PUT', path('nets-demo', message.id, '👍'), reactionSummaryV1);
    const ghost = 900000000;
    for (const method of ['GET', 'PUT', 'DELETE'] as const) {
      const suffix = method === 'GET' ? undefined : '👍';
      const denied = await call(world, outsider.token, method, path('nets-demo', message.id, suffix));
      expect(denied.status).toBe(404);
      expect(denied.body).toEqual((await call(world, outsider.token, method, path('nets-demo', ghost, suffix))).body);
    }
    const deniedList = await call(world, outsider.token, 'GET', path('nets-demo', message.id, '👍'));
    expect(deniedList.status).toBe(404);
    expect(deniedList.body).toEqual((await call(world, outsider.token, 'GET', path('nets-demo', ghost, '👍'))).body);
    const batch = await ok(world, outsider.token, 'GET', `/v2/channels/nets-demo/reactions?messages=${String(message.id)}`, reactionSummariesResponseV1);
    expect(batch.summaries).toEqual([]);
  }
  for (const suffix of [undefined, '👍']) {
    const denied = await call(world, outsider.token, 'GET', path('test-private', privateMessage.id, suffix));
    expect(denied.status).toBe(404); expect(denied.body).toMatchObject({ code: 'channel_not_found' });
  }
  const { page } = await meet();
  await signIn(page, world, outsider.token, outsider.principal.name);
  await openChannel(page, 'nets-demo');
  await expect(page.getByText(net.body, { exact: true })).toHaveCount(0);
  await expect(page.getByText(whisper.body, { exact: true })).toHaveCount(0);
  await expect(page.getByRole('dialog', { name: /Who reacted/ })).toHaveCount(0);
  // #1005 disables admission of temporary guests in this checkout. Exercise
  // the actual door rather than pretending an ordinary fixture member is a guest.
  const guest = await call(world, world.tokens.operator, 'POST', '/v1/join/guest', {});
  expect(guest.status).toBe(403); expect(guest.body).toMatchObject({ code: 'guests_disabled' });
});

test('reactor pages are bounded, moderator removals require confirmation, and revocation clears controls', async ({ meet, world, secrets }) => {
  const name = unique('pages');
  const made = await ok(world, world.tokens.operator, 'POST', '/v1/channels', createChannelResponseV1, { name, visibility: 'private' });
  const channel = made.channel.name;
  const reader = await human(world, secrets, channel);
  const message = await post(world, reader.token, channel, unique('pagination'));
  // Two rows suffice to exercise a real cursor with a one-row page, without
  // creating fifty identities or exhausting the reaction write bucket.
  const other = await human(world, secrets, channel);
  for (const token of [reader.token, other.token]) await ok(world, token, 'PUT', path(channel, message.id, '👍'), reactionSummaryV1);
  const first = await ok(world, reader.token, 'GET', `${path(channel, message.id, '👍')}?limit=1`, listReactorsResponseV1);
  expect(first.reactors).toHaveLength(1); expect(first.next_after).toBeGreaterThan(0);
  const second = await ok(world, reader.token, 'GET', `${path(channel, message.id, '👍')}?limit=1&after=${String(first.next_after ?? 0)}`, listReactorsResponseV1);
  expect(second.reactors).toHaveLength(1); expect(second.reactors[0]?.principal_id).not.toBe(first.reactors[0]?.principal_id);
  // Give the reader the manager class, then exercise the browser's confirmation.
  expect((await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(reader.principal.id)}`, { manages: true })).status).toBe(204);
  const { page } = await meet(); await signIn(page, world, reader.token, reader.principal.name); await openChannel(page, channel);
  const item = row(page, message.id);
  await item.getByRole('button', { name: 'Who reacted with thumbs up' }).click();
  await page.getByRole('button', { name: 'Remove this reaction', exact: true }).first().click();
  await expect(page.getByText(/the person may react again/)).toBeVisible();
  await page.getByRole('button', { name: 'Confirm removal' }).click();
  await expect(chip(item, 1)).toBeVisible();
  expect((await call(world, world.tokens.operator, 'DELETE', `/v1/channels/${encodeURIComponent(channel)}/members/${String(reader.principal.id)}`)).status).toBe(204);
  await expect(page.getByRole('dialog', { name: 'Who reacted with thumbs up' })).toHaveCount(0);
  await expect(item.getByRole('button', { name: /^Add reaction to / })).toHaveCount(0);
});
