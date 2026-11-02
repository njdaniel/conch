// Issue #1019 (Admin A7, ADR-014): the console's Roles and permissions
// section, against the real server. The owner applies Personal and then Group
// from the preset cards; the owner turns Act as members off for operators,
// is shown what operators issued to others, and revokes one; a phone shows
// one list with a switch per row.
//
// The suite shares one server and these specs change its permissions
// document, so every test reads the document first and puts every cell it
// touched back in a `finally`, whatever happened. None of them skips on the
// document's state.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { permissionsResponseV1, type PermissionChangeV1, type PermissionsResponseV1 } from '../src/schema/permission';
import { expect, expectFits, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const stamp = Date.now().toString(36).slice(-6);

const read = (world: World) => ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);

// restore puts back every cell that differs from before, by patch, and checks
// the server took it.
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
  // One patch: the server checks it as a whole, so the order of its cells does not matter.
  const put = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', { revision: now.revision, changes });
  expect(put.status, `restoring the permissions: ${JSON.stringify(put.body)}`).toBeLessThan(300);
  expect(put.status).toBeGreaterThanOrEqual(200);
}

async function operatorIn(page: Page, world: World, token: string): Promise<void> {
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(token);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
}

async function makeOperator(world: World, name: string): Promise<{ readonly id: number; readonly token: string }> {
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
  const made = await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(id)}/role`, { role: 'operator' });
  expect(made.status, `making ${name} an operator: ${JSON.stringify(made.body)}`).toBeLessThan(300);
  const credential: CreateCredentialRequestV1 = { label: name };
  const token = (await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential)).token;
  return { id, token };
}

const desktop: BrowserContextOptions = { viewport: { width: 1440, height: 900 } };
const phone: BrowserContextOptions = { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true };

test('A1019 at 1440×900 the owner applies Personal, then Group, from the preset cards', async ({ meet, world }) => {
  const before = await read(world);
  try {
    const { page } = await meet(desktop);
    await operatorIn(page, world, world.tokens.operator);
    await page.getByRole('button', { name: 'Settings' }).click();
    const region = page.getByRole('region', { name: 'Roles and permissions' });
    await expect(region.getByRole('table')).toBeVisible();
    // The words the design fixes are on the page.
    await expect(region.getByText("can sign in as, and post as, any member who isn't an owner")).toBeVisible();
    await expect(region.locator('tfoot').getByText('Owners hold every permission.')).toBeVisible();
    await expect(region.getByText('In their channels: add and remove members, rename, stop a share, moderate. Set per channel in Channels.')).toBeVisible();

    await region.getByRole('button', { name: 'Use Personal preset' }).click();
    await expect(region.getByText('Applied Personal server.')).toBeVisible();
    expect((await read(world)).preset).toBe('personal');
    await expect(region.getByRole('checkbox', { name: 'Create public channels, Members' })).not.toBeChecked();
    await expect(region.getByRole('region', { name: 'Personal preset' })).toContainText('the members’ column matches this');

    await region.getByRole('button', { name: 'Use Group preset' }).click();
    await expect(region.getByText('Applied Group server.')).toBeVisible();
    expect((await read(world)).preset).toBe('group');
    await expect(region.getByRole('checkbox', { name: 'Create public channels, Members' })).toBeChecked();
    await expectFits(page, '1440×900: Roles and permissions');
  } finally {
    await restore(world, before);
  }
});

test('A1019 at 1440×900 a fixed cell explains itself on keyboard focus', async ({ meet, world }) => {
  const { page } = await meet(desktop);
  await operatorIn(page, world, world.tokens.operator);
  await page.getByRole('button', { name: 'Settings' }).click();
  const region = page.getByRole('region', { name: 'Roles and permissions' });
  const fixed = region.getByRole('checkbox', { name: 'Manage members, Members' });
  await fixed.focus();
  await expect(fixed).toBeFocused();
  await expect(fixed).toHaveAccessibleDescription('Fixed: members cannot hold this permission.');
  await expect(region.locator('.cell:focus-within .reason')).toBeVisible();
});

test('A1019 at 1440×900 the owner turns Act as members off for operators, sees what operators issued, and revokes one', async ({ meet, secrets, world }) => {
  const before = await read(world);
  const name = `operator-${stamp}`;
  // The people made here are left disabled, whatever happens.
  const people: number[] = [];
  try {
    const issuerMade = await makeOperator(world, name);
    people.push(issuerMade.id);
    const issuer = issuerMade.token;
    secrets.push(issuer);
    // The operator issues a credential to someone else, as operators may while
    // they hold Act as members.
    const target: CreatePrincipalRequest = { kind: 'human', name: `member-${stamp}` };
    const targetId = (await ok(world, issuer, 'POST', '/v0/principals', createPrincipalResponse, target)).principal.id;
    people.push(targetId);
    const label: CreateCredentialRequestV1 = { label: `issued-${stamp}` };
    const made = await ok(world, issuer, 'POST', `/v1/principals/${String(targetId)}/credentials`, createCredentialResponseV1, label);
    secrets.push(made.token);
    const id = made.credential.id;
    const { page } = await meet(desktop);
    await operatorIn(page, world, world.tokens.operator);
    await page.getByRole('button', { name: 'Settings' }).click();
    const region = page.getByRole('region', { name: 'Roles and permissions' });
    await region.getByRole('checkbox', { name: 'Act as members, Operators' }).click();
    await region.getByRole('button', { name: 'Save changes' }).click();
    await expect(region.getByText('Saved.')).toBeVisible();
    const list = region.getByRole('region', { name: 'Issued to others by operators' });
    await expect(list).toContainText(`Credential #${String(id)} for`);
    await expect(list.getByRole('button', { name: `Revoke credential #${String(id)}` })).toBeEnabled();
    await expect(list.getByRole('button', { name: /^Sign out everywhere/ }).first()).toBeEnabled();
    expect((await call(world, made.token, 'GET', '/v1/whoami')).status).toBe(200);

    await list.getByRole('button', { name: `Revoke credential #${String(id)}` }).click();
    await expect(list.getByText(`Revoked credential #${String(id)}.`)).toBeVisible();
    await expect(list.getByRole('button', { name: `Revoke credential #${String(id)}` })).toHaveCount(0);
    expect((await call(world, made.token, 'GET', '/v1/whoami')).status).toBe(401);

    // Turning it back on asks first, with the sentence.
    await region.getByRole('checkbox', { name: 'Act as members, Operators' }).click();
    await region.getByRole('button', { name: 'Save changes' }).click();
    const ask = region.getByRole('group', { name: 'Confirm Act as members' });
    await expect(ask).toContainText("Operators can sign in as, and post as, any member who isn't an owner.");
    expect((await read(world)).rows.find((r) => r.permission === 'act_as_members')?.operator).toBe(false);
    await ask.getByRole('button', { name: 'Turn it on for operators' }).click();
    await expect(region.getByText('Saved.')).toBeVisible();
    expect((await read(world)).rows.find((r) => r.permission === 'act_as_members')?.operator).toBe(true);
  } finally {
    await restore(world, before);
    for (const person of people) {
      const off = await call(world, world.tokens.operator, 'POST', `/v1/principals/${String(person)}/disable`);
      expect(off.status, `disabling person ${String(person)}: ${JSON.stringify(off.body)}`).toBeLessThan(300);
    }
  }
});

