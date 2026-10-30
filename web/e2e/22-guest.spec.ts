// G6 (issue #713, ADR-009), turned into the refusal checks of issue #1005:
// guests are removed for now. The data support stays, but no route makes a
// guest and no page offers one. Against the real server: the bare address,
// opened to accounts through the API as `conch admin join set` would, offers
// Create an account and no guest entry; an invite page offers an account
// form and no choice of a guest visit; and every route that would have made a
// guest, or a guest-admitting invite or setting, is refused with the server's
// own answer. The specs that made guests through those routes (join, post,
// refresh, End visit, Create an account, a lost conversion answer) cannot run
// any more, so they are deleted: a live visit's End visit and conversion are
// covered by the unit tests (src/guest.test.tsx) and by the server's own
// tests, which can still seed a visit. The operator's Settings having no
// guest switch is checked in 23-operator-auth.spec.ts. Runs last, and puts
// the joining settings back when it is done.
import { expect, test } from './fixtures';
import { joinSettingsResponseV1, type PutJoinSettingsRequestV1 } from '../src/schema/guest';
import { createInviteResponseV1, type CreateInviteRequestV1 } from '../src/schema/invite';
import { call, inviteUrl, loadWorld, ok, type World } from './world';

async function setJoining(world: World, request: PutJoinSettingsRequestV1): Promise<void> {
  await ok(world, world.tokens.operator, 'PUT', '/v1/join/settings', joinSettingsResponseV1, request);
}

test.beforeAll(async () => {
  await setJoining(loadWorld(), { open_joining: 'account', open_channels: ['general'] });
});

test.afterAll(async () => {
  await setJoining(loadWorld(), { open_joining: 'off', open_channels: [] });
});

test('G6 the bare address offers an account and no guest entry', async ({ meet, world }) => {
  const { page } = await meet();
  await page.goto(world.appUrl);
  const door = page.getByRole('region', { name: 'Join' });
  await expect(door.getByRole('button')).toHaveText(['Create an account']);
  await expect(page.getByRole('button', { name: 'Join as guest' })).toHaveCount(0);
  await door.getByRole('button', { name: 'Create an account' }).click();
  await expect(page.getByLabel('Name to show (optional)')).toHaveCount(0);
  const reply = await call(world, world.tokens.operator, 'GET', '/v1/join');
  expect(reply.body).toMatchObject({ join: { guest: false, account: true } });
});

test('G6 an invite offers an account form and no choice of a guest visit', async ({ meet, world }) => {
  const request: CreateInviteRequestV1 = { kind: 'open', channels: ['general'], voice: false };
  const made = await ok(world, world.tokens.operator, 'POST', '/v1/invites', createInviteResponseV1, request);
  const { page } = await meet();
  await page.goto(inviteUrl(world, made.code));
  await expect(page.getByLabel('Choose a password')).toBeVisible();
  await expect(page.getByRole('group', { name: 'How to join' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Join as guest' })).toHaveCount(0);
  await expect(page.getByLabel('Name to show (optional)')).toHaveCount(0);
});

test('G6 every route that would make a guest, or admit one, is refused', async ({ world }) => {
  const operator = world.tokens.operator;
  // The address and invite routes that made a guest.
  const joined = await call(world, operator, 'POST', '/v1/join/guest', {});
  expect(joined.status).toBe(403);
  expect(joined.body).toMatchObject({ code: 'guests_disabled' });
  const redeemed = await call(world, operator, 'POST', '/v1/invites/redeem-guest', { code: 'conch_inv_unused' });
  expect(redeemed.status).toBe(403);
  expect(redeemed.body).toMatchObject({ code: 'guests_disabled' });
  // An invite that admits a guest cannot be made.
  for (const joinAs of ['guest', 'both'] as const) {
    const made = await call(world, operator, 'POST', '/v1/invites', { kind: 'open', channels: ['general'], voice: false, join_as: joinAs });
    expect(made.status, `join_as ${joinAs}`).toBe(409);
    expect(made.body).toMatchObject({ code: 'join_policy_conflict' });
  }
  // Nor can the settings turn guests on.
  for (const request of [{ guests_enabled: true }, { open_joining: 'guest' }, { open_joining: 'both' }]) {
    const put = await call(world, operator, 'PUT', '/v1/join/settings', request);
    expect(put.status, JSON.stringify(request)).toBe(409);
    expect(put.body).toMatchObject({ code: 'join_policy_conflict' });
  }
  const settings = await call(world, operator, 'GET', '/v1/join/settings');
  expect(settings.body).toMatchObject({ settings: { guests_enabled: false, open_joining: 'account' } });
});
