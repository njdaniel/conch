// Issue #1018 (Admin A6, ADR-014): the administration console and its Server
// section, against the real server. The owner finds the setup card while the
// permissions document is at its first revision, reads what Personal changes
// and applies it; an operator who was not given change_server_settings sees
// the Server writes disabled, with the reason; a member sees one line of what
// they may do and no console.
//
// This runs after the other specs and is the only one that changes the
// permissions document, so the setup card is there for it: if the revision
// has moved the test says so rather than failing on a missing card. The
// operator and the member are made here, with names of their own.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { permissionsResponseV1 } from '../src/schema/permission';
import { expect, expectFits, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const stamp = Date.now().toString(36).slice(-6);

async function person(world: World, name: string, role: 'member' | 'operator'): Promise<string> {
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
  if (role === 'operator') {
    const made = await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(id)}/role`, { role: 'operator' });
    expect(made.status, `making ${name} an operator: ${JSON.stringify(made.body)}`).toBeLessThan(300);
  }
  const credential: CreateCredentialRequestV1 = { label: name };
  return (await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential)).token;
}

// operatorIn signs an operator in with a script credential, which the page
// warns about, and waits for the lobby.
async function operatorIn(page: Page, world: World, token: string, name: string): Promise<void> {
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(token);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText(name);
  await expect(statusLine(page)).toContainText('Connected');
}

const sizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }, true],
];

test('A1018 at 1440×900 the owner reads what Personal changes in the setup card and applies it, and the card goes', async ({ meet, world }) => {
  const before = await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
  expect(before.revision, `the permissions document is at revision ${String(before.revision)}, and the setup card is only there at revision 1; a spec before this one changed the permissions document`).toBe(1);
  // The suite shares one server: whatever happens, put the document back on a
  // preset that leaves members what they had, so a later spec does not inherit Personal.
  try {
    const { page } = await meet({ viewport: { width: 1440, height: 900 } });
    await operatorIn(page, world, world.tokens.operator, 'operator');
    await page.getByRole('button', { name: 'Settings' }).click();
    const group = page.getByRole('group', { name: 'Server settings' });
    await expect(group.getByText('Set on the server’s host')).toBeVisible();
    // The name is text, not a field.
    await expect(group.getByLabel(/server name/i)).toHaveCount(0);
    const card = group.getByRole('region', { name: 'Set up this server' });
    await expect(card.getByText('How will this server be used?')).toBeVisible();
    await card.getByRole('button', { name: 'Personal server' }).click();
    const ask = card.getByRole('group', { name: 'Apply Personal server' });
    await expect(ask).toBeVisible();
    // Asking changes nothing.
    expect((await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1)).revision).toBe(1);
    await ask.getByRole('button', { name: 'Apply Personal server' }).click();
    await expect(card).toHaveCount(0);
    const after = await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
    expect(after.revision).toBeGreaterThan(1);
    expect(after.preset).toBe('personal');
    await expect(group.getByText(/Members see:/)).toBeVisible();
    await expect(group.getByText(/Voice: on for members/)).toBeVisible();
  } finally {
    const current = await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
    const restored = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', { revision: current.revision, preset: 'group' });
    expect(restored.status, `restoring the group preset: ${JSON.stringify(restored.body)}`).toBeLessThan(300);
    expect(restored.status).toBeGreaterThanOrEqual(200);
  }
});

for (const [size, options, phone] of sizes) {
  test(`A1018 at ${size} an operator without change_server_settings sees the Server writes disabled, with the reason`, async ({ meet, secrets, world }) => {
    const name = `operator-${stamp}${phone ? 'p' : 'w'}`;
    const token = await person(world, name, 'operator');
    secrets.push(token);
    const { page } = await meet(options);
    await operatorIn(page, world, token, name);
    await page.getByRole('button', { name: 'Settings' }).click();
    if (phone) await page.getByRole('button', { name: 'Server', exact: true }).tap();
    await page.getByRole('button', { name: 'Joining' }).click();
    const region = page.getByRole('region', { name: 'Joining' });
    await expect(region.getByText('Needs the permission to change server settings.')).toBeVisible();
    await expect(region.getByLabel(/Open sign-up/)).toBeDisabled();
    await expect(region.getByLabel('Pause joining from the address')).toBeDisabled();
    await expect(region.getByRole('button', { name: 'Save joining' })).toBeDisabled();
    // Pausing every invite is manage_members's, which an operator holds.
    await expect(region.getByLabel('Pause all invites')).toBeEnabled();
    await expectFits(page, `${size}: the Server section as an operator`);
  });

  test(`A1018 at ${size} a member sees one line of what they may do and no console`, async ({ meet, secrets, world }) => {
    const name = `member-${stamp}${phone ? 'p' : 'w'}`;
    const token = await person(world, name, 'member');
    secrets.push(token);
    const { page } = await meet(options);
    await page.goto(world.appUrl);
    await page.getByLabel('Credential').fill(token);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await expect(statusLine(page)).toContainText('Connected');
    await page.getByRole('button', { name: 'Settings' }).click();
    const settings = page.getByRole('dialog', { name: 'Settings' });
    await expect(settings.getByText(/^You can .*Voice is (on|off)\.$/)).toBeVisible();
    await expect(settings.getByRole('heading', { name: 'Server administration' })).toHaveCount(0);
    await expect(settings.getByRole('button', { name: 'Joining' })).toHaveCount(0);
    await expect(settings.getByRole('button', { name: 'Server', exact: true })).toHaveCount(0);
    await expectFits(page, `${size}: a member's Settings`);
  });
}