test('A1019 at 390×844 the phone shows one list with a switch per row, and a segmented control', async ({ meet, world }) => {
  const before = await read(world);
  try {
    const { page } = await meet(phone);
    await operatorIn(page, world, world.tokens.operator);
    await page.getByRole('button', { name: 'Settings' }).tap();
    const dialog = page.getByRole('dialog', { name: 'Settings' });
    await dialog.getByRole('button', { name: 'Roles and permissions', exact: true }).tap();
    const region = dialog.getByRole('region', { name: 'Roles and permissions' });
    await expect(region.getByRole('table')).toHaveCount(0);
    await expect(region.getByRole('switch')).toHaveCount(11);
    await expect(region.getByText('Owners hold every permission.')).toBeVisible();
    const share = region.getByRole('switch', { name: 'Share a screen, Members' });
    await expect(share).toBeChecked();
    await share.tap();
    await region.getByRole('button', { name: 'Save changes' }).tap();
    await expect(region.getByText('Saved.')).toBeVisible();
    await expect(share).not.toBeChecked();
    await region.getByRole('button', { name: 'Operators', exact: true }).tap();
    await expect(region.getByRole('switch', { name: 'Manage members, Operators' })).toBeChecked();
    await expectFits(page, '390×844: Roles and permissions');
  } finally {
    await restore(world, before);
  }
});
