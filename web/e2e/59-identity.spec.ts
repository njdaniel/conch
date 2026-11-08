import type { Locator, Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { permissionsResponseV1, type PutPermissionsRequestV1 } from '../src/schema/permission';
import { whoAmIResponseV1 } from '../src/schema/principal';
import { history, memberOf, post } from './api';
import { launchWorker } from './agents';
import { consoleErrors, credentialField, expect, expectFits, messages, openChannel, signIn, statusLine, test } from './fixtures';
import { call, ok, principalId, type World } from './world';
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
const unique = (label: string): string => `id1499-${label}-${stamp}-${String(++next)}`;
const mark = (row: Locator, key: string): Locator => row.locator(`[data-identity="${key}"]`);

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

function communityRow(page: Page, name: string): Locator {
  return page.locator('li.community-member').filter({ has: page.locator('.community-name').getByText(name, { exact: true }) });
}

async function chooseFor(row: Locator, name: string, item: string): Promise<void> {
  await row.getByRole('button', { name: `Actions for ${name}` }).click();
  await row.getByRole('menuitem', { name: item }).click();
}

async function agentMessage(world: World, body: string): Promise<number> {
  const worker = await principalId(world, world.tokens.worker);
  const asked = await post(world, world.tokens.nick, 'general', body);
  let answer = 0;
  await expect.poll(async () => {
    answer = (await history(world, world.tokens.nick, 'general')).find((message) => message.author_id === worker && message.id > asked.id)?.id ?? 0;
    return answer;
  }, { timeout: 30_000 }).toBeGreaterThan(0);
  return answer;
}

async function memberCell(world: World, granted: boolean): Promise<void> {
  const current = await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
  const request: PutPermissionsRequestV1 = { revision: current.revision, changes: [{ permission: 'see_member_directory', role: 'member', granted }] };
  const put = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', request);
  expect(put.status, `the member see_member_directory cell to ${String(granted)}: ${JSON.stringify(put.body)}`).toBeLessThan(300);
}

async function expectOnScreen(page: Page, element: Locator, what: string): Promise<void> {
  await element.scrollIntoViewIfNeeded();
  const box = await element.boundingBox();
  const screen = page.viewportSize();
  if (box === null || screen === null) throw new Error(`${what} has no box on the screen`);
  const where = `${what} is at x ${String(box.x)}, y ${String(box.y)}, ${String(box.width)}×${String(box.height)}`;
  expect(box.x, where).toBeGreaterThanOrEqual(0);
  expect(box.y, where).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width, where).toBeLessThanOrEqual(screen.width);
  expect(box.y + box.height, where).toBeLessThanOrEqual(screen.height);
}

// Scenario 5 applies to every context each scenario opens, not to an empty page
// of its own. The meet fixture separately checks every page for credentials.
function expectClean(pages: readonly WatchedPage[]): void {
  for (const { page, errors } of pages) expect(errors, `console errors on ${page.url()}`).toEqual([]);
}

test('1: a plain member sees Agent but not Owner; the owner sees Owner in the community list', async ({ meet, secrets, world }) => {
  const tag = unique('marks');
  const member = await person(world, secrets, `${tag}-member`);
  await memberOf(world, 'general', member.id, true);
  const owner = await ok(world, world.tokens.operator, 'GET', '/v1/whoami', whoAmIResponseV1);
  const ownerMessage = await post(world, world.tokens.operator, 'general', `${tag} from owner`);
  const worker = await launchWorker(world, 'slow.sh', { sleep: 0 });
  const pages: WatchedPage[] = [];
  try {
    const agentMessageId = await agentMessage(world, `${tag} from agent`);

    const memberContext = await meet();
    pages.push({ page: memberContext.page, errors: consoleErrors(memberContext.page) });
    // Watched from before sign-in: the owner's directory row arrives here, and
    // the member must never be told which row is the owner.
    const traffic = watchTraffic(memberContext.page);
    await signIn(memberContext.page, world, member.token, member.name);
    await openChannel(memberContext.page, 'general');
    await openMembers(memberContext.page);
    await expect(mark(messageRow(memberContext.page, agentMessageId), 'agent')).toBeVisible();
    await expect(mark(channelRow(memberContext.page, 'worker-test'), 'agent')).toBeVisible();
    await expect(mark(agentRow(memberContext.page, 'worker-test'), 'agent')).toBeVisible();
    await expect(agentRow(memberContext.page, 'worker-test').locator('.avatar')).toBeVisible();
    await expect(mark(communityRow(memberContext.page, 'worker-test'), 'agent')).toBeVisible();
    for (const row of [messageRow(memberContext.page, ownerMessage.id), channelRow(memberContext.page, owner.name), communityRow(memberContext.page, owner.name)]) {
      await expect(mark(row, 'owner')).toHaveCount(0);
    }
    const received = await traffic.text();
    expect(received, 'the owner\'s directory row reached the member').toContain(owner.name);
    expect(received).not.toMatch(/"owner"\s*:/);

    const ownerContext = await meet();
    pages.push({ page: ownerContext.page, errors: consoleErrors(ownerContext.page) });
    await ownerIn(ownerContext.page, world);
    await openChannel(ownerContext.page, 'general');
    await openMembers(ownerContext.page);
    await expect(mark(communityRow(ownerContext.page, owner.name), 'owner')).toBeVisible();
  } finally {
    await worker.stop();
  }
  expectClean(pages);
});

