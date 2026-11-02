// Issue #1183: a private channel's row carries a lock, and an operator makes a
// channel public or private, a default or not, from its ⋯ menu. A member in
// the channel sees the lock appear and go live, without a reload. Both on a wide
// window and on a phone, against the real server; the row stays one line.
//
// Each test makes its own channel, and puts it back (public, not a default) in
// a finally: no channel of the shared community is touched, and the default
// channels, #general among them, stay public.
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { createChannelResponse, listChannelsResponse, type CreateChannelRequest } from '../src/schema/channel';
import { channelSettingsResponseV1, type ChannelSettingsRequestV1 } from '../src/schema/generated';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { channelButton, expect, expectFits, showView, signIn, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });

function channelRow(page: Page, name: string): Locator {
  return nav(page).locator('li').filter({ has: page.getByRole('button', { name, exact: true }) });
}

const sizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }, true],
];

async function settings(world: World, id: number, body: ChannelSettingsRequestV1): Promise<void> {
  await ok(world, world.tokens.operator, 'PUT', `/v1/channels/${String(id)}/settings`, channelSettingsResponseV1, body);
}

async function operatorPage(meet: (options?: BrowserContextOptions) => Promise<{ page: Page }>, world: World, options: BrowserContextOptions): Promise<Page> {
  const { page } = await meet(options);
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
  return page;
}

for (const [size, options, phone] of sizes) {
  test(`P1183 at ${size} an operator makes a channel private and public from its menu, and a member sees the lock live`, async ({ meet, secrets, world }) => {
    const suffix = Date.now().toString(36).slice(-5);
    const channel = `p1183-${suffix}${phone ? 'p' : 'w'}`;
    const name = `v${suffix}${phone ? 'p' : 'w'}`;
    const request: CreateChannelRequest = { name: channel };
    const made = (await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request)).channel;
    try {
      // Start from public, whatever a new channel is.
      await settings(world, made.id, { visibility: 'public' });
      const principal: CreatePrincipalRequest = { kind: 'human', name };
      const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
      const credential: CreateCredentialRequestV1 = { label: name };
      const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
      secrets.push(token);
      const added = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`, {});
      expect(added.status).toBeLessThan(300);

      const press = async (target: Locator) => (phone ? target.tap() : target.click());

      // The member's page: the channel is public, no lock; and no ⋯ item for it.
      const seen = await meet(options);
      await signIn(seen.page, world, token, name);
      if (phone) await showView(seen.page, 'Channels');
      const memberRow = channelRow(seen.page, channel);
      await expect(memberRow).toBeVisible();
      await expect(memberRow.locator('svg')).toHaveCount(0);
      await expect(memberRow.getByRole('button', { name: `Actions for ${channel}` })).toHaveCount(0);

      // The operator's page.
      const page = await operatorPage(meet, world, options);
      if (phone) await showView(page, 'Channels');
      const row = channelRow(page, channel);
      const more = row.getByRole('button', { name: `Actions for ${channel}` });
      await expect(row.locator('svg')).toHaveCount(0);
      await press(more);
      await expect(row.getByRole('menuitem')).toContainText(['Make private', 'Make default channel']);

      // Make it private: asked first, focus on Cancel; Cancel changes nothing.
      await press(row.getByRole('menuitem', { name: 'Make private' }));
      const ask = row.getByRole('group', { name: `Change settings of ${channel}` });
      await expect(ask).toContainText(`Make #${channel} private? Only its current members will see it.`);
      await expect(ask.getByRole('button', { name: 'Cancel' })).toBeFocused();
      await expectFits(page, `${size}: the private confirmation in its row`);
      await press(ask.getByRole('button', { name: 'Cancel' }));
      await expect(ask).toHaveCount(0);
      await expect(more).toBeFocused();
      await expect(row.locator('svg')).toHaveCount(0);

      await press(more);
      await press(row.getByRole('menuitem', { name: 'Make private' }));
      await press(ask.getByRole('button', { name: 'Make private' }));
      await expect(ask).toHaveCount(0);

      // The lock, on the operator's row and live on the member's; the names stay.
      await expect(row.locator('svg')).toHaveCount(1);
      await expect(memberRow.locator('svg')).toHaveCount(1);
      await expect(channelButton(page, channel)).toBeVisible();
      await expect(channelButton(seen.page, channel)).toHaveAccessibleDescription(/private channel/);
      for (const where of [row, memberRow]) {
        const box = await where.boundingBox();
        expect(box?.height ?? 0, `the row is ${String(box?.height)} px tall`).toBeLessThan(64);
      }
      await expectFits(page, `${size}: a private channel's row`);
      await expectFits(seen.page, `${size}: a private channel's row, the member's`);

      // A default channel must be public: Make private is disabled, with why.
      await press(more);
      await expect(row.getByRole('menuitem')).toContainText(['Make public', 'Make default channel']);
      await press(row.getByRole('menuitem', { name: 'Make public' }));
      await expect(ask).toContainText(`Make #${channel} public? Every member will be able to find and join it.`);
      await press(ask.getByRole('button', { name: 'Make public' }));
      await expect(ask).toHaveCount(0);
      await expect(row.locator('svg')).toHaveCount(0);
      await expect(memberRow.locator('svg')).toHaveCount(0);

      await press(more);
      await press(row.getByRole('menuitem', { name: 'Make default channel' }));
      await expect(ask).toHaveCount(0);
      await press(more);
      await expect(row.getByRole('menuitem')).toContainText(['Make private', 'Stop being a default channel']);
      const blocked = row.getByRole('menuitem', { name: 'Make private' });
      await expect(blocked).toHaveAttribute('aria-disabled', 'true');
      await expect(blocked).toHaveAccessibleDescription('A default channel must be public. Stop it being a default channel first.');
      await press(row.getByRole('menuitem', { name: 'Stop being a default channel' }));
      await expect(ask).toHaveCount(0);
      await press(more);
      await expect(row.getByRole('menuitem', { name: 'Make private' })).not.toHaveAttribute('aria-disabled', 'true');
    } finally {
      // As found: public and not a default.
      await settings(world, made.id, { default: false });
      await settings(world, made.id, { visibility: 'public' });
      const left = (await ok(world, world.tokens.operator, 'GET', '/v1/channels', listChannelsResponse)).channels.find((c) => c.id === made.id);
      expect(left?.visibility).toBe('public');
      expect(left?.default ?? false).toBe(false);
    }
  });
}
