// Issue #1517 (Preview as member, docs/design/permission-preview.md §5.1, §7):
// the owner opens a member's view from the Members section, sees only what that
// member sees, checks the controls, and comes back to the owner's own view.
// The design's alice/bob world: private channel A holds the owner and alice,
// private channel B the owner and bob; neither manages anything. The scenario
// runs with member hiding on (alice sees the people she shares a channel with)
// and off (she sees everyone, bob included, and still not channel B).
//
// Also: the owner stays online as themselves for a second browser; a reload
// and a sign-out mid-preview end it; and a preview joins no voice, captures no
// microphone and makes no read mark. The suite shares one server, so the cell
// that member hiding turns is put back whatever happens.
import type { Page } from '@playwright/test';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { permissionsResponseV1, type PutPermissionsRequestV1 } from '../src/schema/permission';
import { whoAmIResponseV1 } from '../src/schema/principal';
import { post } from './api';
import { channelButton, expect, expectFits, messages, signIn, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const scopeLine = 'You see the people you share a channel with';
const placeholder = 'Messages are not shown in preview. Open the channel as yourself to read it.';

interface Person {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

async function person(world: World, secrets: string[], name: string): Promise<Person> {
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  return { id, name, token };
}

// privateChannel is made by the owner, who adds the others. Nobody else manages it.
async function privateChannel(world: World, name: string, members: readonly Person[]): Promise<void> {
  const made = await call(world, world.tokens.operator, 'POST', '/v1/channels', { name, visibility: 'private' });
  expect(made.status, `making ${name}: ${JSON.stringify(made.body)}`).toBeLessThan(300);
  for (const m of members) {
    const added = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(name)}/members/${String(m.id)}`);
    expect(added.status, `adding ${m.name} to ${name}: ${JSON.stringify(added.body)}`).toBeLessThan(300);
  }
}

async function memberCell(world: World, granted: boolean): Promise<void> {
  const current = await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
  const row = current.rows.find((r) => r.permission === 'see_member_directory');
  if (row?.member === granted) return;
  const request: PutPermissionsRequestV1 = { revision: current.revision, changes: [{ permission: 'see_member_directory', role: 'member', granted }] };
  const put = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', request);
  expect(put.status, `the member see_member_directory cell to ${String(granted)}: ${JSON.stringify(put.body)}`).toBeLessThan(300);
}

async function ownerIn(page: Page, world: World, token = world.tokens.operator): Promise<void> {
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').or(page.locator('details.operator-signin summary')).first().waitFor();
  const disclosure = page.locator('details.operator-signin');
  if (!(await disclosure.evaluate((d) => (d as HTMLDetailsElement).open))) await disclosure.locator('summary').click();
  await page.getByLabel('Credential').fill(token);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
}

// startPreview opens Settings, the Members section and alice's menu.
async function startPreview(page: Page, name: string, beforeChoosing: () => void = () => undefined): Promise<void> {
  await page.getByRole('button', { name: 'Settings' }).click();
  const region = page.getByRole('dialog', { name: 'Settings' }).getByRole('region', { name: 'Members', exact: true });
  const row = region.locator('li.community-member').filter({ has: page.locator('.community-name', { hasText: new RegExp(`^${name}$`) }) });
  await row.getByRole('button', { name: `Actions for ${name}` }).click();
  beforeChoosing();
  await row.getByRole('menuitem', { name: 'Preview as member' }).click();
  await expect(page.getByRole('region', { name: 'Preview', exact: true })).toContainText(`Previewing ${name}, read-only`);
}

const band = (page: Page) => page.getByRole('region', { name: 'Preview', exact: true });

for (const hiding of [true, false]) {
  test(`P1517 with member hiding ${hiding ? 'on' : 'off'} the owner previews alice, sees only what she sees, checks the controls, and exits`, async ({ meet, secrets, world }) => {
    const tag = `p1517-${Date.now().toString(36).slice(-6)}${hiding ? 'h' : 'v'}`;
    const alice = await person(world, secrets, `${tag}-alice`);
    const bob = await person(world, secrets, `${tag}-bob`);
    const carol = await person(world, secrets, `${tag}-carol`);
    const a = `${tag}-a`;
    const b = `${tag}-b`;
    await privateChannel(world, a, [alice, carol]);
    await privateChannel(world, b, [bob]);
    const ownerName = (await ok(world, world.tokens.operator, 'GET', '/v1/whoami', whoAmIResponseV1)).name;
    try {
      await memberCell(world, !hiding);
      // A second browser: carol, who shares A with the owner and alice, watches the roster.
      const watcher = await meet();
      await signIn(watcher.page, world, carol.token, carol.name);
      await channelButton(watcher.page, a).click();
      const roster = async () => {
        const panel = watcher.page.getByRole('region', { name: 'Members list' });
        if (!(await panel.isVisible())) await watcher.page.getByRole('button', { name: 'Members' }).click();
        return panel;
      };
      const people = await roster();

      const { page } = await meet();
      const mediaAsked: string[] = [];
      await page.exposeFunction('mediaAsked', (what: string) => mediaAsked.push(what));
      await page.addInitScript(() => {
        const devices = navigator.mediaDevices as MediaDevices | undefined;
        if (devices === undefined) return;
        for (const name of ['getUserMedia', 'getDisplayMedia'] as const) {
          const real = devices[name].bind(devices);
          devices[name] = (...args: Parameters<typeof real>) => {
            (window as unknown as { mediaAsked: (what: string) => unknown }).mediaAsked(name);
            return real(...args);
          };
        }
      });
      const requests: { method: string; url: string }[] = [];
      let recording = false;
      page.on('request', (r) => {
        if (recording) requests.push({ method: r.method(), url: new URL(r.url()).pathname });
      });
      await ownerIn(page, world);
      await expect(channelButton(page, b)).toBeVisible();
      // The owner has A open, so the second browser sees them in its roster.
      await channelButton(page, a).click();
      await expect(people.getByRole('list', { name: /^Online/ }).getByText(ownerName)).toBeVisible();

      // The owner's view has both channels. Alice posts into A, so there is something unread.
      // The second mark waits out the marking interval: it is still on its timer
      // when the preview starts, and must not be sent while the preview is shown.
      await post(world, alice.token, a, `${tag} alice says hello`);
      await expect(messages(page).getByText(`${tag} alice says hello`)).toBeVisible();
      await post(world, alice.token, a, `${tag} alice says it again`);
      await expect(messages(page).getByText(`${tag} alice says it again`)).toBeVisible();
      await startPreview(page, alice.name, () => {
        recording = true;
      });

      // Only what alice sees: A, not B, and a placeholder for the conversation.
      await expect(channelButton(page, a)).toBeVisible();
      await expect(channelButton(page, b)).toHaveCount(0);
      await expect(page.getByText(placeholder)).toBeVisible();
      await expect(page.getByText(`${tag} alice says`)).toHaveCount(0);
      await expect(page.locator('.unread')).toHaveCount(0);

      // The people alice sees.
      const community = page.getByRole('region', { name: 'Community members' });
      await expect(community.locator('.community-name', { hasText: new RegExp(`^${alice.name}$`) })).toBeVisible();
      if (hiding) {
        await expect(community.getByText(scopeLine)).toBeVisible();
        await expect(community.locator('.community-name', { hasText: new RegExp(`^${bob.name}$`) })).toHaveCount(0);
      } else {
        await expect(community.getByText(scopeLine)).toHaveCount(0);
        await expect(community.locator('.community-name', { hasText: new RegExp(`^${bob.name}$`) })).toBeVisible();
      }
      // Channel B, and what it holds, is nowhere on the page.
      await expect(page.locator('body')).not.toContainText(new RegExp(`${b}(?![\\w-])`));

      // The controls: shown, not wired.
      const controls = page.getByRole('region', { name: 'Controls' });
      await expect(controls).toContainText(`${alice.name} could join voice here.`);
      for (const button of await controls.getByRole('button').all()) await expect(button).toBeDisabled();
      await expect(page.getByLabel(`Message #${a}`)).toBeDisabled();
      await expect(page.getByRole('region', { name: 'Voice preview' }).getByRole('button', { name: 'Join voice' })).toBeDisabled();
      // Not previewable: the Settings drawer holds no console of the member's.
      await expectFits(page, 'the preview');

      // The owner is online as themselves, and alice is not, for the second browser.
      await expect(people.getByRole('list', { name: /^Online/ }).getByText(ownerName)).toBeVisible();
      await expect(people.getByRole('list', { name: /^Online/ }).getByText(alice.name)).toHaveCount(0);

      // The server's word that the permissions changed re-reads the preview, without a reload.
      await memberCell(world, hiding);
      if (hiding) await expect(community.locator('.community-name', { hasText: new RegExp(`^${bob.name}$`) })).toBeVisible();
      else await expect(community.getByText(scopeLine)).toBeVisible();
      await memberCell(world, !hiding);
      await page.getByRole('button', { name: 'Refresh' }).click();
      if (hiding) await expect(community.getByText(scopeLine)).toBeVisible();
      else await expect(community.locator('.community-name', { hasText: new RegExp(`^${bob.name}$`) })).toBeVisible();

      // No write, no read mark, no voice, no microphone, from the start of the preview.
      await page.waitForTimeout(500);
      const writes = requests.filter((r) => r.method !== 'GET' && !/\/v1\/(sessions|ws-tickets)/.test(r.url));
      expect(writes, 'writes during the preview').toEqual([]);
      expect(requests.filter((r) => /read-state|\/voice\/(session|companion|share|camera)/.test(r.url) && r.method !== 'GET')).toEqual([]);
      expect(mediaAsked).toEqual([]);
      const presence = await call(world, world.tokens.operator, 'GET', `/v1/channels/${encodeURIComponent(a)}/voice`);
      expect(JSON.stringify(presence.body)).not.toContain(alice.name);

      // Exit: back to the owner's view, B and the conversation included.
      await band(page).getByRole('button', { name: 'Exit preview' }).click();
      await expect(band(page)).toHaveCount(0);
      await expect(channelButton(page, b)).toBeVisible();
      await expect(channelButton(page, a)).toBeVisible();
      await expect(page.getByText(placeholder)).toHaveCount(0);
      await expect(statusLine(page)).toContainText('Connected');
      await expect(people.getByRole('list', { name: /^Online/ }).getByText(ownerName)).toBeVisible();
    } finally {
      await memberCell(world, true);
    }
  });
}

