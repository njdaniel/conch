// Issue #1094: the members panel's per-person actions are one ⋯ menu per row,
// not buttons and a checkbox under each name. An operator, on a wide window
// and on a phone, makes someone a manager and removes them through the menu,
// against the real server; the rows stay short, and nothing scrolls sideways.
//
// Each test makes its own channel and person, so no other spec's members
// change.
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { createChannelResponse, listChannelMembersResponseV1, type CreateChannelRequest } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { channelButton, expect, expectFits, showView, signIn, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const panel = (page: Page) => page.getByRole('region', { name: 'Members list' });

function memberRow(page: Page, name: string): Locator {
  return panel(page).locator('li').filter({ has: page.locator('.ag-name').getByText(name, { exact: true }) });
}

async function signInAsOperator(page: Page, appUrl: string, token: string): Promise<void> {
  await page.goto(appUrl);
  await page.getByLabel('Credential').fill(token);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
}

const sizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }, true],
];

for (const [size, options, phone] of sizes) {
  test(`P1094 at ${size} an operator makes someone a manager and removes them through the row's menu`, async ({ meet, secrets, world }) => {
    const suffix = Date.now().toString(36).slice(-5);
    const channel = `p1094-${suffix}${phone ? 'p' : 'w'}`;
    const name = `q${suffix}${phone ? 'p' : 'w'}`;
    const request: CreateChannelRequest = { name: channel };
    await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request);
    const principal: CreatePrincipalRequest = { kind: 'human', name };
    const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
    const credential: CreateCredentialRequestV1 = { label: name };
    const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
    secrets.push(token);
    const added = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`);
    expect(added.status).toBeLessThan(300);

    const { page } = await meet(options);
    await signInAsOperator(page, world.appUrl, world.tokens.operator);
    if (phone) await showView(page, 'Channels');
    await channelButton(page, channel).click();
    if (phone) {
      await showView(page, 'Chat');
      const members = page.getByRole('button', { name: 'Members' });
      if ((await members.getAttribute('aria-expanded')) !== 'true') await members.tap();
    }
    const row = memberRow(page, name);
    await expect(row).toBeVisible();

    // Nothing is inline: no checkbox, no action buttons; one ⋯ per row.
    await expect(panel(page).getByRole('checkbox')).toHaveCount(0);
    await expect(row.getByRole('button', { name: /^(Remove|Make)/ })).toHaveCount(0);
    const more = row.getByRole('button', { name: `Actions for ${name}` });
    await expect(more).toBeVisible();

    // A row is a line of name and state plus a line of badges at most, not a block of controls.
    const box = await row.boundingBox();
    expect(box?.height ?? 0, `the row is ${String(box?.height)} px tall`).toBeLessThan(64);
    await expectFits(page, `${size}: the members panel with a ⋯ per row`);

    const press = async (target: Locator) => (phone ? target.tap() : target.click());

    await press(more);
    await expect(row.getByRole('menuitem')).toHaveText(['Remove from channel', 'Make manager', 'Stop deciding approvals']);
    await expectFits(page, `${size}: a row's menu open`);
    await press(row.getByRole('menuitem', { name: 'Make manager' }));
    await expect(row.getByText('Manager', { exact: true })).toBeVisible();
    await expect(row.getByRole('menu')).toHaveCount(0);

    await press(more);
    await expect(row.getByRole('menuitem', { name: 'Remove as manager' })).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(row.getByRole('menu')).toHaveCount(0);
    await expect(more).toBeFocused();

    await press(more);
    await press(row.getByRole('menuitem', { name: 'Remove from channel' }));
    await press(row.getByRole('button', { name: 'Yes, remove' }));
    await expect(memberRow(page, name)).toHaveCount(0);
    await expect
      .poll(async () => (await ok(world, world.tokens.operator, 'GET', `/v1/channels/${encodeURIComponent(channel)}/members`, listChannelMembersResponseV1)).members.some((m) => m.principal_id === id))
      .toBe(false);
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

const prevented = (page: Page) => page.evaluate(() => (window as unknown as { contextMenusPrevented: boolean[] }).contextMenusPrevented);

// rightClick right-clicks the target and says whether the page claimed the
// event (so the browser's own menu is suppressed).
async function rightClick(page: Page, target: Locator): Promise<boolean> {
  const before = (await prevented(page)).length;
  await target.click({ button: 'right' });
  const seen = await prevented(page);
  expect(seen.length, 'one contextmenu event for one right-click').toBe(before + 1);
  return seen[before] === true;
}

async function newPerson(world: World, secrets: string[], name: string, channel: string): Promise<{ id: number; token: string }> {
  const principal: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  const added = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`);
  expect(added.status).toBeLessThan(300);
  return { id, token };
}

test('P1094 a right-click on a row with actions opens its menu and is claimed; elsewhere, and on a row with no actions, the native menu stays', async ({ meet, secrets, world }) => {
  const suffix = Date.now().toString(36).slice(-5);
  const channel = `p1094-${suffix}-click`;
  const name = `q${suffix}c`;
  const plainName = `q${suffix}m`;
  const request: CreateChannelRequest = { name: channel };
  await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request);
  await newPerson(world, secrets, name, channel);
  const plain = await newPerson(world, secrets, plainName, channel);

  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await signInAsOperator(page, world.appUrl, world.tokens.operator);
  await channelButton(page, channel).click();
  const row = memberRow(page, name);
  await expect(row).toBeVisible();
  await watchContextMenus(page);
  // On a row with actions: claimed, and the menu opens.
  expect(await rightClick(page, row.locator('.ag-name'))).toBe(true);
  await expect(row.getByRole('menu', { name: `Actions for ${name}` })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(row.getByRole('menu')).toHaveCount(0);
  // Outside the member rows: the browser's own menu.
  expect(await rightClick(page, panel(page).getByRole('heading', { name: 'People' }))).toBe(false);
  expect(await rightClick(page, page.getByRole('heading', { name: channel }))).toBe(false);
  await expect(page.getByRole('menu')).toHaveCount(0);

  // A plain member sees rows with no actions at all: the native menu stays.
  const member = (await meet({ viewport: { width: 1440, height: 900 } })).page;
  await signIn(member, world, plain.token, plainName);
  await channelButton(member, channel).click();
  const own = memberRow(member, plainName);
  await expect(own).toBeVisible();
  await expect(panel(member).getByRole('button', { name: /^Actions for / })).toHaveCount(0);
  await watchContextMenus(member);
  expect(await rightClick(member, own.locator('.ag-name'))).toBe(false);
  expect(await rightClick(member, memberRow(member, name).locator('.ag-name'))).toBe(false);
  await expect(member.getByRole('menu')).toHaveCount(0);
});
