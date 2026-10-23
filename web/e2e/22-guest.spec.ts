// G6 (issue #713, ADR-009): temporary guests and accounts in the browser,
// against the real server. The operator opens the bare address to guests and
// accounts, through the API as `conch admin join set` would; a visitor joins
// as a guest, posts, refreshes and ends the visit, and a new visit is someone
// new; a guest cancels, fails and then creates an account, and signs in again
// from a fresh browser; a conversion whose answer is lost is recovered by
// password sign-in. Runs last, so the guests it adds to #general touch no
// other spec, and puts the joining settings back when it is done.
import type { Page } from '@playwright/test';
import { history } from './api';
import { accountPassword, expect, fillAccount, messages, say, signInWithPassword, statusLine, test } from './fixtures';
import { joinSettingsResponseV1, type PutJoinSettingsRequestV1 } from '../src/schema/guest';
import { createInviteResponseV1, type CreateInviteRequestV1 } from '../src/schema/invite';
import { whoAmIResponseV1 } from '../src/schema/principal';
import { call, inviteUrl, loadWorld, ok, type World } from './world';

const unique = (label: string) => `g6-${label}-${String(Date.now())}`;

async function setJoining(world: World, request: PutJoinSettingsRequestV1): Promise<void> {
  await ok(world, world.tokens.operator, 'PUT', '/v1/join/settings', joinSettingsResponseV1, request);
}

test.beforeAll(async () => {
  await setJoining(loadWorld(), { guests_enabled: true, open_joining: 'both', open_channels: ['general'], max_guests_per_address: 50 });
});

test.afterAll(async () => {
  await setJoining(loadWorld(), { open_joining: 'off', guests_enabled: false, open_channels: [] });
});

// countMicrophone has the page count every request for a microphone, before
// any of its own scripts run.
async function countMicrophone(page: Page): Promise<() => Promise<number>> {
  await page.addInitScript(() => {
    const w = window as unknown as { conchMicAsks: number };
    w.conchMicAsks = 0;
    const devices = navigator.mediaDevices as MediaDevices | undefined;
    if (devices === undefined) return;
    const real = devices.getUserMedia.bind(devices);
    devices.getUserMedia = (constraints?: MediaStreamConstraints) => {
      w.conchMicAsks++;
      return real(constraints);
    };
  });
  return () => page.evaluate(() => (window as unknown as { conchMicAsks: number }).conchMicAsks);
}

// joinAsGuest starts a visit from the page in front of it and waits for the
// lobby. The visit's credential is handed to the end-of-test scan.
async function joinAsGuest(page: Page, label: string, secrets: string[]): Promise<string> {
  await page.getByRole('button', { name: 'Join as guest', exact: true }).click();
  await page.getByLabel('Name to show (optional)').fill(label);
  await page.getByRole('button', { name: 'Join as guest', exact: true }).click();
  await expect(page.getByRole('region', { name: 'Guest visit' })).toContainText('Temporary guest');
  await expect(statusLine(page)).toContainText('Connected');
  const token = await page.evaluate(() => window.sessionStorage.getItem('conch.web.token'));
  if (token === null) throw new Error('the visit kept no credential in the tab');
  secrets.push(token);
  return token;
}

const lasting = (page: Page) => page.evaluate(() => window.localStorage.getItem('conch.web.token'));
const tabToken = (page: Page) => page.evaluate(() => window.sessionStorage.getItem('conch.web.token'));