test('2: Make manager and Remove as manager update author and channel rows without a reload', async ({ meet, secrets, world }) => {
  const tag = unique('live');
  const member = await person(world, secrets, `${tag}-member`);
  const third = await person(world, secrets, `${tag}-third`);
  const channel = `${tag}-room`;
  await privateChannel(world, channel, [member, third]);
  const said = await post(world, member.token, channel, `${tag} member author`);
  const pages: WatchedPage[] = [];

  const ownerContext = await meet();
  pages.push({ page: ownerContext.page, errors: consoleErrors(ownerContext.page) });
  await ownerIn(ownerContext.page, world);
  await openChannel(ownerContext.page, channel);
  await openMembers(ownerContext.page);

  const memberContext = await meet();
  pages.push({ page: memberContext.page, errors: consoleErrors(memberContext.page) });
  await signIn(memberContext.page, world, member.token, member.name);
  await openChannel(memberContext.page, channel);
  await openMembers(memberContext.page);

  const thirdContext = await meet();
  pages.push({ page: thirdContext.page, errors: consoleErrors(thirdContext.page) });
  await signIn(thirdContext.page, world, third.token, third.name);
  await openChannel(thirdContext.page, channel);
  await openMembers(thirdContext.page);
  for (const page of [memberContext.page, thirdContext.page]) {
    await page.evaluate(() => {
      document.body.dataset['kept'] = 'yes';
    });
  }

  await chooseFor(channelRow(ownerContext.page, member.name), member.name, 'Make manager');
  for (const page of [memberContext.page, thirdContext.page]) {
    await expect(mark(messageRow(page, said.id), 'manager')).toBeVisible();
    await expect(mark(channelRow(page, member.name), 'manager')).toBeVisible();
    expect(await page.evaluate(() => document.body.dataset['kept']), 'the page reloaded').toBe('yes');
  }

  await chooseFor(channelRow(ownerContext.page, member.name), member.name, 'Remove as manager');
  for (const page of [memberContext.page, thirdContext.page]) {
    await expect(mark(messageRow(page, said.id), 'manager')).toHaveCount(0);
    await expect(mark(channelRow(page, member.name), 'manager')).toHaveCount(0);
    expect(await page.evaluate(() => document.body.dataset['kept']), 'the page reloaded').toBe('yes');
  }
  expectClean(pages);
});

test('3: hidden and out-of-channel identities have no row or mark, and no row of theirs reaches the member', async ({ meet, secrets, world }) => {
  const tag = unique('scope');
  const member = await person(world, secrets, `${tag}-member`);
  const hidden = await person(world, secrets, `${tag}-hidden`);
  const visible = `${tag}-visible`;
  const elsewhere = `${tag}-elsewhere`;
  await privateChannel(world, visible, [member]);
  await privateChannel(world, elsewhere, [hidden]);
  const hid = await call(world, world.tokens.operator, 'POST', `/v1/principals/${String(hidden.id)}/hide`);
  expect(hid.status, `hiding ${hidden.name}: ${JSON.stringify(hid.body)}`).toBeLessThan(300);
  const pages: WatchedPage[] = [];
  try {
    await memberCell(world, false);
    const { page } = await meet();
    pages.push({ page, errors: consoleErrors(page) });
    const traffic = watchTraffic(page);
    await signIn(page, world, member.token, member.name);
    await openChannel(page, visible);
    const panel = await openMembers(page);
    await expect(panel.getByRole('list', { name: 'People' })).toBeVisible();
    for (const name of [hidden.name, 'worker-test']) {
      await expect(panel.getByText(name, { exact: true })).toHaveCount(0);
      await expect(page.locator('body')).not.toContainText(name);
    }
    const received = await traffic.text();
    expect(received.length).toBeGreaterThan(0);
    expect(received).not.toContain(hidden.name);
  } finally {
    await memberCell(world, true);
  }
  expectClean(pages);
});

