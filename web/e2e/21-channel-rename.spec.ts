// Issue #728: an operator renames a channel in the browser. The page shows the
// new name and address without reloading, and a reload lands on it. A second
// page open on the channel moves too, through its events socket, and a page
// opened later on the old address lands on the fallback channel. A page in
// the channel's voice stays in voice, its presence as it was, and what it asks
// of conchd next (a transmit report, a session) names the new name. A member
// is offered no control, and the server refuses the member anyway.
//
// The channel renamed in the shared world is one this spec makes, the
// operator's alone, so no other spec's channels change name; the voice half
// runs in a voice world of its own (voice-world.ts), whose channel it renames.
import type { Browser, Page } from '@playwright/test';
import { createChannelResponse, listChannelsResponse, renameChannelResponseV1, type CreateChannelRequest, type RenameChannelRequestV1 } from '../src/schema/channel';
import { errorV0 } from '../src/schema/error';
import { post } from './api';
import { channelButton, expect, messages, signIn, statusLine, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type Member, type VoiceWorld } from './voice-world';
import { call, ok, type World } from './world';

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });
const panel = (page: Page) => page.getByRole('region', { name: 'Members list' });
const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

// The channel a page lands on when nothing it chose names one it can see
// (issue #556): #general when listed, else the first.
function fallback(names: readonly string[]): string | undefined {
  return names.includes('general') ? 'general' : names[0];
}

// operatorPage signs a new person in as the world's operator, on the address
// given, past the operator warning.
async function operatorPage(meet: () => Promise<{ page: Page }>, world: World, hash = ''): Promise<Page> {
  const { page } = await meet();
  await page.goto(world.appUrl + hash);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
  return page;
}

// sockets lists the path of every socket a page opens, from now on: never
// the address, which holds a ticket.
function sockets(page: Page): string[] {
  const opened: string[] = [];
  page.on('websocket', (ws) => {
    opened.push(new URL(ws.url()).pathname);
  });
  return opened;
}

// requests lists every request a page makes to conchd, as method and path.
function requests(page: Page): string[] {
  const seen: string[] = [];
  page.on('request', (req) => {
    const url = new URL(req.url());
    if (url.pathname.startsWith('/v')) seen.push(`${req.method()} ${url.pathname}`);
  });
  return seen;
}

// showsOpen checks that a page has the channel open by that name: its button
// is the current one, its heading and composer name it, and so does the
// address.
async function showsOpen(page: Page, name: string): Promise<void> {
  await expect(channelButton(page, name)).toHaveAttribute('aria-current', 'true');
  await expect(page.getByRole('heading', { name, exact: true })).toBeVisible();
  await expect(page.getByLabel(`Message #${name}`)).toBeVisible();
  await expect.poll(() => page.evaluate(() => window.location.hash)).toBe(`#/c/${encodeURIComponent(name)}`);
}

