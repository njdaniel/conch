import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { createChannelResponse, listChannelMembersResponseV1, type CreateChannelRequest } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { whoAmIResponseV1 } from '../src/schema/principal';
import { channelButton, expect, showView, signIn, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });
const panel = (page: Page) => page.getByRole('region', { name: 'Members list' });

function memberRow(page: Page, name: string): Locator {
  return panel(page).locator('li').filter({ has: page.locator('.ag-name').getByText(name, { exact: true }) });
}

async function chooseFor(row: Locator, name: string, item: string): Promise<void> {
  await row.getByRole('button', { name: `Actions for ${name}` }).click();
  await row.getByRole('menuitem', { name: item }).click();
  // Removal asks first (issue #1083).
  if (item === 'Remove from channel') await row.getByRole('button', { name: 'Yes, remove' }).click();
}

async function done(world: World, method: 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown): Promise<void> {
  const reply = await call(world, world.tokens.operator, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
}

async function person(world: World, secrets: string[], name: string, memberships: readonly string[]): Promise<{ id: number; name: string; token: string }> {
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  for (const channel of memberships) await done(world, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`);
  return { id, name, token };
}

async function operatorPage(meet: () => Promise<{ page: Page }>, world: World): Promise<Page> {
  const { page } = await meet();
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
  return page;
}

test('C793 an operator delegates a channel; its manager adds, removes and renames while members see only their standing', async ({ meet, secrets, world }) => {
  const suffix = String(Date.now());
  const current = `c793-${suffix}-room`;
  const shared = `c793-${suffix}-shared`;
  const renamed = `c793-${suffix}-renamed`;
  const currentRequest: CreateChannelRequest = { name: current };
  const sharedRequest: CreateChannelRequest = { name: shared };
  await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, currentRequest);
  await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, sharedRequest);
  const manager = await person(world, secrets, `c793-${suffix}-manager`, [current, shared]);
  const target = await person(world, secrets, `c793-${suffix}-target`, [shared]);
  const plain = await person(world, secrets, `c793-${suffix}-member`, [current]);
  const operatorName = (await ok(world, world.tokens.operator, 'GET', '/v1/whoami', whoAmIResponseV1)).name;

  const op = await operatorPage(meet, world);
  await channelButton(op, current).click();
  await chooseFor(memberRow(op, manager.name), manager.name, 'Make manager');
  await expect(memberRow(op, manager.name).getByText('Manager', { exact: true })).toBeVisible();
  await memberRow(op, manager.name).getByRole('button', { name: `Actions for ${manager.name}` }).click();
  await expect(memberRow(op, manager.name).getByRole('menuitem', { name: 'Remove as manager' })).toBeVisible();
  await op.keyboard.press('Escape');

  const mgr = (await meet()).page;
  await signIn(mgr, world, manager.token, manager.name);
  await channelButton(mgr, current).click();
  await expect(nav(mgr).getByText('manages', { exact: true })).toBeVisible();
  await expect(memberRow(mgr, manager.name)).toBeVisible();

  const pickerToggle = panel(mgr).getByRole('button', { name: 'Add from another channel' });
  await pickerToggle.click();
  const picker = panel(mgr).getByRole('form', { name: 'Add person from another channel' });
  await expect(picker.getByRole('option', { name: target.name, exact: true })).toHaveCount(1);
  await expect(picker.getByRole('option', { name: plain.name, exact: true })).toHaveCount(0);
  await pickerToggle.click();

  const byName = panel(mgr).getByRole('form', { name: 'Add person by name' });
  await byName.getByLabel('Exact name').fill(`c793-${suffix}-unknown`);
  await byName.getByRole('button', { name: 'Add by name' }).click();
  await expect(panel(mgr).getByText('No one by that name can be added here.', { exact: true })).toBeVisible();
  await byName.getByLabel('Exact name').fill(target.name);
  await byName.getByRole('button', { name: 'Add by name' }).click();
  await expect(memberRow(mgr, target.name)).toBeVisible();

  await expect(memberRow(mgr, operatorName).getByRole('button', { name: `Actions for ${operatorName}` })).toHaveCount(0);
  await expect(panel(mgr).getByText('Only operators choose who manages a channel.', { exact: true })).toBeVisible();
  await expect(panel(mgr).getByText('Only operators choose who decides approvals.', { exact: true })).toBeVisible();
  await memberRow(mgr, target.name).getByRole('button', { name: `Actions for ${target.name}` }).click();
  await expect(memberRow(mgr, target.name).getByRole('menuitem')).toHaveText(['Remove from channel']);
  await mgr.keyboard.press('Escape');

  const member = (await meet()).page;
  await signIn(member, world, plain.token, plain.name);
  await channelButton(member, current).click();
  await expect(memberRow(member, plain.name)).toBeVisible();
  await expect(panel(member).getByRole('form', { name: 'Add person by name' })).toHaveCount(0);
  await expect(panel(member).getByRole('button', { name: /^Actions for / })).toHaveCount(0);

  await chooseFor(memberRow(mgr, target.name), target.name, 'Remove from channel');
  await expect(memberRow(mgr, target.name)).toHaveCount(0);
  await expect
    .poll(async () => (await ok(world, world.tokens.operator, 'GET', `/v1/channels/${encodeURIComponent(current)}/members`, listChannelMembersResponseV1)).members.some((m) => m.principal_id === target.id))
    .toBe(false);

  await nav(mgr).getByRole('button', { name: `Actions for ${current}` }).click();
  await nav(mgr).getByRole('menuitem', { name: 'Rename channel…' }).click();
  const rename = nav(mgr).getByRole('form', { name: 'Rename channel' });
  await rename.getByLabel('New name').fill(renamed);
  await rename.getByRole('button', { name: 'Rename channel' }).click();
  await expect(nav(mgr).getByRole('form', { name: 'Rename channel' })).toHaveCount(0);
  await expect(channelButton(mgr, renamed)).toHaveAttribute('aria-current', 'true');
  await expect(channelButton(member, renamed)).toHaveAttribute('aria-current', 'true');
});

// Issue #1083 (browser UX audit 4, F6): removing someone from a channel asks
// first, from the row's menu item. Cancel and Escape leave them in; the yes
// removes them, and they lose the channel. On a phone the buttons are at
// least 44 px tall.
const removeSizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }, true],
];

for (const [size, options, phone] of removeSizes) {
  test(`F6 at ${size} removing someone from a channel asks first, and they lose the channel`, async ({ meet, secrets, world }) => {
    const suffix = Date.now().toString(36).slice(-5);
    const channel = `f6-${suffix}${phone ? 'p' : 'w'}`;
    const request: CreateChannelRequest = { name: channel };
    await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request);
    const target = await person(world, secrets, `f6-${suffix}${phone ? 'p' : 'w'}`, [channel]);

    const gone = (await meet()).page;
    await signIn(gone, world, target.token, target.name);
    await channelButton(gone, channel).click();
    await expect(gone.getByRole('heading', { name: channel, exact: true })).toBeVisible();

    const { page } = await meet(options);
    await page.goto(world.appUrl);
    await page.getByLabel('Credential').fill(world.tokens.operator);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await page.getByRole('button', { name: 'Continue anyway' }).click();
    await expect(statusLine(page)).toContainText('Connected');
    if (phone) await showView(page, 'Channels');
    await channelButton(page, channel).click();
    if (phone) {
      await showView(page, 'Chat');
      const members = page.getByRole('button', { name: 'Members' });
      if ((await members.getAttribute('aria-expanded')) !== 'true') await members.tap();
    }
    const press = async (locator: Locator) => (phone ? locator.tap() : locator.click());
    const row = memberRow(page, target.name);
    const more = row.getByRole('button', { name: `Actions for ${target.name}` });
    const still = async () => (await ok(world, world.tokens.operator, 'GET', `/v1/channels/${encodeURIComponent(channel)}/members`, listChannelMembersResponseV1)).members.some((m) => m.principal_id === target.id);

    // The menu item asks; nothing is sent. Focus is on Cancel.
    await press(more);
    await press(row.getByRole('menuitem', { name: 'Remove from channel' }));
    const question = row.getByRole('group', { name: `Remove ${target.name} from #${channel}? They lose this channel and its history.` });
    await expect(question).toBeVisible();
    const cancel = question.getByRole('button', { name: 'Cancel' });
    const yes = question.getByRole('button', { name: 'Yes, remove' });
    await expect(cancel).toBeFocused();
    if (phone) {
      for (const button of [cancel, yes]) expect((await button.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);
    }
    expect(await still()).toBe(true);

    // Escape cancels, and focus goes back to the row's ⋯.
    await page.keyboard.press('Escape');
    await expect(question).toHaveCount(0);
    await expect(more).toBeFocused();
    expect(await still()).toBe(true);

    // Cancel cancels.
    await press(more);
    await press(row.getByRole('menuitem', { name: 'Remove from channel' }));
    await press(cancel);
    await expect(question).toHaveCount(0);
    await expect(more).toBeFocused();
    expect(await still()).toBe(true);
    await expect(row).toBeVisible();

    // Yes removes; the person loses the channel.
    await press(more);
    await press(row.getByRole('menuitem', { name: 'Remove from channel' }));
    await press(yes);
    await expect(row).toHaveCount(0);
    await expect.poll(still).toBe(false);
    await expect(channelButton(gone, channel)).toHaveCount(0, { timeout: 10_000 });
  });
}
