// Issue #1669: an operator with manage_channels or manage_members turned off
// is offered only the controls it still holds, and the server refuses the
// rest when the page sends them anyway (docs/testing.md, the row "Operator
// with a cell off"). Two tests, one per cell, each with its own operator, its
// own channels and its own target, whose names are digits of a fixed width, so
// the page's layout never depends on them (issue #1647).
//
// The suite shares one server and these specs change its permissions
// document. Each test reads the two cells first, restores them in a `finally`
// and checks the restore after it, so a failure in the body is not masked.
import type { Locator, Page } from '@playwright/test';
import { createChannelResponse, type CreateChannelRequest } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { permissionsResponseV1, type PermissionChangeV1 } from '../src/schema/permission';
import { credentialField, expect, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const desktop = { viewport: { width: 1440, height: 900 } };
const cells = ['manage_channels', 'manage_members', 'act_as_members'];

const read = (world: World) => ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);

// operatorCells is the operator column's value for each cell this spec turns off.
async function operatorCells(world: World): Promise<Record<string, boolean>> {
  const doc = await read(world);
  const out: Record<string, boolean> = {};
  for (const permission of cells) {
    const row = doc.rows.find((r) => r.permission === permission);
    if (row === undefined) throw new Error(`the permissions document has no ${permission}`);
    out[permission] = row.operator;
  }
  return out;
}

async function setOperatorCells(world: World, values: Readonly<Record<string, boolean>>): Promise<number> {
  const now = await read(world);
  const changes: PermissionChangeV1[] = Object.entries(values).map(([permission, granted]) => ({ permission, role: 'operator', granted }));
  const put = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', { revision: now.revision, changes });
  return put.status;
}

// stamp is eight digits, so a name made of a letter or two and a stamp is as
// wide as it is long (issue #1647).
const stamp = (): string => String(Date.now()).slice(-8);

interface Person {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

async function person(world: World, secrets: string[], name: string, role: 'member' | 'operator'): Promise<Person> {
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
  if (role === 'operator') {
    const made = await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(id)}/role`, { role: 'operator' });
    expect(made.status, `making ${name} an operator: ${JSON.stringify(made.body)}`).toBeLessThan(300);
  }
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  return { id, name, token };
}

async function channel(world: World, name: string, visibility: 'public' | 'private'): Promise<number> {
  if (visibility === 'public') {
    const request: CreateChannelRequest = { name };
    return (await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request)).channel.id;
  }
  const made = await call(world, world.tokens.operator, 'POST', '/v1/channels', { name, visibility });
  expect(made.status, `making ${name}: ${JSON.stringify(made.body)}`).toBe(201);
  return (made.body as { channel: { id: number } }).channel.id;
}

async function join(world: World, name: string, who: number): Promise<void> {
  const added = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(name)}/members/${String(who)}`, {});
  expect(added.status, `adding ${String(who)} to ${name}: ${JSON.stringify(added.body)}`).toBeLessThan(300);
}

async function signedIn(page: Page, world: World, token: string): Promise<void> {
  await page.goto(world.appUrl);
  await (await credentialField(page)).fill(token);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
}

interface Attempt {
  readonly action: string;
  readonly method: 'GET' | 'POST' | 'PUT' | 'DELETE';
  readonly path: string;
  readonly body?: object;
  readonly status: number;
  readonly code: string;
  readonly permission?: string;
}

// attempt sends a request from the page, with the page's own credential, and
// returns what the server answered.
async function send(page: Page, token: string, a: Attempt): Promise<{ readonly status: number; readonly text: string }> {
  return page.evaluate(
    async ({ method, path, bearer, payload }) => {
      const init: RequestInit = { method, headers: { Authorization: `Bearer ${bearer}`, 'Content-Type': 'application/json', Accept: 'application/json' } };
      if (payload !== undefined) init.body = JSON.stringify(payload);
      const res = await fetch(path, init);
      return { status: res.status, text: await res.text() };
    },
    { method: a.method, path: a.path, bearer: token, payload: a.body },
  );
}

async function refused(page: Page, token: string, attempts: readonly Attempt[]): Promise<void> {
  for (const a of attempts) {
    const reply = await send(page, token, a);
    expect(reply.status, `${a.action}: ${reply.text}`).toBe(a.status);
    const body = JSON.parse(reply.text) as { code?: unknown; permission?: unknown };
    expect(body.code, a.action).toBe(a.code);
    if (a.permission !== undefined) expect(body.permission, a.action).toBe(a.permission);
  }
}

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });
const settings = (page: Page) => page.getByRole('dialog', { name: 'Settings' });

async function menuOf(row: Locator, label: string): Promise<Locator> {
  await row.getByRole('button', { name: `Actions for ${label}` }).click();
  return row.getByRole('menuitem');
}