test('G6 a guest joins from the bare address, posts, survives a refresh, ends the visit, and a new visit is someone new', async ({ meet, secrets, world }) => {
  secrets.push(accountPassword);
  const { page } = await meet();
  const micAsks = await countMicrophone(page);
  await page.goto(world.appUrl);
  const door = page.getByRole('region', { name: 'Join' });
  await expect(door.getByRole('button')).toHaveText(['Join as guest', 'Create an account']);

  const token = await joinAsGuest(page, 'Pat', secrets);
  // Tab-scoped: this tab's sessionStorage, never the origin's localStorage.
  expect(await lasting(page)).toBeNull();
  const who = await ok(world, token, 'GET', '/v1/whoami', whoAmIResponseV1);
  expect(who.identity_class).toBe('guest');
  const tag = who.guest?.tag ?? '';
  expect(tag).toHaveLength(4);
  const shown = `Guest: Pat · ${tag}`;
  const panel = page.getByRole('region', { name: 'Guest visit' });
  await expect(panel).toContainText(shown);
  await expect(panel).toContainText('End visit signs you out permanently. Your messages remain. Create an account to keep access.');
  await expect(panel).toContainText('Closing the tab does not end it.');
  await expect(statusLine(page)).toContainText(shown);

  // A post is shown under the guest's attribution, here and to others.
  const body = unique('hello');
  await say(page, 'general', body);
  await expect(messages(page).getByRole('listitem').filter({ hasText: body })).toContainText(shown);
  const posted = (await history(world, world.tokens.browser, 'general')).find((m) => m.body === body);
  expect(posted?.guest_author).toMatchObject({ handle: who.name, label: 'Pat', tag });

  // A refresh keeps the live visit: the same credential, the same guest.
  await page.reload();
  await expect(page.getByRole('region', { name: 'Guest visit' })).toContainText(shown);
  expect(await tabToken(page)).toBe(token);

  // End visit asks first, and only the server's word signs the guest out.
  await page.getByRole('button', { name: 'End visit', exact: true }).click();
  await page.getByRole('group', { name: /^End your visit\?/ }).getByRole('button', { name: 'Cancel' }).click();
  expect((await call(world, token, 'GET', '/v1/whoami')).status).toBe(200);
  await page.getByRole('button', { name: 'End visit', exact: true }).click();
  await page.getByRole('button', { name: 'Yes, end visit' }).click();
  await expect(page.getByRole('alert').first()).toHaveText(/Your guest visit has ended\. Your messages stay in the conversation\./);
  expect(await tabToken(page)).toBeNull();
  expect((await call(world, token, 'GET', '/v1/whoami')).status).toBe(401);

  // The messages remain, under the attribution they were posted with.
  const after = (await history(world, world.tokens.browser, 'general')).find((m) => m.body === body);
  expect(after?.guest_author).toMatchObject({ handle: who.name, label: 'Pat', tag });

  // A new visit, in the same browser with the same label, is someone new.
  const again = await joinAsGuest(page, 'Pat', secrets);
  const next = await ok(world, again, 'GET', '/v1/whoami', whoAmIResponseV1);
  expect(next.id).not.toBe(who.id);
  expect(next.name).not.toBe(who.name);
  // Joining chat never asked for a microphone.
  expect(await micAsks()).toBe(0);
  await page.getByRole('button', { name: 'End visit', exact: true }).click();
  await page.getByRole('button', { name: 'Yes, end visit' }).click();
  await expect(page.getByRole('alert').first()).toHaveText(/Your guest visit has ended/);
});

test('G6 a guest cancels, fails, then creates an account, and signs in again from a fresh browser', async ({ meet, secrets, world }) => {
  secrets.push(accountPassword);
  const { page } = await meet();
  await page.goto(world.appUrl);
  const guestToken = await joinAsGuest(page, 'Sam', secrets);
  const guest = await ok(world, guestToken, 'GET', '/v1/whoami', whoAmIResponseV1);
  const body = unique('before');
  await say(page, 'general', body);
  const panel = page.getByRole('region', { name: 'Guest visit' });

  // Cancel changes nothing.
  await panel.getByRole('button', { name: 'Create an account' }).click();
  await panel.getByLabel('Your name').fill(unique('cancelled'));
  await panel.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(panel.getByRole('button', { name: 'Create an account' })).toBeVisible();
  expect((await call(world, guestToken, 'GET', '/v1/whoami')).status).toBe(200);

  // A taken name is refused, and the guest is still the guest.
  await panel.getByRole('button', { name: 'Create an account' }).click();
  await fillAccount(page, 'browser-test', accountPassword, 'Create my account');
  await expect(panel.getByRole('alert')).toHaveText('That name is taken. Choose another.');
  await expect(panel).toContainText('Temporary guest');
  expect(await tabToken(page)).toBe(guestToken);
  expect(((await call(world, guestToken, 'GET', '/v1/whoami')).body as { identity_class?: string }).identity_class).toBe('guest');

  // A free name and a good password: the same principal, now a member.
  const name = unique('member');
  await fillAccount(page, name, accountPassword, 'Create my account');
  await expect(statusLine(page)).toContainText(name);
  await expect(statusLine(page)).toContainText('Connected');
  await expect(page.getByRole('region', { name: 'Guest visit' })).toHaveCount(0);
  expect(await tabToken(page)).toBeNull();
  const memberToken = await lasting(page);
  if (memberToken === null) throw new Error('the member credential was not kept to last');
  secrets.push(memberToken);
  const member = await ok(world, memberToken, 'GET', '/v1/whoami', whoAmIResponseV1);
  expect(member).toMatchObject({ id: guest.id, name, identity_class: 'member', auth: { password_set: true } });
  expect((await call(world, guestToken, 'GET', '/v1/whoami')).status).toBe(401);
  // The post made as a guest still reads as the guest's.
  await expect(messages(page).getByRole('listitem').filter({ hasText: body })).toContainText(`Guest: Sam · ${guest.guest?.tag ?? ''}`);

  // A fresh browser: the member signs in with the name and the password.
  const elsewhere = await meet();
  await elsewhere.page.goto(world.appUrl);
  await signInWithPassword(elsewhere.page, name);
  await expect(statusLine(elsewhere.page)).toContainText(name);
  await expect(statusLine(elsewhere.page)).toContainText('Connected');
  const second = await lasting(elsewhere.page);
  if (second === null) throw new Error('the sign-in kept no credential');
  secrets.push(second);
  expect((await ok(world, second, 'GET', '/v1/whoami', whoAmIResponseV1)).id).toBe(guest.id);

  // A wrong password is the one answer, and keeps nothing.
  const third = await meet();
  await third.page.goto(world.appUrl);
  await signInWithPassword(third.page, name, `${accountPassword} but wrong`);
  await expect(third.page.getByRole('alert').first()).toHaveText('The name or password is not right.');
  expect(await lasting(third.page)).toBeNull();
});

