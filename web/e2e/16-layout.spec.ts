// Issue #536: the lobby's layout at a phone, a tablet and a desktop size, and
// at 200% zoom (a 1440×900 screen zoomed is a 720×450 window). At each, with a
// long channel name, a long person's name, long messages and a larger roster:
// nothing widens the page, the composer and the voice strip are on screen and
// not covered, and the members and settings panels open, fit, take focus and
// give it back on Escape. Media continuity through these panels is in
// 12-screen-share, which has LiveKit.
import type { BrowserContextOptions, Locator, Page, WebSocketRoute } from '@playwright/test';
import { voicePresenceV2, type VoicePresenceV2 } from '../src/schema/voice';
import { channelButton, expect, expectFits, mainNav, messages, openChannel, phoneSizes, showView, signIn, test } from './fixtures';
import { call, ok, type World } from './world';

const sizes: readonly (readonly [string, BrowserContextOptions])[] = [
  ...phoneSizes,
  ['768×1024', { viewport: { width: 768, height: 1024 }, hasTouch: true }],
  ['1440×900', { viewport: { width: 1440, height: 900 } }],
  ['1440×900 at 200%', { viewport: { width: 720, height: 450 }, deviceScaleFactor: 2 }],
];

async function must(world: World, token: string, method: 'POST' | 'PUT', path: string, body?: unknown): Promise<unknown> {
  const reply = await call(world, token, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
  return reply.body;
}

interface Member {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

async function member(world: World, channel: string, name: string): Promise<Member> {
  const op = world.tokens.operator;
  const made = (await must(world, op, 'POST', '/v0/principals', { kind: 'human', name })) as { principal: { id: number } };
  const cred = (await must(world, op, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, { label: name })) as { token: string };
  await must(world, op, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(made.principal.id)}`);
  return { id: made.principal.id, name, token: cred.token };
}

// expectUncovered checks that an element, scrolled into view, lies inside the
// window and is what a tap at its middle would reach: nothing is drawn over it.
async function expectUncovered(page: Page, element: Locator, what: string): Promise<void> {
  await element.scrollIntoViewIfNeeded();
  const box = await element.boundingBox();
  const screen = page.viewportSize();
  if (box === null || screen === null) throw new Error(`${what} has no box on the screen`);
  const where = `${what} is at x ${String(box.x)}, y ${String(box.y)}, ${String(box.width)}×${String(box.height)} on a ${String(screen.width)}×${String(screen.height)} window`;
  expect(box.x, where).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width, where).toBeLessThanOrEqual(screen.width + 0.5);
  expect(box.y + Math.min(box.height, 24), where).toBeLessThanOrEqual(screen.height);
  const reached = await element.evaluate((el) => {
    const r = el.getBoundingClientRect();
    const at = document.elementFromPoint(r.left + r.width / 2, r.top + Math.min(r.height / 2, 12));
    return at !== null && (at === el || el.contains(at));
  });
  expect(reached, `${what} is covered by something else`).toBe(true);
}

test('P536 at every size the lobby fits, its composer and voice strip are uncovered, and its panels take and give back focus', async ({ meet, secrets, world }) => {
  const tag = `p536-${String(Date.now())}`;
  const channel = `${tag}-a-channel-with-a-rather-long-name`;
  await must(world, world.tokens.operator, 'POST', '/v0/channels', { name: channel });
  const me = await member(world, channel, `${tag}-someone-whose-display-name-is-long`);
  secrets.push(me.token);
  for (let i = 0; i < 12; i++) secrets.push((await member(world, channel, `${tag}-roster-${String(i)}`)).token);
  const longWord = `${tag}-${'w'.repeat(160)}`;
  for (const body of [longWord, `${tag} ${'a line that goes on for a while, '.repeat(12)}and ends`]) {
    await must(world, me.token, 'POST', `/v2/channels/${encodeURIComponent(channel)}/messages`, { body });
  }

  for (const [size, options] of sizes) {
    const { page } = await meet(options);
    await signIn(page, world, me.token, me.name);
    await openChannel(page, channel);
    await expect(messages(page).getByText(longWord)).toBeVisible();
    await expectFits(page, `${size}: the lobby`);
    await expectUncovered(page, page.getByLabel(`Message #${channel}`), `${size}: the composer`);
    if ((options.viewport?.width ?? 1000) < 700) {
      // A phone: the voice section is the Voice view's (not in voice, there
      // is no strip under Chat), and the navigation is on the screen's
      // bottom edge, uncovered, under every view.
      await expectUncovered(page, mainNav(page), `${size}: the navigation`);
      await showView(page, 'Voice');
      await expectUncovered(page, page.getByRole('region', { name: 'Voice' }), `${size}: the voice section`);
      await expectFits(page, `${size}: the Voice view`);
      await showView(page, 'Channels');
      await expectUncovered(page, channelButton(page, channel), `${size}: the long channel's row`);
      await expectFits(page, `${size}: the Channels view with a long channel name`);
      await showView(page, 'Chat');
    } else {
      await expectUncovered(page, page.getByRole('region', { name: 'Voice' }), `${size}: the voice strip`);
    }

    // Members: a column on a wide window, a panel over the page otherwise.
    const members = page.getByRole('button', { name: 'Members' });
    const list = page.getByRole('region', { name: 'Members list' });
    if ((await members.getAttribute('aria-expanded')) === 'true') {
      await expect(list).toContainText(`${tag}-roster-11`);
    } else {
      await members.click();
      await expect(list).toBeFocused();
      await expect(list).toContainText(`${tag}-roster-11`);
      await expectFits(page, `${size}: the members panel`);
      await page.keyboard.press('Escape');
      await expect(list).toBeHidden();
      await expect(members).toBeFocused();
    }

    // Settings: a panel over the page at every size.
    const settings = page.getByRole('button', { name: 'Settings' });
    await settings.click();
    await expect(page.getByRole('heading', { name: 'Settings' })).toBeFocused();
    await expect(page.getByRole('dialog', { name: 'Settings' })).toContainText(me.name);
    await expectFits(page, `${size}: the settings panel`);
    await page.keyboard.press('Escape');
    await expect(page.getByRole('dialog', { name: 'Settings' })).toBeHidden();
    await expect(settings).toBeFocused();
  }
});