test('P1517 a reload ends the preview, and so does signing out; nothing of it comes back', async ({ meet, secrets, world }) => {
  const tag = `p1517-${Date.now().toString(36).slice(-6)}r`;
  const alice = await person(world, secrets, `${tag}-alice`);
  const a = `${tag}-a`;
  await privateChannel(world, a, [alice]);
  const { page } = await meet();
  await ownerIn(page, world);
  await startPreview(page, alice.name);
  await expect(channelButton(page, a)).toBeVisible();
  // Nothing of the preview is kept by the page.
  expect(await page.evaluate(() => JSON.stringify([Object.keys(localStorage), Object.keys(sessionStorage)]).includes('preview'))).toBe(false);

  await page.reload();
  await expect(statusLine(page)).toContainText('Connected');
  await expect(band(page)).toHaveCount(0);
  await expect(page.getByText(placeholder)).toHaveCount(0);

  await startPreview(page, alice.name);
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByRole('button', { name: 'Yes, sign out' }).click();
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
  await expect(band(page)).toHaveCount(0);
  // Signing back in is the owner's view.
  await ownerIn(page, world);
  await expect(band(page)).toHaveCount(0);
  await expect(page.getByText(placeholder)).toHaveCount(0);
});

test('P1517 the owner losing manage_channels ends the preview, and the page says so', async ({ meet, secrets, world }) => {
  const tag = `p1517-${Date.now().toString(36).slice(-6)}x`;
  const alice = await person(world, secrets, `${tag}-alice`);
  // A second operator, who may preview until a cell is taken from them.
  const request: CreatePrincipalRequest = { kind: 'human', name: `${tag}-op` };
  const op = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
  const made = await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(op)}/role`, { role: 'operator' });
  expect(made.status, JSON.stringify(made.body)).toBeLessThan(300);
  const credential: CreateCredentialRequestV1 = { label: 'op' };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(op)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  // The operator has a channel open, so its events socket is up to carry the word.
  const home = `${tag}-home`;
  await privateChannel(world, home, [{ id: op, name: `${tag}-op`, token }]);
  const current = await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
  const had = current.rows.find((r) => r.permission === 'manage_channels')?.operator === true;
  try {
    const { page } = await meet();
    test.skip(!had, 'operators do not hold manage_channels in this world');
    await ownerIn(page, world, token);
    await channelButton(page, home).click();
    await expect(page.getByRole('heading', { name: home })).toBeVisible();
    await startPreview(page, alice.name);
    const now = await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
    const put = await call(world, world.tokens.operator, 'PUT', '/v1/permissions', { revision: now.revision, changes: [{ permission: 'manage_channels', role: 'operator', granted: false }] });
    expect(put.status, JSON.stringify(put.body)).toBeLessThan(300);
    expect((await ok(world, token, 'GET', '/v1/whoami', whoAmIResponseV1)).permissions, 'the server took manage_channels from the operator').not.toContain('manage_channels');
    await expect(page.getByText(/Preview ended/)).toBeVisible();
    await expect(band(page)).toHaveCount(0);
  } finally {
    const now = await ok(world, world.tokens.operator, 'GET', '/v1/permissions', permissionsResponseV1);
    if (had && now.rows.find((r) => r.permission === 'manage_channels')?.operator !== true) {
      await call(world, world.tokens.operator, 'PUT', '/v1/permissions', { revision: now.revision, changes: [{ permission: 'manage_channels', role: 'operator', granted: true }] });
    }
    await call(world, world.tokens.operator, 'POST', `/v1/principals/${String(op)}/hide`);
  }
});