// finish puts the cells back and hides the people a test made, so no operator
// it made stays on the shared server. It asserts nothing: it runs in a
// `finally`, and an `expect` there would mask the body's error. The caller
// checks what it returns after the `finally`.
async function finish(world: World, before: Readonly<Record<string, boolean>>, made: readonly number[]): Promise<number[]> {
  const statuses = [await setOperatorCells(world, before)];
  for (const id of made) statuses.push((await call(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/hide`)).status);
  return statuses;
}

test('R1669 an operator with manage_channels off is offered no rename, archive, visibility change or private-channel view, and the server refuses each', async ({ meet, secrets, world }) => {
  const before = await operatorCells(world);
  const n = stamp();
  const open = `c${n}`;
  const hidden = `q${n}`;
  const made: number[] = [];
  let statuses: number[] = [];
  try {
    const operator = await person(world, secrets, `oa${n}`, 'operator');
    made.push(operator.id);
    const target = await person(world, secrets, `ta${n}`, 'member');
    made.push(target.id);
    const openId = await channel(world, open, 'public');
    await channel(world, hidden, 'private');
    await join(world, open, operator.id);
    await join(world, open, target.id);

    expect(await setOperatorCells(world, { manage_channels: false })).toBeLessThan(300);
    const { page } = await meet(desktop);
    await signedIn(page, world, operator.token);

    // The channel's menu holds what a plain member holds: no Rename channel…,
    // Set topic, Make private, Make default channel or Archive channel….
    const row = nav(page).locator('li').filter({ has: page.getByRole('button', { name: open, exact: true }) });
    await expect(row).toBeVisible();
    await expect(await menuOf(row, open)).toHaveText(['Invite people', 'Leave channel', 'Favorite']);
    await page.keyboard.press('Escape');
    // The private channel it is not in is not listed; New channel and Browse channels remain.
    await expect(nav(page).getByRole('button', { name: hidden, exact: true })).toHaveCount(0);
    await expect(nav(page).getByRole('button')).toHaveText([open, '⋯', 'Browse channels', 'New channel']);

    // The console has no Channels section, the admin view of every channel;
    // the Members section remains, and its menu has no Add to a channel….
    await page.getByRole('button', { name: 'Settings' }).click();
    const dialog = settings(page);
    const members = dialog.getByRole('region', { name: 'Members', exact: true });
    await expect(members.getByRole('list', { name: 'People' })).toBeVisible();
    await expect(dialog.getByRole('heading', { name: 'Channels', exact: true })).toHaveCount(0);
    await expect(dialog.getByRole('region', { name: 'Channels', exact: true })).toHaveCount(0);
    await expect(dialog.getByRole('list', { name: 'Every channel' })).toHaveCount(0);
    const person1 = members.locator('li.community-member').filter({ has: page.locator('.community-name', { hasText: new RegExp(`^${target.name}$`) }) });
    await expect(await menuOf(person1, target.name)).toHaveText([
      'Turn voice off',
      'Allow member links',
      'Make a link that signs in as this person…',
      'Make a credential that acts as this person…',
      'Make a hook that posts as this person…',
      'Sign out everywhere…',
      'Disable sign-in…',
      'Enable sign-in',
      'Remove from server…',
    ]);

    // Each hidden action, sent from this page with this page's credential.
    const channelPermission = { status: 403, code: 'permission_denied', permission: 'manage_channels' } as const;
    const notManager = { status: 403, code: 'forbidden' } as const;
    await refused(page, operator.token, [
      { action: 'Rename channel', method: 'PUT', path: `/v1/channels/${String(openId)}/name`, body: { name: `r${n}` }, ...notManager },
      { action: 'Make private', method: 'PUT', path: `/v1/channels/${String(openId)}/settings`, body: { visibility: 'private' }, ...channelPermission },
      { action: 'Make default', method: 'PUT', path: `/v1/channels/${String(openId)}/settings`, body: { default: true }, ...channelPermission },
      { action: 'Archive', method: 'PUT', path: `/v1/channels/${String(openId)}/settings`, body: { archived: true }, ...channelPermission },
      { action: 'Every channel (admin view)', method: 'GET', path: '/v1/directory/channels?scope=all', ...channelPermission },
      { action: 'Ban from channel', method: 'PUT', path: `/v1/channels/${open}/bans/${String(target.id)}`, body: {}, ...notManager },
      { action: 'Remove from channel', method: 'DELETE', path: `/v1/channels/${open}/members/${String(target.id)}`, ...notManager },
      { action: 'Private channel: add a member', method: 'PUT', path: `/v1/channels/${hidden}/members/${String(target.id)}`, body: {}, status: 404, code: 'channel_not_found' },
      { action: 'Private channel: its members', method: 'GET', path: `/v1/channels/${hidden}/members`, status: 404, code: 'channel_not_found' },
    ]);
  } finally {
    statuses = await finish(world, before, made);
  }
  // Checked outside the finally, so a failure in the body is not masked.
  expect(await operatorCells(world)).toEqual(before);
  expect(statuses.map((s) => s < 300)).toEqual(statuses.map(() => true));
});

test('R1669 an operator with manage_members off is offered no removal, disabling or role change, and the server refuses each', async ({ meet, secrets, world }) => {
  const before = await operatorCells(world);
  const n = stamp();
  const open = `c${n}`;
  const made: number[] = [];
  let statuses: number[] = [];
  try {
    const operator = await person(world, secrets, `ob${n}`, 'operator');
    made.push(operator.id);
    const target = await person(world, secrets, `tb${n}`, 'member');
    made.push(target.id);
    await channel(world, open, 'public');
    await join(world, open, operator.id);
    await join(world, open, target.id);

    // act_as_members needs manage_members in the same column, so the one
    // patch turns both off.
    expect(await setOperatorCells(world, { manage_members: false, act_as_members: false })).toBeLessThan(300);
    const { page } = await meet(desktop);
    await signedIn(page, world, operator.token);

    // The server-level menu on a person is Add to a channel… alone: no
    // voice or member-link switch, sign-out, Disable sign-in…, Remove from
    // server…, Change role…, or anything that acts as the person.
    await page.getByRole('button', { name: 'Settings' }).click();
    const dialog = settings(page);
    const members = dialog.getByRole('region', { name: 'Members', exact: true });
    await expect(members.getByRole('list', { name: 'People' })).toBeVisible();
    const person1 = members.locator('li.community-member').filter({ has: page.locator('.community-name', { hasText: new RegExp(`^${target.name}$`) }) });
    await expect(await menuOf(person1, target.name)).toHaveText(['Add to a channel…']);
    await page.keyboard.press('Escape');
    // manage_channels is still held, so the Channels section is.
    await expect(dialog.getByRole('heading', { name: 'Channels', exact: true })).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(dialog).toHaveCount(0);

    // The channel's own moderation is manage_channels's, and stays: the
    // channel member panel still offers the four channel-level actions.
    const row = nav(page).locator('li').filter({ has: page.getByRole('button', { name: open, exact: true }) });
    await expect(await menuOf(row, open)).toHaveText(['Invite people', 'Rename channel…', 'Set topic', 'Leave channel', 'Make public', 'Make default channel', 'Archive channel…', 'Favorite']);
    await page.keyboard.press('Escape');
    await row.getByRole('button', { name: open, exact: true }).click();
    const panel = page.getByRole('region', { name: 'Members list' });
    const prow = panel.locator('li').filter({ has: page.locator('.ag-name').getByText(target.name, { exact: true }) });
    await expect(await menuOf(prow, target.name)).toHaveText(['Remove from voice', 'Ban from channel', 'Remove from channel', 'Make manager', 'Stop deciding approvals']);
    await page.keyboard.press('Escape');

    const membersPermission = { status: 403, code: 'permission_denied', permission: 'manage_members' } as const;
    const id = String(target.id);
    await refused(page, operator.token, [
      { action: 'Remove from server', method: 'POST', path: `/v1/principals/${id}/hide`, ...membersPermission },
      { action: 'Disable sign-in', method: 'POST', path: `/v1/principals/${id}/disable`, ...membersPermission },
      { action: 'Enable sign-in', method: 'POST', path: `/v1/principals/${id}/enable`, ...membersPermission },
      { action: 'Turn voice off', method: 'PUT', path: `/v1/principals/${id}/voice-permission`, body: { voice_allowed: false }, ...membersPermission },
      { action: 'Allow member links', method: 'PUT', path: `/v1/principals/${id}/invite-permission`, body: { may_invite: true }, ...membersPermission },
      { action: 'Change role', method: 'PUT', path: `/v1/principals/${id}/role`, body: { role: 'operator' }, status: 403, code: 'forbidden' },
      { action: 'Sign out everywhere (the credentials)', method: 'GET', path: `/v1/principals/${id}/credentials`, ...membersPermission },
      { action: 'Make a person', method: 'POST', path: '/v0/principals', body: { kind: 'human', name: `z${n}` }, ...membersPermission },
      { action: 'Joining settings', method: 'PUT', path: '/v1/invites/settings', body: {}, ...membersPermission },
    ]);
  } finally {
    statuses = await finish(world, before, made);
  }
  // Checked outside the finally, so a failure in the body is not masked.
  expect(await operatorCells(world)).toEqual(before);
  expect(statuses.map((s) => s < 300)).toEqual(statuses.map(() => true));
});