test('4: at 390×844 marks keep the message menu on screen and author text keeps TUI order', async ({ meet, secrets, world }) => {
  const tag = unique('phone');
  const member = await person(world, secrets, `${tag}-member-with-a-long-name`);
  await memberOf(world, 'general', member.id, true);
  const managed = await call(world, world.tokens.operator, 'PUT', `/v1/channels/general/members/${String(member.id)}`, { manages: true });
  expect(managed.status, `making ${member.name} a manager: ${JSON.stringify(managed.body)}`).toBeLessThan(300);
  const own = await post(world, member.token, 'general', `${tag} manager author`);
  const worker = await launchWorker(world, 'slow.sh', { sleep: 0 });
  const pages: WatchedPage[] = [];
  try {
    const agentMessageId = await agentMessage(world, `${tag} agent author`);
    const { page } = await meet({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true });
    pages.push({ page, errors: consoleErrors(page) });
    await signIn(page, world, member.token, member.name);
    await openChannel(page, 'general');

    const agent = messageRow(page, agentMessageId);
    await expect(mark(agent, 'agent')).toBeVisible();
    const text = (await agent.locator('.msg-head').textContent())?.replace(/\s+/g, ' ').trim();
    expect(text).toMatch(/^\d{2}:\d{2} worker-test \[agent\]:$/);
    const ownRow = messageRow(page, own.id);
    await expect(mark(ownRow, 'manager')).toBeVisible();

    const more = ownRow.getByRole('button', { name: /^More actions for / });
    await expect(more).toBeVisible();
    await expectOnScreen(page, more, 'the message ⋯ menu');
    await more.tap();
    await expect(page.getByRole('menu', { name: /^More actions for / })).toBeVisible();
    await expectFits(page, '390×844: identity marks and message actions');
  } finally {
    await worker.stop();
  }
  expectClean(pages);
});

async function agentPerson(world: World, secrets: string[], name: string): Promise<Someone> {
  const principal: CreatePrincipalRequest = { kind: 'agent', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  return { id, name, token };
}

test('6: promotion and demotion move Operator, and disabling an agent drops its row, with no reload', async ({ meet, secrets, world }) => {
  const tag = unique('roles');
  const member = await person(world, secrets, `${tag}-member`);
  const bot = await agentPerson(world, secrets, `${tag}-bot`);
  const channel = `${tag}-room`;
  await privateChannel(world, channel, [member, bot]);
  const said = await post(world, member.token, channel, `${tag} member author`);
  const pages: WatchedPage[] = [];
  const role = async (to: 'operator' | 'member') => {
    const done = await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(member.id)}/role`, { role: to });
    expect(done.status, `making ${member.name} ${to}: ${JSON.stringify(done.body)}`).toBeLessThan(300);
  };
  try {
    const { page } = await meet();
    pages.push({ page, errors: consoleErrors(page) });
    // The owner: a channel's Operator marks are told only to a viewer who
    // manages the channel; everyone else learns them from the directory.
    await ownerIn(page, world);
    await openChannel(page, channel);
    await openMembers(page);
    await expect(mark(agentRow(page, bot.name), 'agent')).toBeVisible();
    await expect(mark(channelRow(page, member.name), 'operator')).toHaveCount(0);
    await page.evaluate(() => {
      document.body.dataset['kept'] = 'yes';
    });

    await role('operator');
    await expect(mark(channelRow(page, member.name), 'operator')).toBeVisible();
    await expect(mark(messageRow(page, said.id), 'operator')).toBeVisible();
    await expect(mark(communityRow(page, member.name), 'operator')).toBeVisible();
    expect(await page.evaluate(() => document.body.dataset['kept']), 'the page reloaded').toBe('yes');

    await role('member');
    await expect(mark(channelRow(page, member.name), 'operator')).toHaveCount(0);
    await expect(mark(messageRow(page, said.id), 'operator')).toHaveCount(0);
    await expect(mark(communityRow(page, member.name), 'operator')).toHaveCount(0);

    const hid = await call(world, world.tokens.operator, 'POST', `/v1/principals/${String(bot.id)}/hide`);
    expect(hid.status, `hiding ${bot.name}: ${JSON.stringify(hid.body)}`).toBeLessThan(300);
    await expect(agentRow(page, bot.name)).toHaveCount(0);
    await expect(channelRow(page, bot.name)).toHaveCount(0);
    await expect(communityRow(page, bot.name)).toHaveCount(0);
    await expect(page.getByRole('region', { name: 'Members list' }).getByText(bot.name)).toHaveCount(0);
    expect(await page.evaluate(() => document.body.dataset['kept']), 'the page reloaded').toBe('yes');
  } finally {
    await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(member.id)}/role`, { role: 'member' });
    await call(world, world.tokens.operator, 'POST', `/v1/principals/${String(bot.id)}/hide`);
  }
  // The owner's page reads the manifest of each agent in the list; once the agent
  // is hidden that read is a 404 the browser logs. It is the page asking about
  // a principal it was just told is gone, not an identity fault, and it is the
  // only console error allowed here.
  for (const { errors } of pages) {
    for (let i = errors.length - 1; i >= 0; i--) if (errors[i]?.includes(`/v1/principals/${String(bot.id)}/manifest`) === true) errors.splice(i, 1);
  }
  expectClean(pages);
});

