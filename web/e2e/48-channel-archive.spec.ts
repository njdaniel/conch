// Issue #844: archive and restore a channel, in a real browser against the real
// server. An owner archives a channel with member-1 in it: member-1's page
// moves it to the Archived group with no reload, the composer is gone, the
// history reads, and a post through the API answers 409 channel_archived.
// Restore brings it back. A second run has member-1 in voice, and archiving
// ends it. Each at 1440×900 and at 390×844.
//
// Each test makes its own channel and puts it back (restored) in a `finally`:
// no channel of the shared community is touched, and an archived channel is
// never left behind (its name would stay taken).
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { createChannelResponse, listChannelsResponse, type CreateChannelRequest } from '../src/schema/channel';
import { channelSettingsResponseV1, type ChannelSettingsRequestV1 } from '../src/schema/generated';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { closeAll, channelButton, credentialField, expect, expectFits, openChannel, showView, signIn, statusLine, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { call, loadWorld, ok, type World } from './world';

const archivedLine = 'This channel is archived.';
const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });
const archivedGroup = (page: Page) => nav(page).getByRole('list', { name: 'Archived channels' });
const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

function channelRow(page: Page, name: string): Locator {
  return nav(page).locator('li').filter({ has: page.getByRole('button', { name, exact: true }) });
}

const sizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }, true],
];

async function archive(world: World, id: number, archived: boolean): Promise<void> {
  const body: ChannelSettingsRequestV1 = { archived };
  await ok(world, world.tokens.operator, 'PUT', `/v1/channels/${String(id)}/settings`, channelSettingsResponseV1, body);
}

async function operatorPage(meet: (options?: BrowserContextOptions) => Promise<{ page: Page }>, world: World, options: BrowserContextOptions): Promise<Page> {
  const { page } = await meet(options);
  await page.goto(world.appUrl);
  await (await credentialField(page)).fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
  return page;
}

for (const [size, options, phone] of sizes) {
  test(`A844 at ${size} an owner archives a channel and restores it; member-1 sees it move live, read-only, and back`, async ({ meet, secrets, world }) => {
    const suffix = Date.now().toString(36).slice(-5);
    const channel = `a844-${suffix}${phone ? 'p' : 'w'}`;
    const name = `member-1-${suffix}${phone ? 'p' : 'w'}`;
    const request: CreateChannelRequest = { name: channel };
    const made = (await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request)).channel;
    try {
      const principal: CreatePrincipalRequest = { kind: 'human', name };
      const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
      const credential: CreateCredentialRequestV1 = { label: name };
      const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
      secrets.push(token);
      const added = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`, {});
      expect(added.status).toBeLessThan(300);
      const history = `history-${suffix}`;
      const posted = await call(world, world.tokens.operator, 'POST', `/v2/channels/${encodeURIComponent(channel)}/messages`, { body: history });
      expect(posted.status).toBe(201);

      const press = async (target: Locator) => (phone ? target.tap() : target.click());

      // member-1 is in the channel, which is open, with its composer.
      const seen = await meet(options);
      await signIn(seen.page, world, token, name);
      await openChannel(seen.page, channel);
      const composer = seen.page.getByLabel(`Message #${channel}`);
      await expect(composer).toBeVisible();
      await expect(seen.page.getByText(history)).toBeVisible();

      // The owner's page.
      const page = await operatorPage(meet, world, options);
      if (phone) await showView(page, 'Channels');
      const row = channelRow(page, channel);
      const more = row.getByRole('button', { name: `Actions for ${channel}` });
      await expect(more).toBeVisible();
      await expect(page.getByRole('button', { name: /^Archived/ })).toHaveCount(0);

      // Archive asks first, says what it does, and Cancel changes nothing.
      await press(more);
      await press(row.getByRole('menuitem', { name: 'Archive channel…' }));
      const ask = row.getByRole('group', { name: `Change settings of ${channel}` });
      await expect(ask).toContainText('Its history stays, and nothing new can be posted in it');
      await expect(ask.getByRole('button', { name: 'Cancel' })).toBeFocused();
      await expectFits(page, `${size}: the archive confirmation in its row`);
      await press(ask.getByRole('button', { name: 'Cancel' }));
      await expect(ask).toHaveCount(0);
      await expect(more).toBeFocused();
      await expect(composer).toBeVisible();

      await press(more);
      await press(row.getByRole('menuitem', { name: 'Archive channel…' }));
      await press(ask.getByRole('button', { name: 'Archive channel' }));
      await expect(ask).toHaveCount(0);

      // member-1's page, with no reload: the composer is replaced, the history reads.
      const log = seen.page.getByRole('log', { name: 'Messages' });
      await expect(seen.page.getByText(archivedLine, { exact: true })).toBeVisible();
      await expect(composer).toHaveCount(0);
      await expect(log).toContainText(history);
      await expectFits(seen.page, `${size}: an archived channel, read-only`);
      // Voice has nothing to join in it.
      await expect(voiceRegion(seen.page).getByRole('button', { name: /^Join voice/ })).toHaveCount(0);
      if (phone) await showView(seen.page, 'Channels');
      const group = archivedGroup(seen.page);
      await expect(group.getByRole('button', { name: channel, exact: true })).toBeVisible();
      await expect(nav(seen.page).locator('ul.channel-list').first().getByRole('button', { name: channel, exact: true })).toHaveCount(0);
      // The owner's page moved it too (collapsed: the owner is not in it).
      await expect(page.getByRole('button', { name: /^Archived \(\d+\)$/ })).toBeVisible();

      // The API refuses anything new in it, and history still reads.
      const refused = await call(world, token, 'POST', `/v2/channels/${encodeURIComponent(channel)}/messages`, { body: 'nope' });
      expect(refused.status).toBe(409);
      expect((refused.body as { code?: string }).code).toBe('channel_archived');
      const read = await call(world, token, 'GET', `/v2/channels/${encodeURIComponent(channel)}/messages`);
      expect(read.status).toBe(200);
      expect(JSON.stringify(read.body)).toContain(history);

      // Restore, from the Archived group on the owner's page.
      await press(page.getByRole('button', { name: /^Archived \(\d+\)$/ }));
      const gone = archivedGroup(page).locator('li').filter({ has: page.getByRole('button', { name: channel, exact: true }) });
      const restoreMore = gone.getByRole('button', { name: `Actions for ${channel}` });
      await press(restoreMore);
      await expect(gone.getByRole('menuitem', { name: 'Restore channel' })).toBeVisible();
      await expect(gone.getByRole('menuitem', { name: /^(Archive channel|Make |Invite people)/ })).toHaveCount(0);
      await press(gone.getByRole('menuitem', { name: 'Restore channel' }));
      await expect(archivedGroup(seen.page)).toHaveCount(0);
      if (phone) await showView(seen.page, 'Chat');
      await expect(composer).toBeVisible();
      await expect(seen.page.getByText(archivedLine, { exact: true })).toHaveCount(0);
      await expect(log).toContainText(history);
      await expect(channelButton(page, channel)).toBeVisible();
      const again = await call(world, token, 'POST', `/v2/channels/${encodeURIComponent(channel)}/messages`, { body: 'back' });
      expect(again.status).toBe(201);
      await expect(log).toContainText('back');
    } finally {
      // As found: live. Archived or not, no channel is left archived.
      await archive(world, made.id, false);
      const left = (await ok(world, world.tokens.operator, 'GET', '/v1/channels', listChannelsResponse)).channels.find((c) => c.id === made.id);
      expect(left?.archived_at).toBeUndefined();
    }
  });
}

