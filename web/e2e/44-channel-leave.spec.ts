// Issue #1095: a member leaves a public channel from its row's ⋯ menu, on a
// wide window and on a phone, against the real server. The row keeps one line
// and the channel is gone from both the page and the member's own channel list.
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { makeInvite } from './api';
import { createChannelResponse, listChannelsResponse, type ChannelVisibility, type CreateChannelRequest } from '../src/schema/channel';
import { channelSettingsResponseV1 } from '../src/schema/generated';
import { channelButton, expect, expectFits, fillAccount, showView, statusLine, test } from './fixtures';
import { call, inviteUrl, ok, type World } from './world';

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });

function channelRow(page: Page, name: string): Locator {
  return nav(page).locator('li').filter({ has: page.getByRole('button', { name, exact: true }) });
}

const sizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }, true],
];

async function makeChannel(world: World, name: string, visibility: ChannelVisibility = 'public'): Promise<void> {
  const request: CreateChannelRequest = { name };
  const made = (await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request)).channel;
  await ok(world, world.tokens.operator, 'PUT', `/v1/channels/${String(made.id)}/settings`, channelSettingsResponseV1, { visibility });
}

for (const [size, options, phone] of sizes) {
  test(`P1095 at ${size} a member leaves a public channel through its menu`, async ({ meet, secrets, world }) => {
    const suffix = Date.now().toString(36).slice(-5);
    const channel = `p1095-${suffix}${phone ? 'p' : 'w'}`;
    const name = `l${suffix}${phone ? 'p' : 'w'}`;
    await makeChannel(world, channel);
    const made = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: [channel], voice: false, may_invite: true });

    const { page } = await meet(options);
    await page.goto(inviteUrl(world, made.code));
    await fillAccount(page, name);
    await expect(statusLine(page)).toContainText(name);
    await expect(statusLine(page)).toContainText('Connected');
    const token = await page.evaluate(() => window.localStorage.getItem('conch.web.token'));
    if (token === null) throw new Error('the member credential was not kept');
    secrets.push(token);

    if (phone) await showView(page, 'Channels');
    const row = channelRow(page, channel);
    await expect(row).toBeVisible();
    const box = await row.boundingBox();
    expect(box?.height ?? 0, `the row is ${String(box?.height)} px tall`).toBeLessThan(64);
    await expectFits(page, `${size}: a member's channel row`);

    const press = async (target: Locator) => (phone ? target.tap() : target.click());
    await press(row.getByRole('button', { name: `Actions for ${channel}` }));
    await expect(row.getByRole('menuitem')).toHaveText(['Invite people', 'Leave channel']);
    await expectFits(page, `${size}: a member's channel menu`);
    const answered = page.waitForResponse((response) => response.request().method() === 'POST' && response.url().endsWith(`/v1/channels/${channel}/leave`));
    await press(row.getByRole('menuitem', { name: 'Leave channel' }));
    const response = await answered;
    expect(response.status()).toBe(204);

    await expect(channelButton(page, channel)).toHaveCount(0);
    const listed = (await ok(world, token, 'GET', '/v1/channels', listChannelsResponse)).channels;
    expect(listed.some((c) => c.name === channel)).toBe(false);
    await expectFits(page, `${size}: after leaving the channel`);
  });
}

test('P1095 at 390×844 a private-channel confirmation and last-member refusal fit', async ({ meet, secrets, world }) => {
  const suffix = Date.now().toString(36).slice(-5);
  const channel = `p1095-${suffix}-private`;
  const name = `l${suffix}p`;
  await makeChannel(world, channel, 'private');
  const made = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: [channel], voice: false, may_invite: true });

  const { page } = await meet({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true });
  await page.goto(inviteUrl(world, made.code));
  await fillAccount(page, name);
  await expect(statusLine(page)).toContainText('Connected');
  const token = await page.evaluate(() => window.localStorage.getItem('conch.web.token'));
  if (token !== null) secrets.push(token);
  // The operator who made the channel leaves it too: this member is now the
  // private channel's last member, whom the server refuses.
  const operatorLeft = await call(world, world.tokens.operator, 'POST', `/v1/channels/${channel}/leave`);
  expect(operatorLeft.status).toBeLessThan(300);
  await showView(page, 'Channels');
  const row = channelRow(page, channel);
  await row.getByRole('button', { name: `Actions for ${channel}` }).tap();
  await row.getByRole('menuitem', { name: 'Leave channel' }).tap();
  const ask = row.getByRole('group', { name: `Leave ${channel}?` });
  await expect(ask).toContainText(`Leave #${channel}? You will not find it again unless someone adds you back.`);
  await expectFits(page, '390×844: the private Leave confirmation');

  const answered = page.waitForResponse((response) => response.request().method() === 'POST' && response.url().endsWith(`/v1/channels/${channel}/leave`));
  await ask.getByRole('button', { name: 'Yes, leave' }).tap();
  expect((await answered).ok()).toBe(false);
  await expect(row.getByRole('status')).toHaveText('You are the last member of this private channel. Add someone else before you leave.');
  await expectFits(page, '390×844: the last-member refusal');
});
