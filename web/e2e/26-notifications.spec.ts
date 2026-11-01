import type { Page } from '@playwright/test';
import { post } from './api';
import { conversation, expect, signIn, test } from './fixtures';
import { call, principalId } from './world';

async function instrument(page: Page, permission: NotificationPermission = 'denied'): Promise<void> {
  await page.addInitScript((permission) => {
    const w = window as unknown as { __notes: { title: string; body: string; closed: boolean }[]; Notification: unknown };
    w.__notes = [];
    const Native = window.Notification;
    function Tracked(title: string, options: NotificationOptions) {
      const note = new Native(title, options) as Notification & { closed: boolean };
      note.closed = false;
      w.__notes.push(note);
      const close = note.close.bind(note);
      note.close = () => {
        note.closed = true;
        close();
      };
      return note;
    }
    Tracked.prototype = Native.prototype;
    Tracked.permission = permission;
    Tracked.requestPermission = () => Promise.resolve(permission);
    w.Notification = Tracked;
  }, permission);
}

async function notes(page: Page): Promise<{ title: string; body: string; closed: boolean }[]> {
  return page.evaluate(() => (window as unknown as { __notes: { title: string; body: string; closed: boolean }[] }).__notes);
}

const centerButton = (page: Page) => page.getByRole('button', { name: /^Notifications(?: \(\d+\))?$/ });
const alertItems = (page: Page) => page.getByRole('list', { name: 'Recent notifications' }).getByRole('listitem');

async function policy(page: Page, value: 'off' | 'mentions' | 'all'): Promise<void> {
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByLabel('Message notifications').selectOption(value);
  await page.getByRole('button', { name: 'Close' }).click();
}

async function enableNative(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Enable browser notifications' }).click();
  await page.getByRole('button', { name: 'Close' }).click();
}

async function toggleMute(page: Page, channel: string): Promise<void> {
  await page.getByRole('button', { name: 'Settings' }).click();
  const checkbox = page.getByLabel(`Mute notifications for #${channel}`);
  if (await checkbox.isHidden()) await page.getByText('Channel mutes', { exact: true }).click();
  await checkbox.click();
  await page.getByRole('button', { name: 'Close' }).click();
}

async function openCenter(page: Page): Promise<void> {
  if (await page.getByRole('region', { name: 'Notifications' }).isHidden()) await centerButton(page).click();
}

test('notification policy, hidden-tab native alerts, mute, off and logout cleanup', async ({ meet, world }) => {
  const { page } = await meet();
  await instrument(page, 'granted');
  await signIn(page, world, world.tokens.nick, 'nick-test');
  await policy(page, 'all');
  await enableNative(page);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });

  await post(world, world.tokens.guest, 'nets-demo', 'native one');
  await expect.poll(async () => (await notes(page)).length).toBe(1);
  await openCenter(page);
  await expect(alertItems(page)).toHaveCount(1);
  await toggleMute(page, 'nets-demo');
  await expect(alertItems(page)).toHaveCount(0);
  expect((await notes(page))[0]?.closed).toBe(true);
  await post(world, world.tokens.guest, 'nets-demo', 'muted');
  expect((await notes(page)).length).toBe(1);

  await toggleMute(page, 'nets-demo');
  await post(world, world.tokens.guest, 'nets-demo', 'native two');
  await expect.poll(async () => (await notes(page)).length).toBe(2);
  await policy(page, 'off');
  expect((await notes(page))[1]?.closed).toBe(true);
  await openCenter(page);
  await expect(alertItems(page)).toHaveCount(0);
  await post(world, world.tokens.guest, 'nets-demo', 'off');
  expect((await notes(page)).length).toBe(2);

  await policy(page, 'all');
  await post(world, world.tokens.guest, 'nets-demo', 'before logout');
  await expect.poll(async () => (await notes(page)).length).toBe(3);
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByRole('button', { name: 'Yes, sign out', exact: true }).click();
  await expect(page.getByLabel('Credential')).toBeVisible();
  expect((await notes(page))[2]?.closed).toBe(true);
});

test('mentions-only and denied fallback preserve scoped audiences and revoke alerts', async ({ meet, world }) => {
  const { page } = await meet();
  await instrument(page);
  await signIn(page, world, world.tokens.guest, 'jamie-test');
  await policy(page, 'mentions');

  await post(world, world.tokens.browser, 'nets-demo', 'not a mention');
  await expect(centerButton(page)).toHaveText('Notifications');
  await post(world, world.tokens.browser, 'nets-demo', 'hello @jamie-test');
  await openCenter(page);
  await expect(alertItems(page)).toHaveCount(1);
  expect((await notes(page)).length).toBe(0);

  const nick = await principalId(world, world.tokens.guest);
  const observer = await principalId(world, world.tokens.observer);
  for (const recipient of [nick, observer]) {
    const response = await call(world, world.tokens.browser, 'POST', '/v2/channels/nets-demo/messages', {
      body: recipient === nick ? 'whisper for @jamie-test' : 'whisper for someone else',
      audience: { kind: 'principals', principal_ids: [recipient] },
    });
    expect(response.status).toBe(201);
  }
  await expect(alertItems(page)).toHaveCount(2);
  expect((await notes(page)).length).toBe(0);

  const removed = await call(world, world.tokens.operator, 'DELETE', `/v1/channels/nets-demo/members/${String(nick)}`);
  expect(removed.status).toBe(204);
  await expect(centerButton(page)).toHaveText('Notifications');
  await openCenter(page);
  await expect(alertItems(page)).toHaveCount(0);
});

// Issue #1043: names complete from the channel's members, and a message that
// names the viewer is marked on its row.
test('@mention completes from the channel and the person named sees the mark', async ({ meet, world }) => {
  const sender = await meet();
  await signIn(sender.page, world, world.tokens.browser, 'browser-test');
  await expect(conversation(sender.page).getByRole('heading', { name: 'general' })).toBeVisible();

  const box = sender.page.getByRole('textbox', { name: 'Message #general' });
  const tag = `e1043-${Date.now().toString(36)}`;
  await box.fill(`${tag} hello @jam`);
  const list = sender.page.getByRole('listbox', { name: 'Mention suggestions' });
  await expect(list.getByRole('option')).toHaveText(['@jamie-test']);
  await sender.page.keyboard.press('Enter');
  await expect(box).toHaveValue(`${tag} hello @jamie-test `);
  await expect(list).toBeHidden();
  // Nobody outside the channel is offered.
  await box.fill('@no-such-person-here');
  await expect(list).toBeHidden();
  await box.fill(`${tag} hello @jamie-test `);

  const viewer = await meet();
  await signIn(viewer.page, world, world.tokens.guest, 'jamie-test');
  await expect(conversation(viewer.page).getByRole('heading', { name: 'general' })).toBeVisible();
  await sender.page.getByRole('button', { name: 'Send' }).click();
  const row = conversation(viewer.page).getByRole('listitem').filter({ hasText: tag });
  await expect(row).toHaveCount(1);
  await expect(row).toHaveClass(/mentions-you/);
  await expect(row).toContainText('Mentions you');

  await post(world, world.tokens.browser, 'general', `${tag} no name here`);
  const plain = conversation(viewer.page).getByRole('listitem').filter({ hasText: `${tag} no name here` });
  await expect(plain).toHaveCount(1);
  await expect(plain).not.toHaveClass(/mentions-you/);
});