test('A844 a member without manage_channels is offered neither Archive nor Restore', async ({ meet, secrets, world }) => {
  const suffix = Date.now().toString(36).slice(-5);
  const channel = `a844-perm-${suffix}`;
  const name = `member-2-${suffix}`;
  const request: CreateChannelRequest = { name: channel };
  const made = (await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request)).channel;
  try {
    const principal: CreatePrincipalRequest = { kind: 'human', name };
    const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
    const credential: CreateCredentialRequestV1 = { label: name };
    const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
    secrets.push(token);
    await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`, {});
    const { page } = await meet({ viewport: { width: 1440, height: 900 } });
    await signIn(page, world, token, name);
    const row = channelRow(page, channel);
    await expect(row).toBeVisible();
    // The ⋯ of a member holds Leave and nothing that archives.
    await row.getByRole('button', { name: `Actions for ${channel}` }).click();
    await expect(row.getByRole('menuitem', { name: /Archive|Restore/ })).toHaveCount(0);
    // And the server says no to the request itself.
    const refused = await call(world, token, 'PUT', `/v1/channels/${String(made.id)}/settings`, { archived: true });
    expect(refused.status).toBe(403);
  } finally {
    await archive(world, made.id, false);
  }
});

test.describe('with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('browser voice needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the browser voice suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  for (const [size, options, phone] of sizes) {
    test(`A844 at ${size} archiving a channel ends member-1's voice in it, and restore offers voice again`, async ({ browser }) => {
      const context = await browser.newContext(options);
      try {
        const page = await context.newPage();
        const member = vw.alice;
        await signIn(page, vw, member.token, member.name);
        await openChannel(page, vw.channel);
        const voice = voiceRegion(page);
        if (phone) await showView(page, 'Voice');
        await voice.getByRole('button', { name: 'Join voice' }).click();
        await expect(voice).toContainText(/Connected in #voice-room/, { timeout: 30_000 });

        const listed = await vw.api(vw.operator, 'GET', '/v1/channels');
        const id = (JSON.parse(listed.text) as { channels: { id: number; name: string }[] }).channels.find((c) => c.name === vw.channel)?.id;
        expect(id).toBeDefined();
        try {
          const put = await vw.api(vw.operator, 'PUT', `/v1/channels/${String(id)}/settings`, { archived: true });
          expect(put.status, put.text).toBe(200);

          // Voice is over before the answer; the page follows, and offers no way back in.
          await expect(voice).not.toContainText(/Connected in #voice-room/, { timeout: 30_000 });
          await expect(voice).toContainText(archivedLine);
          await expect(voice.getByRole('button', { name: /^Join voice/ })).toHaveCount(0);
          await expect(voice.getByRole('button', { name: /^Watch only/ })).toHaveCount(0);
          const rejoin = await vw.api(member.token, 'POST', `/v1/channels/${vw.channel}/voice/session`, {});
          expect(rejoin.status).toBe(409);
          expect((JSON.parse(rejoin.text) as { code?: string }).code).toBe('channel_archived');
          await expectFits(page, `${size}: voice in an archived channel`);
        } finally {
          const back = await vw.api(vw.operator, 'PUT', `/v1/channels/${String(id)}/settings`, { archived: false });
          expect(back.status, back.text).toBe(200);
        }

        // Restore starts nothing; Join voice is offered again.
        await expect(voice.getByRole('button', { name: 'Join voice' })).toBeVisible();
        await expect(voice).not.toContainText(/Connected in #voice-room/);
      } finally {
        await closeAll(context);
      }
    });
  }
});
