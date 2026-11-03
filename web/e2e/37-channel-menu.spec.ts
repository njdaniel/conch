// Issue #1135: a channel row's actions are one ⋯ menu, as a member's are
// (#1094): Invite people and Rename channel…, not a form under the list. A
// manager renames the channel they manage through the menu; an inviter opens
// Invite people and finds the panel with that channel ticked. Both on a wide
// window and on a phone, against the real server; the row stays one line.
//
// Each test makes its own channels and people, so no other spec's channels
// change name.
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { makeInvite } from './api';
import { createChannelResponse, listChannelsResponse, type CreateChannelRequest } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { channelButton, expect, expectFits, fillAccount, showView, signIn, statusLine, test } from './fixtures';
import { call, inviteUrl, ok, type World } from './world';

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });

function channelRow(page: Page, name: string): Locator {
  return nav(page).locator('li').filter({ has: page.getByRole('button', { name, exact: true }) });
}

const sizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }, true],
];

async function makeChannel(world: World, name: string): Promise<void> {
  const request: CreateChannelRequest = { name };
  await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request);
}

for (const [size, options, phone] of sizes) {
  test(`P1135 at ${size} a manager renames their channel through its menu, and Escape returns focus to the ⋯`, async ({ meet, secrets, world }) => {
    const suffix = Date.now().toString(36).slice(-5);
    const channel = `p1135-${suffix}${phone ? 'p' : 'w'}`;
    const renamed = `${channel}-r`;
    const name = `m${suffix}${phone ? 'p' : 'w'}`;
    await makeChannel(world, channel);
    const principal: CreatePrincipalRequest = { kind: 'human', name };
    const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
    const credential: CreateCredentialRequestV1 = { label: name };
    const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
    secrets.push(token);
    const added = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`, { manages: true });
    expect(added.status).toBeLessThan(300);

    const { page } = await meet(options);
    await signIn(page, world, token, name);
    if (phone) await showView(page, 'Channels');
    const row = channelRow(page, channel);
    await expect(row).toBeVisible();

    // Nothing sits under the list: no rename button, no form.
    await expect(nav(page).getByRole('button', { name: /^Rename/ })).toHaveCount(0);
    await expect(page.getByRole('form', { name: 'Rename channel' })).toHaveCount(0);
    const more = row.getByRole('button', { name: `Actions for ${channel}` });
    await expect(more).toBeVisible();
    const box = await row.boundingBox();
    expect(box?.height ?? 0, `the row is ${String(box?.height)} px tall`).toBeLessThan(64);
    await expectFits(page, `${size}: a channel row with a ⋯`);

    const press = async (target: Locator) => (phone ? target.tap() : target.click());

    // The keyboard: Enter on the ⋯ opens the menu with the first item
    // focused; Escape closes it and puts focus back.
    await more.focus();
    await page.keyboard.press('Enter');
    await expect(row.getByRole('menuitem')).toHaveText(['Rename channel…']);
    await expect(row.getByRole('menuitem', { name: 'Rename channel…' })).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(row.getByRole('menu')).toHaveCount(0);
    await expect(more).toBeFocused();

    // The form opens under the row, in place; Escape closes it, focus on the ⋯.
    await press(more);
    await expectFits(page, `${size}: the channel menu open`);
    await press(row.getByRole('menuitem', { name: 'Rename channel…' }));
    const form = row.getByRole('form', { name: 'Rename channel' });
    await expect(form.getByLabel('New name')).toHaveValue(channel);
    await expect(form.getByLabel('New name')).toBeFocused();
    await expectFits(page, `${size}: the rename form in its row`);
    await page.keyboard.press('Escape');
    await expect(form).toHaveCount(0);
    await expect(more).toBeFocused();

    // Rename it.
    await press(more);
    await press(row.getByRole('menuitem', { name: 'Rename channel…' }));
    await form.getByLabel('New name').fill(renamed);
    await press(form.getByRole('button', { name: 'Rename channel' }));
    await expect(channelButton(page, renamed)).toBeVisible();
    await expect(channelButton(page, channel)).toHaveCount(0);
    await expect(form).toHaveCount(0);
    await expect(channelRow(page, renamed).getByRole('button', { name: `Actions for ${renamed}` })).toBeFocused();
    const list = (await ok(world, world.tokens.operator, 'GET', '/v1/channels', listChannelsResponse)).channels;
    expect(list.some((c) => c.name === renamed)).toBe(true);
    expect(list.some((c) => c.name === channel)).toBe(false);
    await expectFits(page, `${size}: after the rename`);
  });

  test(`P1135 at ${size} an inviter opens Invite people from a channel's menu with that channel ticked`, async ({ meet, secrets, world }) => {
    const suffix = Date.now().toString(36).slice(-5);
    const first = `p1135-${suffix}${phone ? 'p' : 'w'}-a`;
    const second = `p1135-${suffix}${phone ? 'p' : 'w'}-b`;
    const name = `i${suffix}${phone ? 'p' : 'w'}`;
    await makeChannel(world, first);
    await makeChannel(world, second);
    const made = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: [first, second], voice: false, may_invite: true });

    const { page } = await meet(options);
    await page.goto(inviteUrl(world, made.code));
    await fillAccount(page, name);
    await expect(statusLine(page)).toContainText(name);
    await expect(statusLine(page)).toContainText('Connected');
    const kept = await page.evaluate(() => window.localStorage.getItem('conch.web.token'));
    if (kept !== null) secrets.push(kept);

    if (phone) await showView(page, 'Channels');
    const row = channelRow(page, second);
    const more = row.getByRole('button', { name: `Actions for ${second}` });
    await expect(more).toBeVisible();
    const box = await row.boundingBox();
    expect(box?.height ?? 0, `the row is ${String(box?.height)} px tall`).toBeLessThan(64);

    const press = async (target: Locator) => (phone ? target.tap() : target.click());
    await press(more);
    // A plain member who may invite: Invite people alone, no rename.
    await expect(row.getByRole('menuitem')).toHaveText(['Invite people']);
    await press(row.getByRole('menuitem', { name: 'Invite people' }));

    const panel = page.getByRole('region', { name: 'Invites' });
    await expect(panel.getByRole('checkbox', { name: `#${second}` })).toBeChecked();
    await expect(panel.getByRole('checkbox', { name: `#${first}` })).not.toBeChecked();
    await expect(panel.getByRole('button', { name: 'Make invite' })).toBeEnabled();
    await expectFits(page, `${size}: the invite panel opened from a channel`);
  });
}

// watchContextMenus records, for each contextmenu event on the page, whether
// the page called preventDefault on it by the time it reached the document.
async function watchContextMenus(page: Page): Promise<void> {
  await page.evaluate(() => {
    const seen: boolean[] = [];
    Object.assign(window, { contextMenusPrevented: seen });
    document.addEventListener('contextmenu', (event) => {
      seen.push(event.defaultPrevented);
    });
  });
}

test('P1135 a right-click on a channel row with actions opens its menu and is claimed; on the heading, the native menu stays', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
  await watchContextMenus(page);
  const row = channelRow(page, 'general');
  const prevented = () => page.evaluate(() => (window as unknown as { contextMenusPrevented: boolean[] }).contextMenusPrevented);
  await row.click({ button: 'right' });
  expect(await prevented()).toEqual([true]);
  await expect(row.getByRole('menu', { name: 'Actions for general' })).toBeVisible();
  await page.keyboard.press('Escape');
  await nav(page).getByRole('heading', { name: 'Channels' }).click({ button: 'right' });
  expect(await prevented()).toEqual([true, false]);
});
