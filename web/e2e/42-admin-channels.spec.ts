// Issue #1021 (Admin A9, ADR-014): the console's Channels section, against the
// real server. On a personal server the owner makes a private channel and adds
// a person to it through Add person, the first item of the row's menu; a
// manager is made and a person removed; and a phone does the same from the
// sheet its ⋯ button opens.
//
// The suite shares one server and these specs change its permissions document
// (the Personal preset), so every test reads the document first and puts every
// cell it touched back in a `finally`. The people and channels made here have
// names no other spec uses.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { channelDirectoryResponseV1, listChannelMembersResponseV1 } from '../src/schema/channel';
import { createPrincipalResponse, type CreatePrincipalRequest } from '../src/schema/credential';
import { permissionsResponseV1, type PermissionChangeV1, type PermissionsResponseV1 } from '../src/schema/permission';
import { expect, expectFits, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const stamp = Date.now().toString(36).slice(-6);

const read = (world: World) => ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);

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

async function personal(world: World): Promise<void> {
  const now = await read(world);
  const put = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', { revision: now.revision, preset: 'personal' });
  expect(put.status, `applying Personal: ${JSON.stringify(put.body)}`).toBeLessThan(300);
}

async function ownerIn(page: Page, world: World): Promise<void> {
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
}

async function person(world: World, name: string): Promise<number> {
  const request: CreatePrincipalRequest = { kind: 'human', name };
  return (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
}

const membersOf = async (world: World, channel: string) => (await ok(world, world.tokens.operator, 'GET', `/v1/channels/${encodeURIComponent(channel)}/members`, listChannelMembersResponseV1)).members;

const desktop: BrowserContextOptions = { viewport: { width: 1440, height: 900 } };
const phone: BrowserContextOptions = { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true };

test('A1021 at 1440×900 on a personal server the owner makes a private channel, adds a person to it, makes them a manager and removes them', async ({ meet, world }) => {
  const before = await read(world);
  const channel = `c1021-${stamp}-w`;
  const friend = `friend-${stamp}-w`;
  try {
    await personal(world);
    const friendId = await person(world, friend);
    const { page } = await meet(desktop);
    await ownerIn(page, world);
    await page.getByRole('button', { name: 'Settings' }).click();
    const region = page.getByRole('region', { name: 'Channels' });
    await expect(region.getByRole('list', { name: 'Every channel' })).toBeVisible();

    // Make a private channel, with no daily cap.
    await region.getByRole('button', { name: 'New channel' }).click();
    await region.getByLabel('Channel name').fill(channel);
    await region.getByRole('radio', { name: /Private/ }).check();
    await region.getByRole('button', { name: 'Create channel' }).click();
    const row = region.locator(`[data-channel="${channel}"]`);
    await expect(row).toBeVisible();
    await expect(row.getByText('Private')).toBeAttached();
    await expect(row).toContainText('1 person');
    const created = (await ok(world, world.tokens.operator, 'GET', '/v1/directory/channels?scope=all', channelDirectoryResponseV1)).channels.find((c) => c.name === channel);
    expect(created?.visibility).toBe('private');

    // On a personal server Add person is the row's first action.
    await row.getByRole('button', { name: `Actions for ${channel}` }).click();
    await expect(row.getByRole('menuitem').first()).toHaveText('Add person…');
    await row.getByRole('menuitem', { name: 'Add person…' }).click();
    await row.getByLabel('Their name').fill(friend);
    await row.getByRole('button', { name: 'Add person', exact: true }).click();
    await expect(row.getByText(`Added ${friend} to ${channel}.`)).toBeVisible();
    await expect(row).toContainText('2 people');
    expect((await membersOf(world, channel)).map((m) => m.principal_id)).toContain(friendId);

    // Make them a manager, then take them out.
    await row.getByRole('button', { name: `Actions for ${channel}` }).click();
    await row.getByRole('menuitem', { name: 'People and managers…' }).click();
    await row.getByRole('button', { name: `Let ${friend} manage ${channel}` }).click();
    // Asked first, in place.
    await row.getByRole('button', { name: `Make ${friend} a manager` }).click();
    await expect(row).toContainText(`Managed by`);
    await expect(row).toContainText(friend);
    expect((await membersOf(world, channel)).find((m) => m.principal_id === friendId)?.manages).toBe(true);

    await row.getByRole('button', { name: `Remove ${friend} from ${channel}` }).click();
    await row.getByRole('button', { name: `Remove ${friend}`, exact: true }).click();
    await expect(row).toContainText('1 person');
    expect((await membersOf(world, channel)).map((m) => m.principal_id)).not.toContain(friendId);
    await expectFits(page, '1440×900: Channels');
  } finally {
    await restore(world, before);
  }
});

test('A1021 at 390×844 the phone adds a person from the sheet its ⋯ button opens', async ({ meet, world }) => {
  const before = await read(world);
  const channel = `c1021-${stamp}-p`;
  const friend = `friend-${stamp}-p`;
  try {
    await personal(world);
    const friendId = await person(world, friend);
    const made = await call(world, world.tokens.operator, 'POST', '/v1/channels', { name: channel, visibility: 'private' });
    expect(made.status, `making ${channel}: ${JSON.stringify(made.body)}`).toBe(201);
    const { page } = await meet(phone);
    await ownerIn(page, world);
    await page.getByRole('button', { name: 'Settings' }).tap();
    const dialog = page.getByRole('dialog', { name: 'Settings' });
    await dialog.getByRole('button', { name: 'Channels', exact: true }).tap();
    const row = dialog.locator(`[data-channel="${channel}"]`);
    await expect(row).toBeVisible();
    // The drawer's header stays at the top: bring the row to the middle, as a finger would.
    await row.evaluate((el) => {
      el.scrollIntoView({ block: 'center' });
    });
    await row.getByRole('button', { name: `Actions for ${channel}` }).tap();
    const sheet = row.getByRole('menu');
    await expect(sheet.getByRole('menuitem').first()).toHaveText('Add person…');
    // The sheet rests on the bottom of the screen, its items finger-sized.
    const box = await sheet.boundingBox();
    expect((box?.y ?? 0) + (box?.height ?? 0)).toBeGreaterThanOrEqual(844 - 1);
    for (const item of await sheet.getByRole('menuitem').all()) expect((await item.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(43.5);
    await sheet.getByRole('menuitem', { name: 'Add person…' }).tap();
    await row.getByLabel('Their name').fill(friend);
    await row.getByRole('button', { name: 'Add person', exact: true }).tap();
    await expect(row.getByText(`Added ${friend} to ${channel}.`)).toBeVisible();
    expect((await membersOf(world, channel)).map((m) => m.principal_id)).toContain(friendId);

    // People, with a Remove confirmation open: every control in the row is finger-sized.
    await row.getByRole('button', { name: `Actions for ${channel}` }).tap();
    await row.getByRole('menuitem', { name: 'People and managers…' }).tap();
    await row.getByRole('button', { name: `Remove ${friend} from ${channel}` }).tap();
    await expect(row.getByRole('group', { name: `Remove ${friend}` })).toBeVisible();
    const small = await row.evaluate((el) => {
      const found: string[] = [];
      for (const control of el.querySelectorAll<HTMLElement>('button, input[type="text"]')) {
        if (control.closest('[hidden]') !== null) continue;
        const box = control.getBoundingClientRect();
        if (box.width === 0 || box.height === 0) continue;
        if (box.height < 43.5) found.push(`${control.tagName.toLowerCase()} "${control.textContent.trim().slice(0, 40)}" is ${String(Math.round(box.height))} px high`);
      }
      return found;
    });
    expect(small, '390×844: the controls of a row with a Remove confirmation open').toEqual([]);
    await expectFits(page, '390×844: Channels, Remove asked');
    await row.getByRole('button', { name: 'Keep' }).tap();
    expect((await membersOf(world, channel)).map((m) => m.principal_id)).toContain(friendId);
    await expectFits(page, '390×844: Channels');
  } finally {
    await restore(world, before);
  }
});