// The test does not sign out: that would revoke the world's operator
// credential, which later specs use.
test('C728 an operator renames the open channel; a second page moves too; a reload and a later page land where they should', async ({ meet, world }) => {
  const from = `c728-${String(Date.now())}`;
  const to = `${from}-renamed`;
  const made: CreateChannelRequest = { name: from };
  const channel = (await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, made)).channel;

  const a = await operatorPage(meet, world);
  const b = await operatorPage(meet, world);
  for (const page of [a, b]) {
    await channelButton(page, from).click();
    await showsOpen(page, from);
    // The events socket is live: the panel has its roster.
    await expect(panel(page).getByRole('heading', { name: 'People' })).toBeVisible();
    // The message socket is open: the composer takes a message. Until then
    // its first open could still come, and be counted below as one the
    // rename caused (issue #802).
    await expect(page.getByLabel(`Message #${from}`)).toBeEnabled();
  }
  const opened = { a: sockets(a), b: sockets(b) };
  const asked = requests(a);
  // Set on the page itself: a reload would lose it.
  for (const page of [a, b]) await page.evaluate(() => Object.assign(window, { c728: 'not reloaded' }));

  // A renames from the channel list.
  await nav(a).getByRole('button', { name: `Actions for ${from}` }).click();
  await nav(a).getByRole('menuitem', { name: 'Rename channel…' }).click();
  const form = a.getByRole('form', { name: 'Rename channel' });
  await expect(form.getByLabel('New name')).toHaveValue(from);
  await form.getByLabel('New name').fill(to);
  await form.getByRole('button', { name: 'Rename channel' }).click();
  await expect(form).toHaveCount(0);
  await showsOpen(a, to);
  await expect(channelButton(a, from)).toHaveCount(0);
  expect(asked.filter((r) => r.startsWith('PUT') && r.endsWith('/name'))).toEqual([`PUT /v1/channels/${String(channel.id)}/name`]);

  // B was told by its events socket, and moved without doing anything.
  await showsOpen(b, to);
  await expect(channelButton(b, from)).toHaveCount(0);

  // Neither page reloaded or opened a socket again; both still follow the
  // channel live, under its new name, and A asks nothing by the old one.
  for (const page of [a, b]) expect(await page.evaluate(() => (window as unknown as { c728?: string }).c728)).toBe('not reloaded');
  expect(opened, 'sockets opened after the rename').toEqual({ a: [], b: [] });
  const said = `after the rename ${String(Date.now())}`;
  await post(world, world.tokens.operator, to, said);
  for (const page of [a, b]) await expect(messages(page).getByText(said)).toBeVisible();
  const renameAt = asked.findIndex((r) => r.startsWith('PUT') && r.endsWith('/name'));
  expect(asked.slice(renameAt).filter((r) => r.includes(`/${from}/`) || r.endsWith(`/${from}`))).toEqual([]);

  // The server holds the new name, and the old one is no channel's.
  const list = (await ok(world, world.tokens.operator, 'GET', '/v1/channels', listChannelsResponse)).channels;
  expect(list.filter((c) => c.id === channel.id).map((c) => c.name)).toEqual([to]);
  expect((await call(world, world.tokens.operator, 'GET', `/v1/channels/${encodeURIComponent(from)}/members`)).status).toBe(404);

  // A reload lands on the new name.
  await a.reload();
  await expect(statusLine(a)).toContainText('Connected');
  await showsOpen(a, to);

  // A page opened later on the old address lands where a link to a channel
  // it cannot see lands: the fallback.
  const c = await operatorPage(meet, world, `#/c/${encodeURIComponent(from)}`);
  const landing = fallback(list.map((ch) => ch.name));
  expect(landing).toBeDefined();
  await showsOpen(c, landing ?? '');
  await expect(channelButton(c, from)).toHaveCount(0);
  await expect(channelButton(c, to)).toBeVisible();
});

test('C728 a rename refused because the channel was renamed meanwhile says so, moves the page and keeps the name typed', async ({ meet, world }) => {
  const from = `c728s-${String(Date.now())}`;
  const elsewhere = `${from}-elsewhere`;
  const channel = (await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, { name: from })).channel;
  // This page's events socket reaches nothing (a stand-in that says nothing),
  // as for a page whose socket was down when the rename went out: it learns
  // of the rename only when the server refuses its own.
  const { page } = await meet();
  await page.routeWebSocket(/\/v1\/events\/ws/, () => undefined);
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
  await channelButton(page, from).click();
  await showsOpen(page, from);
  await nav(page).getByRole('button', { name: `Actions for ${from}` }).click();
  await nav(page).getByRole('menuitem', { name: 'Rename channel…' }).click();
  const form = page.getByRole('form', { name: 'Rename channel' });

  // Another operator renames it first, as a script would.
  const request: RenameChannelRequestV1 = { name: elsewhere, from };
  expect((await ok(world, world.tokens.operator, 'PUT', `/v1/channels/${String(channel.id)}/name`, renameChannelResponseV1, request)).channel.name).toBe(elsewhere);

  await form.getByLabel('New name').fill(`${from}-mine`);
  await form.getByRole('button', { name: 'Rename channel' }).click();
  await expect(nav(page).getByText(`The channel was renamed to #${elsewhere} meanwhile; look again.`)).toBeVisible();
  await expect(form.getByLabel('New name')).toHaveValue(`${from}-mine`);
  await showsOpen(page, elsewhere);
  const now = await ok(world, world.tokens.operator, 'GET', '/v1/channels', listChannelsResponse);
  expect(now.channels.filter((c) => c.id === channel.id).map((c) => c.name)).toEqual([elsewhere]);
});

test('C728 a member is offered no control, and the server refuses the member anyway', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(channelButton(page, 'general')).toHaveAttribute('aria-current', 'true');
  await expect(nav(page).getByRole('button', { name: /^Rename/ })).toHaveCount(0);
  await expect(page.getByRole('form', { name: 'Rename channel' })).toHaveCount(0);

  const general = (await ok(world, world.tokens.browser, 'GET', '/v1/channels', listChannelsResponse)).channels.find((c) => c.name === 'general');
  expect(general).toBeDefined();
  const request: RenameChannelRequestV1 = { name: `c728-member-${String(Date.now())}`, from: 'general' };
  const refused = await call(world, world.tokens.browser, 'PUT', `/v1/channels/${String(general?.id ?? 0)}/name`, request);
  expect(refused.status).toBe(403);
  expect(errorV0(refused.body)).toEqual([]);
  expect((refused.body as { code: string }).code).toBe('forbidden');
  const after = (await ok(world, world.tokens.browser, 'GET', '/v1/channels', listChannelsResponse)).channels.map((c) => c.name);
  expect(after).toContain('general');
});

