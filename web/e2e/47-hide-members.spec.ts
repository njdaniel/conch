// Issue #996 (community model §13): when the owner turns the member
// see_member_directory cell off, a member's community panel narrows to the
// people they share a channel with, in front of their eyes, with no reload,
// and says so. Owner, alice and bob; alice shares a private channel with the
// owner and bob shares another with the owner, so alice and bob share none.
// Wide and on a phone, against the real server.
//
// The cell is the whole server's and the suite shares one server, so the test
// puts it back whatever happens.
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { createChannelResponseV1, type CreateChannelRequestV1 } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { permissionsResponseV1, type PutPermissionsRequestV1 } from '../src/schema/permission';
import { post } from './api';
import { expect, expectFits, openChannel, signIn, test } from './fixtures';
import { call, ok, type World } from './world';

const sizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }, true],
];

const scopeLine = 'You see the people you share a channel with';

interface Person {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

async function person(world: World, secrets: string[], name: string): Promise<Person> {
  const principal: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  return { id, name, token };
}

async function privateChannel(world: World, owner: Person, name: string, others: readonly Person[]): Promise<void> {
  const request: CreateChannelRequestV1 = { name, visibility: 'private' };
  await ok(world, owner.token, 'POST', '/v1/channels', createChannelResponseV1, request);
  for (const other of others) {
    const added = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(name)}/members/${String(other.id)}`);
    expect(added.status, `adding ${other.name} to ${name}: ${JSON.stringify(added.body)}`).toBeLessThan(300);
  }
}

async function memberCell(world: World, granted: boolean): Promise<void> {
  const current = await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
  const request: PutPermissionsRequestV1 = { revision: current.revision, changes: [{ permission: 'see_member_directory', role: 'member', granted }] };
  const put = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', request);
  expect(put.status, `the member see_member_directory cell to ${String(granted)}: ${JSON.stringify(put.body)}`).toBeLessThan(300);
}

async function openMembers(page: Page): Promise<Locator> {
  const toggle = page.getByRole('button', { name: 'Members' });
  if ((await toggle.getAttribute('aria-expanded')) === 'false') await toggle.click();
  const list = page.getByRole('region', { name: 'Members list' });
  await expect(list).toBeVisible();
  return list;
}

for (const [size, options, phone] of sizes) {
  test(`H996 at ${size} turning the member directory off takes bob from alice's panel and her mention list without a reload, and the panel says why`, async ({ meet, secrets, world }) => {
    const tag = `h996-${Date.now().toString(36).slice(-6)}${phone ? 'p' : 'w'}`;
    const owner = await person(world, secrets, `${tag}-owner`);
    const alice = await person(world, secrets, `${tag}-alice`);
    const bob = await person(world, secrets, `${tag}-bob`);
    const withAlice = `${tag}-a`;
    const withBob = `${tag}-b`;
    await privateChannel(world, owner, withAlice, [alice]);
    await privateChannel(world, owner, withBob, [bob]);
    await post(world, owner.token, withAlice, `${tag} owner to alice`);

    try {
      const { page } = await meet(options);
      await signIn(page, world, alice.token, alice.name);
      await openChannel(page, withAlice);
      const panel = await openMembers(page);
      const people = panel.getByRole('list', { name: 'People' });
      const row = (name: string) => people.getByRole('listitem').filter({ hasText: name });
      // With the directory on, she sees everyone, bob included, and nothing about scope.
      await expect(row(bob.name)).toBeVisible();
      await expect(panel.getByText(scopeLine)).toHaveCount(0);
      await expectFits(page, `${size}: the community panel with the directory on`);
      // A marker on the page: a reload would lose it.
      await page.evaluate(() => {
        document.body.dataset['kept'] = 'yes';
      });

      await memberCell(world, false);

      await expect(panel.getByText(scopeLine)).toBeVisible();
      await expect(row(bob.name)).toHaveCount(0);
      await expect(row(owner.name)).toBeVisible();
      await expect(row(alice.name).getByText('You')).toBeVisible();
      expect(await page.evaluate(() => document.body.dataset['kept']), 'the page reloaded').toBe('yes');
      await expectFits(page, `${size}: the community panel with the directory off`);
      // Not in her mention list either. The panel covers the box on a phone.
      if (phone) await page.keyboard.press('Escape');
      const box = page.getByLabel(`Message #${withAlice}`);
      await box.fill('@');
      await expect(page.getByRole('listbox', { name: 'Mention suggestions' }).getByRole('option', { name: `@${owner.name}` })).toBeVisible();
      await expect(page.getByRole('option', { name: `@${bob.name}` })).toHaveCount(0);
      await box.fill('');
      // And nothing the page holds names bob.
      await expect(page.locator('body')).not.toContainText(bob.name);

      // Back on, and the panel widens again, still without a reload.
      await memberCell(world, true);
      const again = await openMembers(page);
      await expect(again.getByRole('list', { name: 'People' }).getByRole('listitem').filter({ hasText: bob.name })).toBeVisible();
      await expect(again.getByText(scopeLine)).toHaveCount(0);
      expect(await page.evaluate(() => document.body.dataset['kept']), 'the page reloaded').toBe('yes');
    } finally {
      await memberCell(world, true);
    }
  });
}