// ---------------------------------------------------------------- issue #587

// The shared server has no LiveKit, so nobody can be in voice for real. The
// strip is served a presence document instead, on its socket (and on the one
// read it makes after the server closes one): the server's own document for
// the channel, with voice up and the given people in its room. It exists only
// in this test; the product shows only who the server reports. It is v2
// presence, which the page asks for and reads (voice-multi-share.md §7.1).
interface Presence {
  // show sends a new document on every socket the page has open.
  show: (who: readonly number[], sharing?: number) => void;
}

async function servePresence(page: Page, world: World, token: string, channel: string, who: readonly number[]): Promise<Presence> {
  const real = await ok(world, token, 'GET', `/v1/channels/${encodeURIComponent(channel)}/voice?schema=conch.voice_presence.v2`, voicePresenceV2);
  const doc = (people: readonly number[], sharing?: number): string => {
    const presence: VoicePresenceV2 = {
      ...real,
      configured: true,
      available: true,
      rooms: [
        {
          participants: people.map((id) => ({ principal_id: id, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z', ...(id === sharing ? { sharing: true, share_started_at: '2026-10-18T00:00:00Z' } : {}) })),
        },
      ],
    };
    expect(voicePresenceV2(presence), 'the faked presence document').toEqual([]);
    return JSON.stringify(presence);
  };
  let current = doc(who);
  const sockets: WebSocketRoute[] = [];
  await page.routeWebSocket(/\/v1\/voice\/ws/, (ws) => {
    sockets.push(ws);
    ws.send(current);
  });
  await page.route(/\/v1\/channels\/[^/]+\/voice$/, async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    return route.fulfill({ status: 200, contentType: 'application/json', body: current });
  });
  return {
    show: (people, sharing) => {
      current = doc(people, sharing);
      for (const ws of sockets) ws.send(current);
    },
  };
}

type Box = NonNullable<Awaited<ReturnType<Locator['boundingBox']>>>;

async function boxOf(element: Locator, what: string): Promise<Box> {
  const box = await element.boundingBox();
  if (box === null) throw new Error(`${what} has no box on the screen`);
  return box;
}

// expectInside checks that a chip is drawn whole: inside the strip and inside
// the window.
async function expectInside(page: Page, chip: Locator, strip: Box, what: string): Promise<void> {
  const box = await boxOf(chip, what);
  const screen = page.viewportSize();
  if (screen === null) throw new Error('no window size');
  const where = `${what} is at x ${String(box.x)}, y ${String(box.y)}, ${String(box.width)}×${String(box.height)}`;
  expect(box.x, where).toBeGreaterThanOrEqual(Math.max(strip.x, 0) - 0.5);
  expect(box.y, where).toBeGreaterThanOrEqual(Math.max(strip.y, 0) - 0.5);
  expect(box.x + box.width, where).toBeLessThanOrEqual(Math.min(strip.x + strip.width, screen.width) + 0.5);
  expect(box.y + box.height, where).toBeLessThanOrEqual(Math.min(strip.y + strip.height, screen.height) + 0.5);
}

// The sizes of issue #587's checks, and whether each is a phone.
const rosterSizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['1280×720', { viewport: { width: 1280, height: 720 } }, false],
  ['768×1024', { viewport: { width: 768, height: 1024 }, hasTouch: true }, false],
  ...phoneSizes.map(([size, options]) => [size, options, true] as const),
];

test("P587 the voice roster shows everyone and how many, and the strip's controls stay put as it grows", async ({ meet, secrets, world }) => {
  const tag = `p587-${String(Date.now())}`;
  const channel = `${tag}-voice`;
  await must(world, world.tokens.operator, 'POST', '/v0/channels', { name: channel });
  const me = await member(world, channel, `${tag}-me`);
  secrets.push(me.token);
  const others: Member[] = [];
  for (let i = 0; i < 7; i++) others.push(await member(world, channel, `${tag}-in-voice-${String(i)}`));
  for (const o of others) secrets.push(o.token);
  const everyone = [me.id, ...others.map((o) => o.id)];
  // The roster is in principal order, and the viewer is named as themselves.
  const names = [me.name, ...others.map((o) => o.name)];

  for (const [size, options, phone] of rosterSizes) {
    const { page } = await meet(options);
    const presence = await servePresence(page, world, me.token, channel, [me.id]);
    await signIn(page, world, me.token, me.name);
    await openChannel(page, channel);
    // On a phone the voice section is the Voice view's.
    if (phone) await showView(page, 'Voice');
    const strip = page.getByRole('region', { name: 'Voice' });
    const roster = strip.getByRole('list', { name: /in voice$/ });
    await expect(strip.getByRole('heading', { name: '1 in voice' })).toBeVisible();
    await expect(roster).toHaveAccessibleName('1 in voice');
    // Not in voice, the controls are Join voice and Watch only, in the row
    // and at its end, where Leave voice is once joined.
    const control = strip.getByRole('button', { name: /^Watch only/ });
    await expect(control).toBeEnabled();
    const alone = await boxOf(control, `${size}: Watch only`);

    presence.show(everyone);
    await expect(strip.getByRole('heading', { name: '8 in voice' })).toBeVisible();
    await expect(roster).toHaveAccessibleName('8 in voice');
    for (const name of names) await expect(roster).toContainText(name);
    // Every chip, those a phone hides too (which are not list items while
    // hidden).
    const chips = roster.locator('li');
    await expect(chips).toHaveCount(8);

    if (phone) {
      // The Voice view shows everyone, by name (issue #929): the compact
      // strip's three chips and "+5 more" are for the strip under Chat and
      // Channels, once joined.
      await expect(strip.getByRole('button', { name: /more$/ })).toBeHidden();
      for (let i = 0; i < 8; i++) {
        await expect(chips.nth(i)).toBeVisible();
        await expect(chips.nth(i).locator('.who')).toBeVisible();
      }
    } else {
      await expect(strip.getByRole('button', { name: /more$/ })).toBeHidden();
      // With everyone shown, the controls are where they were with one.
      const crowd = await boxOf(control, `${size}: Watch only`);
      expect(Math.abs(crowd.x - alone.x), `${size}: Watch only moved sideways`).toBeLessThanOrEqual(1);
      expect(Math.abs(crowd.y - alone.y), `${size}: Watch only moved up or down`).toBeLessThanOrEqual(1);
    }

    // On a phone the dock is capped at part of the screen and scrolls within
    // itself, so each chip is scrolled to before it is measured.
    for (let i = 0; i < 8; i++) {
      const chip = chips.nth(i);
      await expect(chip).toBeVisible();
      await chip.scrollIntoViewIfNeeded();
      await expectInside(page, chip, await boxOf(strip, `${size}: the voice strip`), `${size}: ${names[i] ?? ''}'s chip`);
    }
    await expectFits(page, `${size}: the lobby with eight in voice`);
    if (phone) await showView(page, 'Chat');
    await expectUncovered(page, page.getByLabel(`Message #${channel}`), `${size}: the composer`);
  }
});


// ---------------------------------------------------------------- issue #929

// The phone layout, at the four phone sizes: Chat, Channels and Voice, one at a
// time, under a bottom navigation. Nothing here has LiveKit, so nobody can join:
// the strip is served a presence document (as for #587) and the test asserts
// that no view, channel or panel asked for a voice session. A call carried
// through the views is in 12-screen-share and 10-voice, which have LiveKit.
for (const [size, options] of phoneSizes) {
  test(`P929 at ${size} the views fit, keep drafts and the log's place, join nothing, and survive a rotation`, async ({ meet, secrets, world }) => {
    const tag = `p929-${size.slice(0, 3)}-${String(Date.now())}`;
    const first = `${tag}-a-channel-with-a-rather-long-name`;
    const second = `${tag}-second`;
    const op = world.tokens.operator;
    await must(world, op, 'POST', '/v0/channels', { name: first });
    await must(world, op, 'POST', '/v0/channels', { name: second });
    const me = await member(world, first, `${tag}-someone-whose-display-name-is-long`);
    secrets.push(me.token);
    await must(world, op, 'PUT', `/v1/channels/${encodeURIComponent(second)}/members/${String(me.id)}`);
    const others: Member[] = [];
    for (let i = 0; i < 7; i++) others.push(await member(world, first, `${tag}-in-voice-${String(i)}`));
    for (const o of others) secrets.push(o.token);
    for (let i = 0; i < 40; i++) {
      await must(world, me.token, 'POST', `/v2/channels/${encodeURIComponent(first)}/messages`, { body: `${tag} line ${String(i + 1)} ${'and on it goes '.repeat(i % 5)}` });
    }

    const { page } = await meet(options);
    let sessions = 0;
    page.on('request', (r) => {
      if (r.method() === 'POST' && r.url().endsWith('/voice/session')) sessions++;
    });
    await servePresence(page, world, me.token, first, [me.id, ...others.map((o) => o.id)]);
    await signIn(page, world, me.token, me.name);
    await openChannel(page, first);
    const nav = mainNav(page);
    const log = messages(page);
    const composer = page.getByLabel(`Message #${first}`);

    // Three labelled buttons, finger-sized, the current one marked.
    await expect(nav.getByRole('button')).toHaveText(['Chat', 'Channels', 'Voice']);
    for (const name of ['Chat', 'Channels', 'Voice']) {
      const box = await boxOf(nav.getByRole('button', { name, exact: true }), `${size}: ${name}`);
      expect(box.width, `${size}: ${name} is ${String(box.width)}×${String(box.height)}`).toBeGreaterThanOrEqual(44);
      expect(box.height, `${size}: ${name} is ${String(box.width)}×${String(box.height)}`).toBeGreaterThanOrEqual(44);
    }
    await expectUncovered(page, nav, `${size}: the navigation`);

    // A draft, and the log scrolled up from its end.
    await composer.fill(`${tag} half a thought`);
    await log.evaluate((el) => {
      el.scrollTop = el.scrollHeight - el.clientHeight - 300;
    });
    const placed = await log.evaluate((el) => el.scrollTop);
    expect(placed, `${size}: the log is scrolled up from its end`).toBeGreaterThan(0);

    for (const view of ['Channels', 'Voice', 'Chat'] as const) {
      await showView(page, view);
      await expectFits(page, `${size}: the ${view} view`);
      if (view === 'Voice') {
        const strip = page.getByRole('region', { name: 'Voice' });
        await expect(strip.getByRole('heading', { name: '8 in voice' })).toBeVisible();
        await expect(composer, `${size}: the composer is not drawn in the Voice view`).toBeHidden();
        await expectUncovered(page, strip.getByRole('button', { name: /^Watch only/ }), `${size}: Watch only`);
        await expectUncovered(page, nav.getByRole('button', { name: 'Chat', exact: true }), `${size}: the way back to Chat`);
      }
    }
    await expect(composer).toHaveValue(`${tag} half a thought`);
    expect(Math.abs((await log.evaluate((el) => el.scrollTop)) - placed), `${size}: the log kept its place through Channels and Voice`).toBeLessThanOrEqual(2);

    // Scrolled up by only a little (40 px, more than the 24 px that still
    // counts as the end): not snapped back to the newest message either, by
    // the log being resized behind the Voice view and back.
    await log.evaluate((el) => {
      el.scrollTop = el.scrollHeight - el.clientHeight - 40;
    });
    const near = await log.evaluate((el) => el.scrollTop);
    for (const view of ['Voice', 'Channels', 'Chat'] as const) await showView(page, view);
    await expect(composer).toHaveValue(`${tag} half a thought`);
    expect(Math.abs((await log.evaluate((el) => el.scrollTop)) - near), `${size}: the log kept a place 40 px from its end`).toBeLessThanOrEqual(2);

    // Another channel, chosen in Channels: Chat is back, with the other
    // channel's own (empty) box; this one's draft is there on return.
    await showView(page, 'Channels');
    await expect(channelButton(page, first)).toHaveAttribute('aria-current', 'true');
    await channelButton(page, second).click();
    await expect(mainNav(page).getByRole('button', { name: 'Chat', exact: true })).toHaveAttribute('aria-current', 'page');
    await expect(page.getByRole('heading', { name: second })).toBeFocused();
    await expect(page.getByLabel(`Message #${second}`)).toHaveValue('');
    await page.getByLabel(`Message #${second}`).fill(`${tag} for the second`);
    await openChannel(page, first);
    await expect(composer).toHaveValue(`${tag} half a thought`);

    // Settings from the Voice view, and back: the view and the focus.
    await showView(page, 'Voice');
    const settings = page.getByRole('button', { name: 'Settings' });
    await settings.click();
    await expect(page.getByRole('dialog', { name: 'Settings' })).toBeVisible();
    await expectFits(page, `${size}: Settings over the Voice view`);
    await page.keyboard.press('Escape');
    await expect(settings).toBeFocused();
    await expect(nav.getByRole('button', { name: 'Voice', exact: true })).toHaveAttribute('aria-current', 'page');
    await showView(page, 'Chat');

    // The on-screen keyboard shortens the window. This is a resize, which is
    // not a keyboard: it checks the layout gives the composer and the
    // navigation the room that is left, not what a real keyboard does.
    const full = options.viewport;
    await composer.focus();
    await page.setViewportSize({ width: full.width, height: Math.round(full.height * 0.55) });
    await expectUncovered(page, composer, `${size}: the composer with a short window`);
    await expectUncovered(page, page.getByRole('button', { name: 'Send' }), `${size}: Send with a short window`);
    await expectFits(page, `${size}: a short window`);
    await page.setViewportSize(full);
    await expect(composer).toHaveValue(`${tag} half a thought`);

    // Turned on its side the window is wider than a phone: the tablet layout,
    // with no navigation; turned back, the phone layout and the same page.
    await page.setViewportSize({ width: full.height, height: full.width });
    // (Landscape is 700 px or wider for all but the 320 px phone, whose 640 px
    // is still a phone's width.)
    if (full.height >= 700) await expect(nav).toHaveCount(0);
    else await expect(nav).toBeVisible();
    await expectFits(page, `${size}: on its side`);
    await expectUncovered(page, composer, `${size}: the composer on its side`);
    await page.setViewportSize(full);
    await expect(nav).toBeVisible();
    await expect(composer).toHaveValue(`${tag} half a thought`);
    await expectFits(page, `${size}: turned back`);

    expect(sessions, `${size}: nothing asked for a voice session`).toBe(0);
  });
}

// ---------------------------------------------------------------- issue #1813

// In a short desktop window the page scrolls as a document (issue #536). It
// used to leave an empty band under the composer: the members list made the
// middle row taller than the conversation. Now the conversation sets the row,
// so at every height, with or without voice on the server and with a notice
// bar above the page, the composer sits on the voice dock, and with the page
// scrolled to its end the composer is whole. Joined and watching a share, which
// hold the dock up, are in 12-screen-share, which has LiveKit.
const shortWindows: readonly (readonly [string, BrowserContextOptions])[] = [
  ...[400, 470, 560, 561, 640].map((height): readonly [string, BrowserContextOptions] => [`1215×${String(height)}`, { viewport: { width: 1215, height } }]),
  ['720×450 at 200%', { viewport: { width: 720, height: 450 }, deviceScaleFactor: 2 }],
];

// With voice set up on the server the dock is taller: the strip's roster, its
// buttons and its status (about 130 px against 58 px without).
const dockStates = ['no voice', 'voice set up, nobody joined', 'a notice bar above the page'] as const;

test('P1813 in a short window the composer stays on the voice dock, whole, as the person scrolls', async ({ meet, secrets, world }) => {
  const tag = `p1813-${String(Date.now())}`;
  const channel = `${tag}-c`;
  await must(world, world.tokens.operator, 'POST', '/v0/channels', { name: channel });
  const me = await member(world, channel, `${tag}-me`);
  secrets.push(me.token);
  const others: Member[] = [];
  // A members list longer than any of these windows.
  for (let i = 0; i < 30; i++) others.push(await member(world, channel, `${tag}-roster-${String(i)}`));
  for (const o of others) secrets.push(o.token);
  for (let i = 0; i < 60; i++) {
    await must(world, me.token, 'POST', `/v2/channels/${encodeURIComponent(channel)}/messages`, { body: `${tag} message ${String(i)} ${'words '.repeat(i % 7 === 0 ? 40 : 3)}` });
  }

  for (const state of dockStates) {
    for (const [size, options] of shortWindows) {
      // A window just over 560 px has the full-height layout, whose column
      // clips (and scrolls inside itself) when a tall dock leaves it too little:
      // that is the layout as it was, not this change's (issue #1815).
      if (size.endsWith('×561') && state === 'voice set up, nobody joined') continue;
      const { page } = await meet(options);
      if (state === 'voice set up, nobody joined') await servePresence(page, world, me.token, channel, others.slice(0, 2).map((o) => o.id));
      await signIn(page, world, me.token, me.name);
      await openChannel(page, channel);
      await expect(messages(page).getByText(`${tag} message 59`)).toBeVisible();
      if (state === 'a notice bar above the page') {
        // What the update notice does (UpdateNotice.tsx): a bar above the
        // page, its height in --notice-h on the root.
        await page.evaluate(() => {
          const bar = document.createElement('div');
          bar.textContent = 'A new version is ready';
          bar.style.height = '44px';
          document.body.prepend(bar);
          document.documentElement.style.setProperty('--notice-h', '44px');
        });
      }
      const composer = page.locator('.composer');
      const dock = page.locator('.dock');
      const screen = page.viewportSize();
      if (screen === null) throw new Error('no window size');

      // The composer's bottom edge is on the dock's top edge; and, with the
      // page scrolled to its end, the composer and the dock are whole inside
      // the window.
      const settled = async (step: string): Promise<void> => {
        const where = `${size}, ${state}, ${step}`;
        await expect(async () => {
          const c = await boxOf(composer, `${where}: the composer`);
          const d = await boxOf(dock, `${where}: the dock`);
          expect(Math.abs(c.y + c.height - d.y), `${where}: the composer is ${String(d.y - c.y - c.height)} px above the dock`).toBeLessThanOrEqual(1);
        }).toPass({ timeout: 2000 });
        await page.evaluate(() => {
          window.scrollTo(0, document.documentElement.scrollHeight);
        });
        const c = await boxOf(composer, `${where}: the composer at the end of the page`);
        const d = await boxOf(dock, `${where}: the dock at the end of the page`);
        expect(c.y, `${where}: the composer's top at the end of the page`).toBeGreaterThanOrEqual(-0.5);
        expect(c.y + c.height, `${where}: the composer's bottom at the end of the page`).toBeLessThanOrEqual(d.y + 1);
        expect(d.y + d.height, `${where}: the dock's bottom at the end of the page`).toBeLessThanOrEqual(screen.height + 0.5);
        await page.evaluate(() => {
          window.scrollTo(0, 0);
        });
      };
      const wheel = async (x: number, y: number, dy: number, step: string): Promise<void> => {
        await page.mouse.move(x, y);
        await page.mouse.wheel(0, dy);
        await settled(step);
      };

      await settled('at the start');
      const box = await boxOf(messages(page), `${size}: the log`);
      const mid = { x: box.x + box.width / 2, y: Math.min(box.y + box.height / 2, screen.height - 10) };
      await wheel(mid.x, mid.y, -3000, 'wheel up past the top of the messages');
      await wheel(mid.x, mid.y, 6000, 'wheel down to the end');
      await wheel(mid.x, mid.y, 3000, 'wheel past the end of the messages');
      await wheel(20, screen.height - 20, 2000, 'wheel in the bottom corner');
      await wheel(screen.width - 20, screen.height / 2, 2000, 'wheel over the members column');
      await page.keyboard.press('End');
      await settled('the End key');
    }
  }
});
