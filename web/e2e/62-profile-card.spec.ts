import type { Locator, Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { whoAmIResponseV1 } from '../src/schema/principal';
import { memberOf, post } from './api';
import { launchWorker } from './agents';
import { consoleErrors, credentialField, expect, expectFits, messages, openChannel, signIn, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';
import { watchTraffic } from './watch';

interface Someone {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

interface WatchedPage {
  readonly page: Page;
  readonly errors: string[];
}

const stamp = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
let next = 0;
const unique = (label: string): string => `pc1544-${label}-${stamp}-${String(++next)}`;

async function person(world: World, secrets: string[], name: string): Promise<Someone> {
  const principal: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  return { id, name, token };
}

async function privateChannel(world: World, name: string, members: readonly Someone[]): Promise<void> {
  const made = await call(world, world.tokens.operator, 'POST', '/v1/channels', { name, visibility: 'private' });
  expect(made.status, `making ${name}: ${JSON.stringify(made.body)}`).toBeLessThan(300);
  for (const member of members) await memberOf(world, name, member.id, true);
}

async function ownerIn(page: Page, world: World): Promise<void> {
  await page.goto(world.appUrl);
  await (await credentialField(page)).fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
}

async function openMembers(page: Page): Promise<Locator> {
  const toggle = page.getByRole('button', { name: 'Members' });
  if ((await toggle.getAttribute('aria-expanded')) === 'false') await toggle.click();
  const panel = page.getByRole('region', { name: 'Members list' });
  await expect(panel).toBeVisible();
  return panel;
}

function messageRow(page: Page, id: number): Locator {
  return messages(page).locator(`[data-message-id="${String(id)}"]`);
}

function channelRow(page: Page, name: string): Locator {
  return page.locator('.ag-panel li.member-row').filter({ has: page.locator('.ag-name').getByText(name, { exact: true }) });
}

function agentRow(page: Page, name: string): Locator {
  return page.getByRole('list', { name: 'Agents' }).locator('li.ag-agent').filter({ has: page.locator('.ag-name').getByText(name, { exact: true }) });
}

function expectClean(pages: readonly WatchedPage[]): void {
  for (const { page, errors } of pages) expect(errors, `console errors on ${page.url()}`).toEqual([]);
}

async function expectOnScreen(page: Page, element: Locator, what: string): Promise<void> {
  const box = await element.boundingBox();
  const screen = page.viewportSize();
  if (box === null || screen === null) throw new Error(`${what} has no box on the screen`);
  const where = `${what} is at x ${String(box.x)}, y ${String(box.y)}, ${String(box.width)}×${String(box.height)}`;
  expect(box.x, where).toBeGreaterThanOrEqual(0);
  expect(box.y, where).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width, where).toBeLessThanOrEqual(screen.width);
  expect(box.y + box.height, where).toBeLessThanOrEqual(screen.height);
}

test('1: a member card discloses no unsent Owner mark and opening cards sends no request', async ({ meet, secrets, world }) => {
  const tag = unique('reads');
  const member = await person(world, secrets, `${tag}-member`);
  await memberOf(world, 'general', member.id, true);
  const owner = await ok(world, world.tokens.operator, 'GET', '/v1/whoami', whoAmIResponseV1);
  const ownerMessage = await post(world, world.tokens.operator, 'general', `${tag} from owner`);
  const worker = await launchWorker(world, 'slow.sh', { sleep: 0 });
  const pages: WatchedPage[] = [];
  try {
    const memberContext = await meet();
    pages.push({ page: memberContext.page, errors: consoleErrors(memberContext.page) });
    const traffic = watchTraffic(memberContext.page);
    const requests: string[] = [];
    let recording = false;
    memberContext.page.on('request', (request) => {
      if (recording) requests.push(`${request.method()} ${new URL(request.url()).pathname}`);
    });
    await signIn(memberContext.page, world, member.token, member.name);
    await openChannel(memberContext.page, 'general');
    const ownerRow = messageRow(memberContext.page, ownerMessage.id);
    await expect(ownerRow.locator('.msg-head').getByRole('button', { name: owner.name, exact: true })).toBeVisible();
    const received = await traffic.text();
    expect(received).toContain(owner.name);
    expect(received).not.toMatch(/"owner"\s*:/);

    recording = true;
    await ownerRow.locator('.msg-head').getByRole('button', { name: owner.name, exact: true }).click();
    const ownerCard = memberContext.page.getByRole('dialog', { name: owner.name, exact: true });
    await expect(ownerCard).toBeVisible();
    await expect(ownerCard.locator('.avatar')).toBeVisible();
    await expect(ownerCard.locator('[data-identity="owner"]')).toHaveCount(0);
    await expect(ownerCard.getByText('Owner', { exact: true })).toHaveCount(0);
    expect(requests, 'requests made by opening the owner card').toEqual([]);
    await ownerCard.getByRole('button', { name: 'Close', exact: true }).click();

    await openMembers(memberContext.page);
    const agentName = agentRow(memberContext.page, 'worker-test').getByRole('button', { name: 'worker-test', exact: true });
    await expect(agentName).toBeVisible();
    await agentName.click();
    const agentCard = memberContext.page.getByRole('dialog', { name: 'worker-test', exact: true });
    await expect(agentCard.locator('.profile-card-kind')).toHaveText('Agent');
    await expect(agentCard.locator('[data-identity="agent"]')).toBeVisible();
    expect(requests, 'requests made by opening either card').toEqual([]);
  } finally {
    await worker.stop();
  }
  expectClean(pages);
});

test('2: a members frame adds Manager to an open card without a reload', async ({ meet, secrets, world }) => {
  const tag = unique('live');
  const member = await person(world, secrets, `${tag}-member`);
  const third = await person(world, secrets, `${tag}-third`);
  const channel = `${tag}-room`;
  await privateChannel(world, channel, [member, third]);
  const pages: WatchedPage[] = [];

  const ownerContext = await meet();
  pages.push({ page: ownerContext.page, errors: consoleErrors(ownerContext.page) });
  await ownerIn(ownerContext.page, world);
  await openChannel(ownerContext.page, channel);
  await openMembers(ownerContext.page);

  const thirdContext = await meet();
  pages.push({ page: thirdContext.page, errors: consoleErrors(thirdContext.page) });
  await signIn(thirdContext.page, world, third.token, third.name);
  await openChannel(thirdContext.page, channel);
  await openMembers(thirdContext.page);
  await channelRow(thirdContext.page, member.name).getByRole('button', { name: member.name, exact: true }).click();
  const card = thirdContext.page.getByRole('dialog', { name: member.name, exact: true });
  await expect(card).toBeVisible();
  await expect(card.locator('[data-identity="manager"]')).toHaveCount(0);
  await thirdContext.page.evaluate(() => {
    document.body.dataset['kept'] = 'yes';
  });

  const actions = channelRow(ownerContext.page, member.name).getByRole('button', { name: `Actions for ${member.name}` });
  await actions.click();
  await channelRow(ownerContext.page, member.name).getByRole('menuitem', { name: 'Make manager' }).click();
  await expect(card.locator('[data-identity="manager"]')).toBeVisible();
  expect(await thirdContext.page.evaluate(() => document.body.dataset['kept']), 'the page reloaded').toBe('yes');
  expectClean(pages);
});

test('3: Tab, Enter and Escape open and close a card with focus returned to the name', async ({ meet, secrets, world }) => {
  const tag = unique('keyboard');
  const member = await person(world, secrets, `${tag}-member`);
  await memberOf(world, 'general', member.id, true);
  const said = await post(world, member.token, 'general', `${tag} keyboard`);
  const { page } = await meet();
  const pages = [{ page, errors: consoleErrors(page) }];
  await signIn(page, world, member.token, member.name);
  await openChannel(page, 'general');
  const name = messageRow(page, said.id).locator('.msg-head').getByRole('button', { name: member.name, exact: true });
  await expect(name).toBeVisible();
  for (let stops = 0; stops < 50 && !(await name.evaluate((el) => el === document.activeElement)); stops++) await page.keyboard.press('Tab');
  await expect(name).toBeFocused();
  await page.keyboard.press('Enter');
  const card = page.getByRole('dialog', { name: member.name, exact: true });
  await expect(card).toBeVisible();
  await expect(card.getByRole('button', { name: 'Close', exact: true })).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(card).toHaveCount(0);
  await expect(name).toBeFocused();
  expectClean(pages);
});

test('4: at 390×844 a tap opens a fitting card and a long press still opens the message menu', async ({ meet, secrets, world }) => {
  const tag = unique('phone');
  const member = await person(world, secrets, `${tag}-member`);
  await memberOf(world, 'general', member.id, true);
  const said = await post(world, member.token, 'general', `${tag} touch`);
  const { page } = await meet({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true });
  const pages = [{ page, errors: consoleErrors(page) }];
  await signIn(page, world, member.token, member.name);
  await openChannel(page, 'general');
  const row = messageRow(page, said.id);
  const name = row.locator('.msg-head').getByRole('button', { name: member.name, exact: true });
  await expect(name).toBeVisible();
  await name.tap();
  const card = page.getByRole('dialog', { name: member.name, exact: true });
  await expect(card).toBeVisible();
  await expectOnScreen(page, card, 'the profile card');
  const close = card.getByRole('button', { name: 'Close', exact: true });
  for (const [what, target] of [['name', name], ['Close', close]] as const) {
    const box = await target.boundingBox();
    expect(box?.width ?? 0, `${what} target width`).toBeGreaterThanOrEqual(44);
    expect(box?.height ?? 0, `${what} target height`).toBeGreaterThanOrEqual(44);
  }
  await expectFits(page, '390×844 profile card');
  await close.tap();
  await expect(card).toHaveCount(0);
  expect(await row.getByRole('menu').count()).toBe(0);

  const body = row.locator('.body');
  await body.dispatchEvent('touchstart', { touches: [{ identifier: 0, clientX: 10, clientY: 10 }] });
  await expect(row.getByRole('menu', { name: /^More actions for / })).toBeVisible();
  await body.dispatchEvent('touchend', { touches: [] });
  await expect(card).toHaveCount(0);
  await expectFits(page, '390×844 message menu after the profile card');
  expectClean(pages);
});