test('7: at 360 px wide marked member lists keep every row control in its row and the page from scrolling sideways', async ({ meet, secrets, world }) => {
  const tag = unique('narrow');
  const member = await person(world, secrets, `${tag}-a-member-with-a-very-long-name`);
  await memberOf(world, 'general', member.id, true);
  const promoted = await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(member.id)}/role`, { role: 'operator' });
  expect(promoted.status, `making ${member.name} an operator: ${JSON.stringify(promoted.body)}`).toBeLessThan(300);
  const managed = await call(world, world.tokens.operator, 'PUT', `/v1/channels/general/members/${String(member.id)}`, { manages: true });
  expect(managed.status, `making ${member.name} a manager: ${JSON.stringify(managed.body)}`).toBeLessThan(300);
  const worker = await launchWorker(world, 'slow.sh', { sleep: 0 });
  const pages: WatchedPage[] = [];
  try {
    const { page } = await meet({ viewport: { width: 360, height: 740 } });
    pages.push({ page, errors: consoleErrors(page) });
    await ownerIn(page, world);
    await openChannel(page, 'general');
    const panel = await openMembers(page);
    await expect(mark(channelRow(page, member.name), 'operator')).toBeVisible();
    await expect(mark(channelRow(page, member.name), 'manager')).toBeVisible();
    await expect(mark(agentRow(page, 'worker-test'), 'agent')).toBeVisible();
    await expect(mark(communityRow(page, member.name), 'operator')).toBeVisible();

    const lists = [
      ['channel people', panel.getByRole('list', { name: /^(Online|Offline)/ }).locator('li.member-row')],
      ['agents', panel.getByRole('list', { name: 'Agents' }).locator('li.member-row')],
      ['community', panel.getByRole('list', { name: 'People' }).locator('li.community-member')],
    ] as const;
    let checked = 0;
    for (const [what, rows] of lists) {
      expect(await rows.count(), `${what} has rows`).toBeGreaterThan(0);
      for (const row of await rows.all()) {
        await row.scrollIntoViewIfNeeded();
        const more = row.getByRole('button', { name: /^Actions for / });
        if ((await more.count()) === 0) continue;
        const rowBox = await row.boundingBox();
        const moreBox = await more.boundingBox();
        if (rowBox === null || moreBox === null) throw new Error(`${what}: a row or its ⋯ control has no box`);
        const where = `${what}: ⋯ at x ${String(moreBox.x)}, ${String(moreBox.width)} wide, in a row at x ${String(rowBox.x)}, ${String(rowBox.width)} wide`;
        expect(moreBox.x, where).toBeGreaterThanOrEqual(0);
        expect(moreBox.x + moreBox.width, where).toBeLessThanOrEqual(360);
        expect(moreBox.x, where).toBeGreaterThanOrEqual(rowBox.x - 1);
        expect(moreBox.x + moreBox.width, where).toBeLessThanOrEqual(rowBox.x + rowBox.width + 1);
        checked++;
      }
    }
    expect(checked, 'rows with a ⋯ control were measured').toBeGreaterThan(2);
    await expectFits(page, '360: marked member lists');
    const panelScroll = await panel.evaluate((el) => ({ scroll: el.scrollWidth, client: el.clientWidth }));
    expect(panelScroll.scroll, 'the members panel scrolls sideways').toBeLessThanOrEqual(panelScroll.client);
  } finally {
    await worker.stop();
    await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(member.id)}/role`, { role: 'member' });
  }
  expectClean(pages);
});
