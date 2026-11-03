// Issue #1020 (Admin A8, ADR-014, #972): the console's Members section and the
// one permission-aware menu on a person, against the real server. The owner
// changes a person's role from the menu; an operator who was not given
// act_as_members is offered no link, credential or hook that signs in or acts
// as someone; the browser's own right-click menu is kept everywhere but on a
// person's row; and a phone does the same from the sheet its ⋯ button opens,
// asking before it removes anyone.
//
// The suite shares one server and these specs change its permissions document,
// so the test that does reads the document first and puts every cell it
// touched back in a `finally`. The people made here have names no other spec
// uses.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { permissionsResponseV1, type PermissionChangeV1, type PermissionsResponseV1 } from '../src/schema/permission';
import { serverMembersResponseV1, type ServerMemberV1 } from '../src/schema/principal';
import { expect, expectFits, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const stamp = Date.now().toString(36).slice(-6);

const read = (world: World) => ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
// directory reads every page of the community list, as the page does.
async function directory(world: World): Promise<ServerMemberV1[]> {
  const all: ServerMemberV1[] = [];
  let after = 0;
  for (let page = 0; page < 50; page++) {
    const query = `limit=100${after === 0 ? '' : `&after=${String(after)}`}`;
    const got = await ok(world, world.tokens.operator, 'GET', `/v1/directory/members?${query}`, serverMembersResponseV1);
    all.push(...got.members);
    if (got.after === undefined || got.after <= after) break;
    after = got.after;
  }
  return all;
}

// restore puts back every cell that differs from before, by patch.
async function restore(world: World, before: PermissionsResponseV1): Promise<void> {
  const now = await read(world);
  const changes: PermissionChangeV1[] = [];
  for (const row of now.rows) {
    const was = before.rows.find((r) => r.permission === row.permission);
    if (was === undefined) continue;
    if (!row.member_fixed && row.member !== was.member) changes.push({ permission: row.permission, role: 'member', granted: was.member });
    if (!row.operator_fixed && row.operator !== was.operator) changes.push({ permission: row.permission, role: 'operator', granted: was.operator });
  }
  if (changes.length === 0) return;
  const put = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', { revision: now.revision, changes });
  expect(put.status, `restoring the permissions: ${JSON.stringify(put.body)}`).toBeLessThan(300);
}

async function signedIn(page: Page, world: World, token: string): Promise<void> {
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(token);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
}

async function person(world: World, name: string, role: 'member' | 'operator' = 'member'): Promise<{ readonly id: number; readonly token: string }> {
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
  if (role === 'operator') {
    const made = await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(id)}/role`, { role: 'operator' });
    expect(made.status, `making ${name} an operator: ${JSON.stringify(made.body)}`).toBeLessThan(300);
  }
  const credential: CreateCredentialRequestV1 = { label: name };
  const token = (await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential)).token;
  return { id, token };
}

const desktop: BrowserContextOptions = { viewport: { width: 1440, height: 900 } };
const phone: BrowserContextOptions = { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true };

// The section, inside Settings: the member panel beside the conversation lists the same people.
const section = (page: Page) => page.getByRole('dialog', { name: 'Settings' }).getByRole('region', { name: 'Members', exact: true });

const rowOf = (page: Page, name: string) => section(page).locator('li.community-member').filter({ has: page.locator('.community-name', { hasText: new RegExp(`^${name}$`) }) });

test('A1020 at 1440×900 the owner changes a role from a person’s menu, and the server has it', async ({ meet, world }) => {
  const name = `member-${stamp}-w`;
  const target = await person(world, name);
  const { page } = await meet(desktop);
  await signedIn(page, world, world.tokens.operator);
  await page.getByRole('button', { name: 'Settings' }).click();
  const region = section(page);
  await expect(region.getByRole('list', { name: 'People' })).toBeVisible();
  const row = rowOf(page, name);
  await expect(row).toBeVisible();

  await row.getByRole('button', { name: `Actions for ${name}` }).click();
  await expect(row.getByRole('menuitem', { name: 'Change role…' })).toBeVisible();
  // The items that act as the person say so.
  await expect(row.getByRole('menuitem', { name: 'Make a link that signs in as this person…' })).toBeVisible();
  await row.getByRole('menuitem', { name: 'Change role…' }).click();
  await row.getByRole('radio', { name: 'Operator' }).check();
  await row.getByRole('button', { name: 'Make an operator' }).click();
  await expect(region.getByText(new RegExp(`^${name} is now an operator`))).toBeVisible();
  expect((await directory(world)).find((m) => m.id === target.id)?.role).toBe('operator');
  await expectFits(page, '1440×900: Members');
});

test('A1020 at 1440×900 an operator without act_as_members is offered no sign-in link, credential or hook', async ({ meet, secrets, world }) => {
  const before = await read(world);
  const operatorName = `operator-${stamp}-w`;
  const memberName = `member-${stamp}-n`;
  try {
    const operator = await person(world, operatorName, 'operator');
    secrets.push(operator.token);
    await person(world, memberName);
    const now = await read(world);
    const put = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', {
      revision: now.revision,
      changes: [{ permission: 'act_as_members', role: 'operator', granted: false }],
    });
    expect(put.status, `taking act_as_members from operators: ${JSON.stringify(put.body)}`).toBeLessThan(300);
    const { page } = await meet(desktop);
    await signedIn(page, world, operator.token);
    await page.getByRole('button', { name: 'Settings' }).click();
    await expect(section(page).getByRole('list', { name: 'People' })).toBeVisible();
    const row = rowOf(page, memberName);
    await row.getByRole('button', { name: `Actions for ${memberName}` }).click();
    await expect(row.getByRole('menuitem', { name: 'Sign out everywhere…' })).toBeVisible();
    await expect(row.getByRole('menuitem', { name: /signs in as|acts as|posts as/ })).toHaveCount(0);
    // An owner is not theirs to act on.
    await page.keyboard.press('Escape');
    const owner = (await directory(world)).find((m) => m.owner === true);
    expect(owner).toBeDefined();
    await expect(rowOf(page, owner?.name ?? '').getByRole('button', { name: /^Actions for/ })).toHaveCount(0);
  } finally {
    await restore(world, before);
  }
});

test('A1020 at 1440×900 the browser’s own right-click menu is kept everywhere but on a person’s row', async ({ meet, world }) => {
  const name = `member-${stamp}-c`;
  await person(world, name);
  const { page } = await meet(desktop);
  await signedIn(page, world, world.tokens.operator);
  await page.getByRole('button', { name: 'Settings' }).click();
  const region = section(page);
  await expect(region.getByRole('list', { name: 'People' })).toBeVisible();
  // Record whether the page cancelled each contextmenu event.
  await page.evaluate(() => {
    const seen: boolean[] = [];
    Object.defineProperty(window, 'contextmenuCancelled', { value: seen });
    document.addEventListener('contextmenu', (event) => {
      seen.push(event.defaultPrevented);
    });
  });
  const cancelled = () => page.evaluate(() => (window as unknown as { contextmenuCancelled: boolean[] }).contextmenuCancelled.slice());

  await region.getByRole('heading', { name: 'Members', exact: true }).click({ button: 'right' });
  await page.getByRole('heading', { name: 'Server administration' }).click({ button: 'right' });
  expect(await cancelled(), 'outside a person’s row the native menu is kept').toEqual([false, false]);

  const row = rowOf(page, name);
  await row.locator('.community-name').click({ button: 'right' });
  await expect(row.getByRole('menu')).toBeVisible();
  expect((await cancelled()).at(-1), 'on a person’s row the page’s menu replaces it').toBe(true);
  await page.keyboard.press('Escape');
  await expect(row.getByRole('menu')).toHaveCount(0);
  await expect(row.getByRole('button', { name: `Actions for ${name}` })).toBeFocused();
});

test('A1020 at 390×844 the phone opens a sheet from the ⋯ button, and asks before it removes anyone', async ({ meet, world }) => {
  const name = `member-${stamp}-p`;
  const target = await person(world, name);
  const { page } = await meet(phone);
  await signedIn(page, world, world.tokens.operator);
  await page.getByRole('button', { name: 'Settings' }).tap();
  const dialog = page.getByRole('dialog', { name: 'Settings' });
  await dialog.getByRole('button', { name: 'Members', exact: true }).tap();
  const region = section(page);
  const row = rowOf(page, name);
  await expect(row).toBeVisible();
  await row.evaluate((el) => {
    el.scrollIntoView({ block: 'center' });
  });
  await row.getByRole('button', { name: `Actions for ${name}` }).tap();
  const sheet = row.getByRole('menu');
  await expect(sheet).toBeVisible();
  // The sheet rests on the bottom of the screen, its items finger-sized.
  const box = await sheet.boundingBox();
  expect((box?.y ?? 0) + (box?.height ?? 0)).toBeGreaterThanOrEqual(844 - 1);
  for (const item of await sheet.getByRole('menuitem').all()) expect((await item.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(43.5);

  await sheet.getByRole('menuitem', { name: 'Remove from server…' }).tap();
  const question = row.getByRole('group', { name: new RegExp(`Remove ${name}`) });
  await expect(question).toBeVisible();
  for (const button of await question.getByRole('button').all()) expect((await button.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(43.5);
  await expectFits(page, '390×844: Members, Remove asked');
  // Asking changed nothing.
  expect((await directory(world)).map((m) => m.id)).toContain(target.id);
  await question.getByRole('button', { name: 'Cancel' }).tap();
  await expect(question).toHaveCount(0);
  expect((await directory(world)).map((m) => m.id)).toContain(target.id);

  await row.getByRole('button', { name: `Actions for ${name}` }).tap();
  await row.getByRole('menuitem', { name: 'Remove from server…' }).tap();
  await row.getByRole('button', { name: 'Yes, remove' }).tap();
  await expect(region.getByText(`${name} is removed from the server.`)).toBeVisible();
  await expect(rowOf(page, name)).toHaveCount(0);
  expect((await directory(world)).map((m) => m.id)).not.toContain(target.id);
});
