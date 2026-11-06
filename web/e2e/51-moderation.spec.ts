import type { Locator, Page } from '@playwright/test';
import { createChannelResponse, listChannelMembersResponseV1, type CreateChannelRequest, type PutChannelMemberRequestV1 } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import type { ErrorV0 } from '../src/schema/error';
import { listChannelBansResponseV1 } from '../src/schema/generated';
import { postMessageResponseV2, type PostMessageRequestV2 } from '../src/schema/message';
import { post } from './api';
import { expect, openChannel, phones, rememberPushToTalk, signIn, test } from './fixtures';
import { call, ok, type World } from './world';
import { dockerAvailable, startVoiceWorld } from './voice-world';

let serial = 0;
const unique = (label: string) => `m1440-${label}-${Date.now().toString(36)}-${String(++serial)}`;
const panel = (page: Page) => page.getByRole('region', { name: 'Members list' });
const personRow = (page: Page, name: string) => panel(page).locator('li').filter({ has: page.locator('.ag-name').getByText(name, { exact: true }) });
const messageRow = (page: Page, id: number) => page.getByRole('log', { name: 'Messages' }).locator(`[data-message-id="${String(id)}"]`).first();
async function choose(row: Locator, name: string, action: string) {
  await row.getByRole('button', { name: `Actions for ${name}` }).click();
  await row.getByRole('menuitem', { name: action, exact: true }).click();
}
async function room(world: World) {
  const request: CreateChannelRequest = { name: unique('room') };
  return (await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request)).channel.name;
}
async function human(world: World, secrets: string[], channel: string, manager = false) {
  const request: CreatePrincipalRequest = { kind: 'human', name: unique(manager ? 'manager' : 'member') };
  const principal = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal;
  const credential: CreateCredentialRequestV1 = { label: principal.name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(principal.id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  const membership: PutChannelMemberRequestV1 = { manages: manager, can_decide: false };
  expect((await call(world, world.tokens.operator, 'PUT', `/v1/channels/${channel}/members/${String(principal.id)}`, membership)).status).toBe(204);
  return { id: principal.id, name: principal.name, token };
}
function trackSockets() {
  const sockets: WebSocket[] = [];
  const Original = window.WebSocket;
  const Wrapped = function (url: string | URL, protocols?: string | string[]) {
    const socket = new Original(url, protocols); sockets.push(socket); return socket;
  } as unknown as typeof WebSocket;
  Wrapped.prototype = Original.prototype;
  window.WebSocket = Wrapped;
  Object.assign(window, { moderationSockets: sockets });
}

test('manager confirms membership removal, ban and own unban; a plain member has no controls', async ({ meet, world, secrets }) => {
  const channel = await room(world);
  const manager = await human(world, secrets, channel, true);
  const member = await human(world, secrets, channel);
  const target = await human(world, secrets, channel);
  const mgr = (await meet()).page;
  const plain = (await meet()).page;
  const targetPage = (await meet()).page;
  await signIn(mgr, world, manager.token, manager.name); await openChannel(mgr, channel);
  await signIn(plain, world, member.token, member.name); await openChannel(plain, channel);
  await signIn(targetPage, world, target.token, target.name); await openChannel(targetPage, channel);
  const beforeRemoval = await post(world, target.token, channel, unique('membership-history'));
  await expect(messageRow(targetPage, beforeRemoval.id)).toContainText(beforeRemoval.body);
  await expect(panel(plain).getByRole('button', { name: /^Actions for / })).toHaveCount(0);
  await expect(panel(plain).getByRole('button', { name: 'Channel bans' })).toHaveCount(0);
  expect((await call(world, member.token, 'DELETE', `/v1/channels/${channel}/voice/participants/${String(target.id)}`)).status).toBe(403);
  const row = personRow(mgr, target.name);
  await choose(row, target.name, 'Remove from channel');
  await expect(row.getByRole('button', { name: 'Cancel' })).toBeFocused();
  await expect(row).toContainText('and its nets? They lose this channel and its history. This does not ban them or revoke their account.');
  await row.getByRole('button', { name: 'Cancel' }).click();
  await expect(row).toBeVisible();
  await choose(row, target.name, 'Remove from channel');
  await row.getByRole('button', { name: 'Yes, remove' }).click();
  await expect(row).toHaveCount(0);
  await expect(targetPage.getByText(`You no longer have access to #${channel}`, { exact: true })).toBeVisible();
  await expect(targetPage.getByText(beforeRemoval.body, { exact: true })).toHaveCount(0);
  expect((await call(world, target.token, 'GET', `/v2/channels/${channel}/messages`)).status).toBe(404);
  // A bodyless operator PUT adds with can_decide true, and a manager may not ban an approver (moderation.md 6.1); re-add as a plain member.
  const plainMembership: PutChannelMemberRequestV1 = { manages: false, can_decide: false };
  expect((await call(world, world.tokens.operator, 'PUT', `/v1/channels/${channel}/members/${String(target.id)}`, plainMembership)).status).toBe(204);
  await expect(row).toBeVisible();
  await targetPage.reload(); await openChannel(targetPage, channel);
  await expect(messageRow(targetPage, beforeRemoval.id)).toContainText(beforeRemoval.body);
  await choose(row, target.name, 'Ban from channel');
  await expect(row.getByRole('group', { name: 'Confirm moderation' })).toContainText('A new account or guest visit is not identified by this ban.');
  await row.getByRole('button', { name: 'Confirm' }).click();
  await expect(row).toHaveCount(0);
  expect((await call(world, world.tokens.operator, 'PUT', `/v1/channels/${channel}/members/${String(target.id)}`)).status).toBe(404);
  await panel(mgr).getByRole('button', { name: 'Channel bans', exact: true }).click();
  const bans = panel(mgr).getByRole('region', { name: 'Channel bans' });
  await bans.getByRole('button', { name: `Unban ${target.name}` }).click();
  await expect(bans).toContainText('Unban does not re-add');
  await bans.getByRole('button', { name: 'Confirm' }).click();
  await expect(bans.getByText('Ban lifted or already absent. Unban does not re-add them.', { exact: true })).toBeVisible();
  expect((await ok(world, manager.token, 'GET', `/v1/channels/${channel}/bans`, listChannelBansResponseV1)).bans).toEqual([]);
  expect((await ok(world, manager.token, 'GET', `/v1/channels/${channel}/members`, listChannelMembersResponseV1)).members.some((m) => m.principal_id === target.id)).toBe(false);
  // Repeats retain the same explicit REST intent and restore no membership.
  expect((await call(world, manager.token, 'DELETE', `/v1/channels/${channel}/bans/${String(target.id)}`)).status).toBe(204);
  expect((await call(world, manager.token, 'DELETE', `/v1/channels/${channel}/members/${String(target.id)}`)).status).toBe(204);
});

test('message removal replaces live copies by id, survives reconnect and never notifies an inaccessible whisper', async ({ meet, world, secrets }) => {
  const channel = await room(world);
  const manager = await human(world, secrets, channel, true);
  const author = await human(world, secrets, channel);
  const outsider = await human(world, secrets, channel);
  const mgr = await meet(); const reader = await meet(phones[0][1]); const other = await meet();
  await reader.page.addInitScript(trackSockets);
  for (const [client, person] of [[mgr, manager], [reader, author], [other, outsider]] as const) {
    await signIn(client.page, world, person.token, person.name); await openChannel(client.page, channel);
  }
  const message = await post(world, author.token, channel, unique('original-secret'));
  const a = messageRow(mgr.page, message.id); const b = messageRow(reader.page, message.id);
  await expect(b).toContainText(message.body);
  await expect(b.getByRole('menuitem', { name: 'Remove message' })).toHaveCount(0);
  await a.getByRole('button', { name: /^More actions/ }).click();
  await a.getByRole('menuitem', { name: 'Remove message', exact: true }).click();
  await expect(a.getByRole('button', { name: 'Cancel' })).toBeFocused();
  await expect(a).toContainText('copies already received cannot be recalled');
  await a.getByRole('button', { name: 'Confirm' }).click();
  for (const page of [mgr.page, reader.page, other.page]) {
    await expect(messageRow(page, message.id)).toContainText('This message was removed by a moderator.');
    await expect(messageRow(page, message.id)).toHaveCount(1);
    await expect(page.getByText(message.body, { exact: true })).toHaveCount(0);
  }
  // Miss an older message's update offline. Reconnect must re-read its safe
  // envelope even though the socket cursor is newer than this message.
  const offline = await post(world, author.token, channel, unique('offline-secret'));
  await expect(messageRow(reader.page, offline.id)).toContainText(offline.body);
  await reader.context.setOffline(true);
  await reader.page.evaluate(() => {
    for (const socket of (window as unknown as { moderationSockets: WebSocket[] }).moderationSockets) socket.close();
  });
  expect((await call(world, manager.token, 'DELETE', `/v2/channels/${channel}/messages/${String(offline.id)}`)).status).toBe(204);
  await reader.context.setOffline(false);
  await expect(messageRow(reader.page, offline.id)).toContainText('This message was removed by a moderator.');
  await reader.page.reload();
  await expect(messageRow(reader.page, offline.id)).toContainText('This message was removed by a moderator.');
  await other.page.getByRole('button', { name: 'Settings' }).click();
  await other.page.getByLabel('Message notifications').selectOption('all');
  await other.page.getByRole('button', { name: 'Close', exact: true }).click();
  const request: PostMessageRequestV2 = { body: unique('whisper-secret'), audience: { kind: 'principals', principal_ids: [manager.id] } };
  const whisper = (await ok(world, author.token, 'POST', `/v2/channels/${channel}/messages`, postMessageResponseV2, request)).message;
  await expect(messageRow(mgr.page, whisper.id)).toContainText(request.body);
  expect((await call(world, manager.token, 'DELETE', `/v2/channels/${channel}/messages/${String(whisper.id)}`)).status).toBe(204);
  await expect(messageRow(mgr.page, whisper.id)).toContainText('This message was removed by a moderator.');
  await expect(messageRow(other.page, whisper.id)).toHaveCount(0);
  await other.page.getByRole('button', { name: /^Notifications(?: \(\d+\))?$/ }).click();
  await expect(other.page.getByRole('list', { name: 'Recent notifications' }).getByRole('listitem')).toHaveCount(0);
  await expect(other.page.getByText(request.body, { exact: true })).toHaveCount(0);
  expect((await call(world, outsider.token, 'DELETE', `/v2/channels/${channel}/messages/${String(message.id)}`)).status).toBe(403);
  expect((await call(world, manager.token, 'DELETE', `/v2/channels/${channel}/messages/${String(message.id)}`)).status).toBe(204);
});

test('LiveKit: a manager confirms a voice-only removal with retry, a member cannot, and the target keeps chat access', async ({ browser, world }, info) => {
  if (!dockerAvailable()) {
    if (process.env['CI'] !== undefined) throw new Error('Moderation voice parity requires Docker and LiveKit in CI');
    info.skip(true, 'Docker is required for the live moderation voice flow'); return;
  }
  const vw = await startVoiceWorld(world);
  const contexts = [];
  try {
    const manager = await vw.addMember(unique('voice-manager'));
    const target = await vw.addMember(unique('voice-target'));
    const member = await vw.addMember(unique('voice-member'));
    const grant: PutChannelMemberRequestV1 = { manages: true };
    expect((await vw.api(vw.operator, 'PUT', `/v1/channels/${vw.channel}/members/${String(manager.id)}`, grant)).status).toBe(204);
    const pages = [];
    for (const person of [manager, target, member]) {
      const context = await browser.newContext(); contexts.push(context);
      await rememberPushToTalk(context);
      const page = await context.newPage(); pages.push(page);
      await signIn(page, vw, person.token, person.name); await openChannel(page, vw.channel);
    }
    const [mgr, targetPage, plain] = pages;
    if (mgr === undefined || targetPage === undefined || plain === undefined) throw new Error('missing moderation pages');
    const voice = (page: Page) => page.getByRole('region', { name: 'Voice' });
    for (const page of [targetPage, plain]) {
      await voice(page).getByRole('button', { name: 'Join voice' }).click();
      await expect(voice(page)).toContainText(`Connected in #${vw.channel}`, { timeout: 30_000 });
    }
    await expect(voice(plain).getByRole('button', { name: /Remove .* from voice/ })).toHaveCount(0);
    expect((await vw.api(member.token, 'DELETE', `/v1/channels/${vw.channel}/voice/participants/${String(target.id)}`)).status).toBe(403);
    const kick = voice(mgr).getByRole('button', { name: `Remove ${target.name} from voice`, exact: true });
    const failure: ErrorV0 = { code: 'voice_unavailable', message: 'voice is temporarily unavailable; retry' };
    await mgr.route(`**/v1/channels/${vw.channel}/voice/participants/${String(target.id)}`, async (route) => {
      expect(route.request().method()).toBe('DELETE');
      await route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify(failure) });
    }, { times: 1 });
    await kick.click();
    await expect(voice(mgr).getByRole('button', { name: 'Cancel' })).toBeFocused();
    await expect(voice(mgr)).toContainText('They can rejoin; chat access is unchanged.');
    await voice(mgr).getByRole('button', { name: 'Confirm' }).click();
    await expect(voice(mgr)).toContainText('Voice removal was incomplete.');
    await expect(voice(targetPage)).toContainText(`Connected in #${vw.channel}`);
    await voice(mgr).getByRole('button', { name: 'Retry' }).click();
    await expect(voice(mgr)).toContainText('Removed or already absent/removed.');
    // The target is out of voice and told so (the presence socket closed 1008 `voice_removed`,
    // issue #1409): no rejoin by itself, never "channel gone", and Join voice is theirs to press.
    await expect(voice(targetPage)).toContainText('You were removed from voice', { timeout: 30_000 });
    await expect(voice(targetPage)).not.toContainText(`Connected in #${vw.channel}`);
    const rejoin = voice(targetPage).getByRole('button', { name: 'Join voice' });
    await expect(rejoin).toBeEnabled();
    await expect(voice(plain)).toContainText(`Connected in #${vw.channel}`);
    // A deliberate rejoin works at once.
    await rejoin.click();
    await expect(voice(targetPage)).toContainText(`Connected in #${vw.channel}`, { timeout: 30_000 });
    await expect(voice(targetPage)).not.toContainText('You were removed from voice');
    // Voice removal leaves chat access unchanged: the target is still a member who can read and post.
    expect((await vw.api(target.token, 'GET', `/v2/channels/${vw.channel}/messages`)).status).toBe(200);
    const stillPosts: PostMessageRequestV2 = { body: unique('after-voice-kick') };
    expect((await vw.api(target.token, 'POST', `/v2/channels/${vw.channel}/messages`, stillPosts)).status).toBe(201);
  } finally {
    for (const context of contexts) await context.close();
    vw.stop();
  }
});