test('G6 a lost answer to Create an account is recovered by signing in with the new name', async ({ meet, secrets, world }) => {
  secrets.push(accountPassword);
  const { page } = await meet();
  await page.goto(world.appUrl);
  const guestToken = await joinAsGuest(page, 'Lee', secrets);
  const guest = await ok(world, guestToken, 'GET', '/v1/whoami', whoAmIResponseV1);
  // The server converts the guest, and the answer never reaches the page.
  await page.route('**/v1/guest/convert', async (route) => {
    await route.fetch();
    await route.abort('connectionreset');
  });
  const name = unique('lost');
  await page.getByRole('region', { name: 'Guest visit' }).getByRole('button', { name: 'Create an account' }).click();
  await fillAccount(page, name, accountPassword, 'Create my account');
  await expect(page.getByRole('alert').first()).toHaveText(/The server did not confirm your new account, and your guest visit has ended/);
  expect(await tabToken(page)).toBeNull();
  // Never the old guest credential: it no longer works.
  expect((await call(world, guestToken, 'GET', '/v1/whoami')).status).toBe(401);
  await signInWithPassword(page, name);
  await expect(statusLine(page)).toContainText(name);
  const memberToken = await lasting(page);
  if (memberToken === null) throw new Error('the sign-in kept no credential');
  secrets.push(memberToken);
  expect((await ok(world, memberToken, 'GET', '/v1/whoami', whoAmIResponseV1)).id).toBe(guest.id);
});

test('G6 an invite that admits both offers both, and its guest visit is kept for the tab alone', async ({ meet, secrets, world }) => {
  const request: CreateInviteRequestV1 = { kind: 'open', channels: ['general'], voice: false, join_as: 'both' };
  const made = await ok(world, world.tokens.operator, 'POST', '/v1/invites', createInviteResponseV1, request);
  const { page } = await meet();
  await page.goto(inviteUrl(world, made.code));
  const choice = page.getByRole('group', { name: 'How to join' });
  await expect(choice.getByRole('button')).toHaveText(['Create an account', 'Join as guest']);
  await choice.getByRole('button', { name: 'Join as guest' }).click();
  await page.getByLabel('Name to show (optional)').fill('Kim');
  await page.getByRole('button', { name: 'Join as guest', exact: true }).click();
  await expect(page.getByRole('region', { name: 'Guest visit' })).toContainText('Guest: Kim ·');
  const token = await tabToken(page);
  if (token === null) throw new Error('the visit kept no credential in the tab');
  secrets.push(token);
  expect(await lasting(page)).toBeNull();
  expect((await ok(world, token, 'GET', '/v1/whoami', whoAmIResponseV1)).guest?.origin).toBe('invite');
  // Another tab of the same browser has no visit: it is this tab's.
  const other = await page.context().newPage();
  await other.goto(world.appUrl);
  await expect(other.getByRole('region', { name: 'Join' })).toBeVisible();
  await expect(other.getByRole('region', { name: 'Guest visit' })).toHaveCount(0);
  await other.close();
  await page.getByRole('button', { name: 'End visit', exact: true }).click();
  await page.getByRole('button', { name: 'Yes, end visit' }).click();
  await expect(page.getByRole('alert').first()).toHaveText(/Your guest visit has ended/);
});