test.describe('in voice, with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('browser voice needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the browser voice suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  async function person(browser: Browser, who: 'alice' | Member) {
    const context = await browser.newContext();
    const page = await context.newPage();
    const watch = await watchPage(page, vw);
    const opened = sockets(page);
    const asked = requests(page);
    const m = typeof who === 'string' ? vw[who] : who;
    await signIn(page, vw, m.token, m.name);
    await channelButton(page, vw.channel).click();
    return { context, page, watch, opened, asked, member: m };
  }

  test('a page in voice stays in voice, its presence as it was, and asks conchd by the new name from then on', async ({ browser }) => {
    const dave = await vw.addMember('dave');
    const a = await person(browser, 'alice');
    const d = await person(browser, dave);
    try {
      for (const p of [a, d]) {
        await voiceRegion(p.page).getByRole('button', { name: 'Join voice' }).click();
        await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
      }
      const inVoice = voiceRegion(a.page).getByRole('list', { name: 'In voice' });
      await expect(inVoice).toContainText('You');
      await expect(inVoice).toContainText('dave');
      await expect(panel(a.page).getByRole('heading', { name: 'People' })).toBeVisible();
      // Its message socket is open too, so a late first open is not counted
      // as one the rename caused (issue #802).
      await expect(a.page.getByLabel(`Message #${vw.channel}`)).toBeEnabled();
      const roster = await inVoice.textContent();
      const socketsBefore = a.opened.length;
      const sessionsBefore = a.asked.filter((r) => r.endsWith('/voice/session')).length;

      // An operator renames the channel, as a script would.
      const id = (await vw.api(vw.operator, 'GET', '/v1/channels').then((r) => JSON.parse(r.text) as { channels: { id: number; name: string }[] })).channels.find(
        (c) => c.name === vw.channel,
      )?.id;
      expect(id).toBeDefined();
      const to = 'voice-lounge';
      const request: RenameChannelRequestV1 = { name: to, from: vw.channel };
      expect((await vw.api(vw.operator, 'PUT', `/v1/channels/${String(id ?? 0)}/name`, request)).status).toBe(200);

      // The page moves, and voice does not: still connected, the same people,
      // no socket opened again and no new session asked for.
      await expect(channelButton(a.page, to)).toHaveAttribute('aria-current', 'true');
      await expect(voiceRegion(a.page)).toContainText(`Connected in #${to}`);
      await expect(inVoice).toHaveText(roster ?? '');
      expect(a.opened.slice(socketsBefore), 'sockets opened after the rename').toEqual([]);
      expect(a.asked.filter((r) => r.endsWith('/voice/session'))).toHaveLength(sessionsBefore);
      const renamedAt = a.asked.length;

      // A press and a release report under the new name.
      await a.page.getByRole('heading', { name: 'Conch' }).click();
      await a.page.keyboard.down('Backquote');
      await expect(voiceRegion(a.page)).toContainText('Transmitting');
      await a.page.keyboard.up('Backquote');
      await expect(voiceRegion(a.page)).toContainText('Not transmitting');
      await expect.poll(() => a.asked.slice(renamedAt).filter((r) => r.endsWith('/voice/transmit'))).toEqual([
        `POST /v1/channels/${to}/voice/transmit`,
        `POST /v1/channels/${to}/voice/transmit`,
      ]);

      // The room renewed (another holder disabled): the page asks for its
      // next session by the new name and is back in voice by itself.
      await vw.disable(dave);
      await expect.poll(() => a.asked.slice(renamedAt).filter((r) => r.endsWith('/voice/session')), { timeout: 30_000 }).toEqual([`POST /v1/channels/${to}/voice/session`]);
      await expect(voiceRegion(a.page)).toContainText(`Connected in #${to}`, { timeout: 30_000 });
      expect(a.asked.slice(renamedAt).filter((r) => r.includes(`/${vw.channel}/`))).toEqual([]);
      await a.watch.settled();
    } finally {
      await a.context.close();
      await d.context.close();
    }
  });
});
