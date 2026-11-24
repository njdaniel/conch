// Issue #536: the lobby's layout at a phone, a tablet and a desktop size, and
// at 200% zoom (a 1440×900 screen zoomed is a 720×450 window). At each, with a
// long channel name, a long person's name, long messages and a larger roster:
// nothing widens the page, the composer and the voice strip are on screen and
// not covered, and the members and settings panels open, fit, take focus and
// give it back on Escape. Media continuity through these panels is in
// 12-screen-share, which has LiveKit.
import type { BrowserContextOptions, Locator, Page, WebSocketRoute } from '@playwright/test';
import { voicePresenceV2, type VoicePresenceV2 } from '../src/schema/voice';
import { channelButton, drawDesktopStrip, expect, expectFits, mainNav, messages, openChannel, phoneSizes, showView, signIn, statusLine, test } from './fixtures';
import { runV1 } from '../src/schema/run';
import { attach, openSession, post } from './api';
import { call, ok, type World } from './world';

const sizes: readonly (readonly [string, BrowserContextOptions])[] = [
  ...phoneSizes,
  ['768×1024', { viewport: { width: 768, height: 1024 }, hasTouch: true }],
  ['1440×900', { viewport: { width: 1440, height: 900 } }],
  ['1440×900 at 200%', { viewport: { width: 720, height: 450 }, deviceScaleFactor: 2 }],
];

// Issue #1931: a layout probe using the shipped stylesheet, without LiveKit.
// The conversation's intrinsic height exceeds the column: only a compact
// stage may give its track a zero minimum instead of the min-content floor.
// Both compact stage components use this rule. The former nested :has()
// was discarded by Chromium, leaving the conversation at 300 px throughout.
test('P1931 a compact stage lets the conversation use the remaining track', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await page.goto(world.appUrl);
  await page.evaluate(() => {
    const centre = document.createElement('div');
    centre.id = 'compact-stage-probe';
    centre.className = 'centre';
    centre.style.width = '600px';
    centre.style.height = '200px';
    const stage = document.createElement('div');
    stage.className = 'stage';
    const call = document.createElement('div');
    // call-stage uses display: contents; its child supplies the row's height.
    const row = document.createElement('div');
    row.style.height = '44px';
    row.style.flexShrink = '0';
    call.append(row);
    stage.append(call);
    const conversation = document.createElement('div');
    conversation.style.minHeight = '0';
    const content = document.createElement('div');
    content.style.height = '300px';
    conversation.append(content);
    centre.append(stage, conversation);
    document.body.append(centre);
  });
  const centre = page.locator('#compact-stage-probe');
  await expect(centre).toHaveCSS('display', 'grid');
  const conversation = centre.locator(':scope > div').nth(1);
  const height = () => conversation.evaluate((el) => el.getBoundingClientRect().height);
  for (const component of ['call-view', 'call-stage']) {
    const call = centre.locator('.stage > div');
    await call.evaluate((el, name) => { el.setAttribute('class', name); }, component);
    await expect.poll(height, 'an expanded stage keeps the min-content floor').toBeGreaterThanOrEqual(300);
    await call.evaluate((el) => { el.classList.add('compact'); });
    await expect.poll(height, `${component}: a compact stage leaves room for the conversation`).toBeGreaterThan(0);
    await expect.poll(height, `${component}: the conversation fits below the compact row`).toBeLessThan(200);
  }
});

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
// so at every height, with or without voice on the server, with a notice bar
// above the page and under the desktop app's strip (issue #1980), the composer
// sits on the voice dock, and with the page
// scrolled to its end the composer is whole. Over 560 px the page fills the
// window unless the dock leaves the conversation too little, when it scrolls the
// same way (issue #1815), so 561, 600 and 640 are here too. Joined and watching
// a share, which hold the dock up, are in 12-screen-share, which has LiveKit,
// under the desktop app's strip as well.
//
// Each state has one page per pixel ratio, resized through the heights: every
// page is one more live session on the one credential, which may hold 16
// (MaxLiveSessionsPerCredential), and the test's pages all live until it ends.
// Past the budget the page still draws, but its members panel says "Could not
// start a session", which is not the page these cases mean to measure.
const shortWindows: readonly (readonly [string, { readonly width: number; readonly height: number }, number])[] = [
  ...[400, 470, 560, 561, 600, 640].map((height) => [`1215×${String(height)}`, { width: 1215, height }, 1] as const),
  ['720×450 at 200%', { width: 720, height: 450 }, 2],
];

// With voice set up on the server the dock is taller: the strip's roster, its
// buttons and its status (about 130 px against 58 px without). In the desktop
// app the client is under the app's strip, which takes its own height from the
// page (issue #1980).
const dockStates = ['no voice', 'voice set up, nobody joined', 'a notice bar above the page', "voice set up, under the desktop app's strip"] as const;

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
    // The state's page at each pixel ratio, opened at the first window of it.
    const pages = new Map<number, Page>();
    for (const [size, viewport, deviceScaleFactor] of shortWindows) {
      let page = pages.get(deviceScaleFactor);
      if (page === undefined) {
        ({ page } = await meet({ viewport, deviceScaleFactor }));
        pages.set(deviceScaleFactor, page);
        if (state === 'voice set up, nobody joined' || state === "voice set up, under the desktop app's strip") await servePresence(page, world, me.token, channel, others.slice(0, 2).map((o) => o.id));
        await signIn(page, world, me.token, me.name);
        await openChannel(page, channel);
        await expect(messages(page).getByText(`${tag} message 59`)).toBeVisible();
        if (state === "voice set up, under the desktop app's strip") await drawDesktopStrip(page);
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
      } else {
        await page.setViewportSize(viewport);
        await page.evaluate(() => {
          window.scrollTo(0, 0);
        });
      }
      // What is measured is the signed-in lobby with its members list, not a
      // page that ran out of sessions.
      await expect(statusLine(page)).toContainText('Connected');
      await expect(page.getByText('Could not start a session'), `${size}, ${state}: the members panel started a session`).toHaveCount(0);
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

// ---------------------------------------------------------------- issue #1815

// Over 560 px the page grows past the window only for what a tall dock
// squeezes: the composer and the stage's 300 px (issue #1815). Anything else
// tall in the centre column keeps scrolling inside itself, as in the
// full-height layout. Run detail with a long task and a many-line outcome, at
// 1440×900 and in a window just over 560 px: the page does not scroll, the
// dock is whole in the window, and the run detail ends on the dock and
// scrolls inside itself to its last line.
test('P1815 a long Run detail scrolls inside its column, and the dock stays in the window', async ({ meet, world }) => {
  const tag = `p1815-${String(Date.now())}`;
  const session = await openSession(world, world.tokens.worker, `${tag}-client`);
  // The longest task the schema allows (one line), and an outcome of many lines.
  const task = `${tag} ${'a long task for the layout '.repeat(18)}`.slice(0, 512);
  const outcome = Array.from({ length: 120 }, (_, i) => String(i)).join('\n').slice(0, 512);
  const started = await ok(world, world.tokens.worker, 'POST', `/v1/sessions/${String(session.id)}/runs`, runV1, { channel: 'general', task });
  await must(world, world.tokens.worker, 'POST', `/v1/runs/${String(started.id)}/state`, { state: 'running', outcome });
  try {
    const { page } = await meet({ viewport: { width: 1440, height: 900 } });
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await openChannel(page, 'general');
    const panel = page.getByRole('region', { name: 'Members list' });
    await panel.getByRole('button', { name: 'Show sessions for worker-test' }).click();
    await panel.getByRole('button', { name: new RegExp(`^Run #${String(started.id)} · `) }).click();
    const detail = page.getByRole('article', { name: 'Run detail' });
    await expect(detail.getByRole('heading', { name: `Run #${String(started.id)}` })).toBeVisible();
    const outcomeText = detail.locator('dd.ag-prose');
    await expect(outcomeText).toBeVisible();
    const dock = page.locator('.dock');

    for (const viewport of [{ width: 1440, height: 900 }, { width: 1215, height: 600 }]) {
      await page.setViewportSize(viewport);
      const where = `${String(viewport.width)}×${String(viewport.height)}, a long Run detail`;
      await expect(async () => {
        const scroll = await page.evaluate(() => ({ page: document.documentElement.scrollHeight, window: window.innerHeight }));
        expect(scroll.page, `${where}: the page is ${String(scroll.page)} px tall in a ${String(scroll.window)} px window`).toBeLessThanOrEqual(scroll.window + 1);
        const d = await boxOf(dock, `${where}: the dock`);
        expect(d.y, `${where}: the dock's top`).toBeGreaterThanOrEqual(0);
        expect(d.y + d.height, `${where}: the dock's bottom`).toBeLessThanOrEqual(viewport.height + 0.5);
        const r = await boxOf(detail, `${where}: the run detail`);
        expect(r.y + r.height, `${where}: the run detail's bottom`).toBeLessThanOrEqual(d.y + 1);
      }).toPass({ timeout: 5000 });
      // It scrolls inside itself, to its last line, above the dock.
      const inside = await detail.evaluate((el) => {
        el.scrollTop = el.scrollHeight;
        return { scrolled: el.scrollTop, overflow: el.scrollHeight - el.clientHeight };
      });
      expect(inside.overflow, `${where}: the run detail is taller than its box`).toBeGreaterThan(0);
      expect(inside.scrolled, `${where}: the run detail scrolls`).toBeGreaterThan(0);
      const last = await boxOf(outcomeText, `${where}: the outcome`);
      const d = await boxOf(dock, `${where}: the dock`);
      expect(last.y + last.height, `${where}: the outcome's end, scrolled to`).toBeLessThanOrEqual(d.y + 1);
    }
  } finally {
    await call(world, world.tokens.worker, 'DELETE', `/v1/sessions/${String(session.id)}`);
  }
});

// ---------------------------------------------------------------- issue #1980

// A maximized window on a Windows laptop's screen: the taskbar (48 px) and the
// window's title bar (32 px) come off the screen's height. Not in voice and
// not watching, with voice set up on the server, a members list and a log
// longer than the window: the page does not scroll and the voice dock is whole
// in the window. In the desktop app the client sits under the app's strip
// (ProfileBar's header.desktop-bar), in #root beside the lobby: the lobby
// takes what the strip leaves of the window. It used to be a whole window
// tall under the strip, so the dock was the strip's height under the window's
// edge and the page scrolled; the browser cases passed throughout.
const windowsScreens = [
  [1280, 800],
  [1366, 768],
  [1280, 720],
] as const;
const windowsChrome = 48 + 32;
const clientPlaces = ['in a browser', "under the desktop app's strip"] as const;

test('P1980 on a Windows laptop screen, not in voice, the dock is in the window and the page does not scroll', async ({ meet, secrets, world }) => {
  const tag = `p1980-${String(Date.now())}`;
  const channel = `${tag}-c`;
  await must(world, world.tokens.operator, 'POST', '/v0/channels', { name: channel });
  const me = await member(world, channel, `${tag}-me`);
  secrets.push(me.token);
  const others: Member[] = [];
  for (let i = 0; i < 30; i++) others.push(await member(world, channel, `${tag}-roster-${String(i)}`));
  for (const o of others) secrets.push(o.token);
  for (let i = 0; i < 40; i++) {
    await must(world, me.token, 'POST', `/v2/channels/${encodeURIComponent(channel)}/messages`, { body: `${tag} message ${String(i)} ${'words '.repeat(i % 7 === 0 ? 40 : 3)}` });
  }

  // One page per place, resized through the screens (one live session each on
  // the credential; see P1813).
  for (const place of clientPlaces) {
    const { page } = await meet({ viewport: { width: windowsScreens[0][0], height: windowsScreens[0][1] - windowsChrome } });
    await servePresence(page, world, me.token, channel, []);
    await signIn(page, world, me.token, me.name);
    await openChannel(page, channel);
    await expect(messages(page).getByText(`${tag} message 39`)).toBeVisible();
    const dock = page.locator('.dock');
    await expect(dock.getByRole('button', { name: 'Join voice' })).toBeVisible();
    if (place === "under the desktop app's strip") await drawDesktopStrip(page);
    for (const [width, height] of windowsScreens) {
      const viewport = { width, height: height - windowsChrome };
      await page.setViewportSize(viewport);
      const where = `a maximized window (${String(viewport.width)}×${String(viewport.height)}) on a ${String(width)}×${String(height)} screen, ${place}`;
      await expect(statusLine(page)).toContainText('Connected');
      await expect(page.getByText('Could not start a session'), `${where}: the members panel started a session`).toHaveCount(0);
      // Nothing overflows here, so the page does not scroll and the lobby
      // fills the window: the dock is on the window's bottom edge, not
      // floating above a blank band.
      await expect(async () => {
        const scroll = await page.evaluate(() => ({ page: document.documentElement.scrollHeight, window: window.innerHeight }));
        expect(scroll.page, `${where}: the page is ${String(scroll.page)} px tall in a ${String(scroll.window)} px window`).toBeLessThanOrEqual(scroll.window + 1);
        const d = await boxOf(dock, `${where}: the dock`);
        expect(d.y, `${where}: the dock's top`).toBeGreaterThanOrEqual(0);
        expect(Math.abs(d.y + d.height - viewport.height), `${where}: the dock's bottom is at ${String(d.y + d.height)}, the window's at ${String(viewport.height)}`).toBeLessThanOrEqual(1);
      }).toPass({ timeout: 5000 });
      // The composer still sits on the dock.
      const c = await boxOf(page.locator('.composer'), `${where}: the composer`);
      const d = await boxOf(dock, `${where}: the dock`);
      expect(Math.abs(c.y + c.height - d.y), `${where}: the composer is ${String(d.y - c.y - c.height)} px above the dock`).toBeLessThanOrEqual(1);
    }
  }
});

// The desktop app draws its strip over the sign-in card too, while it checks a
// kept credential ("Checking your sign-in…") and over the form after. The
// strip's layout is the lobby's alone (issue #1980): the card keeps the width
// it has in a browser, so it does not start narrow, fitted to its one line,
// and jump wider when the form replaces it. The check is held until the card
// has been measured, then answered as an expired credential, which brings the
// form back in the same page under the same strip.
test('P1980 under the desktop app\'s strip the "Checking your sign-in" card is as wide as the form', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1280, height: 720 } });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  let answer: () => void = () => undefined;
  const answered = new Promise<void>((resolve) => {
    answer = resolve;
  });
  await page.route(/\/v1\/whoami$/, async (route) => {
    await answered;
    await route.fulfill({ status: 401, contentType: 'application/json', body: '{"error":"unauthorized"}' });
  });
  await page.reload();
  const card = page.locator('#root > main.signin');
  await expect(card.getByRole('status')).toHaveText('Checking your sign-in…');
  await drawDesktopStrip(page);
  const checking = await boxOf(card, 'the "Checking your sign-in" card');
  answer();
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
  await expect(page.locator('#root > .desktop-bar'), 'the strip is still drawn over the form').toHaveCount(1);
  const form = await boxOf(card, 'the sign-in form');
  expect(Math.abs(checking.width - form.width), `the checking card is ${String(checking.width)} px wide, the form ${String(form.width)} px`).toBeLessThanOrEqual(1);
});

// A tall solid PNG, drawn in the page: an image attachment as the image-heavy
// chats of issue #2133 have them.
async function tallImage(page: Page): Promise<Uint8Array<ArrayBuffer>> {
  const image = await page.evaluate(async () => {
    const canvas = document.createElement('canvas');
    canvas.width = 320;
    canvas.height = 560;
    const context = canvas.getContext('2d');
    if (context === null) throw new Error('no canvas for the fixture image');
    context.fillStyle = '#168953';
    context.fillRect(0, 0, canvas.width, canvas.height);
    const png = await new Promise<Blob | null>((resolve) => {
      canvas.toBlob(resolve, 'image/png');
    });
    if (png === null) throw new Error('no fixture image');
    return Array.from(new Uint8Array(await png.arrayBuffer()));
  });
  return new Uint8Array(image);
}

test('P2133 scrolling an image-heavy desktop chat keeps the composer in place', async ({ meet, secrets, world }) => {
  const channel = `composer-${Date.now().toString(36)}`;
  await must(world, world.tokens.operator, 'POST', '/v0/channels', { name: channel });
  const me = await member(world, channel, `${channel}-member`);
  secrets.push(me.token);
  const { page } = await meet({ viewport: { width: 1215, height: 900 } });
  const bytes = await tallImage(page);
  for (let i = 0; i < 6; i++) {
    await attach(world, me.token, channel, `Attachment ${String(i)}`, [{ filename: `sample-${String(i)}.png`, mediaType: 'image/png', bytes }]);
  }
  await servePresence(page, world, me.token, channel, []);
  await signIn(page, world, me.token, me.name);
  await openChannel(page, channel);
  await drawDesktopStrip(page);

  const log = messages(page);
  const composer = page.locator('.composer');
  const dock = page.locator('.dock');
  await expect(log.getByRole('button', { name: 'Enlarge sample-5.png' })).toBeVisible();

  for (const height of [900, 560]) {
    await page.setViewportSize({ width: 1215, height });
    await log.evaluate((element) => {
      window.scrollTo(0, 0);
      element.scrollTop = element.scrollHeight;
    });
    const maximum = await log.evaluate((element) => element.scrollHeight - element.clientHeight);
    expect(maximum).toBeGreaterThan(0);
    if (height === 560) {
      expect(await page.evaluate(() => document.documentElement.scrollHeight)).toBeGreaterThan(height);
    }
    const before = await boxOf(composer, 'the composer before scrolling');
    const transcript = await boxOf(log, 'the transcript');
    await page.mouse.move(transcript.x + 40, transcript.y + 30);
    await page.mouse.wheel(0, -200);
    await expect.poll(() => log.evaluate((element) => element.scrollTop)).toBeLessThan(maximum - 20);
    await page.mouse.wheel(0, 2000);
    await expect.poll(() => log.evaluate((element) => element.scrollTop)).toBeGreaterThanOrEqual(maximum - 1);

    // Scroll past the end, allowing Chromium to apply a chained scroll if
    // there is one. The short window has extra document space to scroll into.
    await page.mouse.wheel(0, 2000);
    await page.evaluate(() => new Promise<void>((resolve) => {
      requestAnimationFrame(() => {
        requestAnimationFrame(() => { resolve(); });
      });
    }));
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
    const after = await boxOf(composer, 'the composer after scrolling');
    expect(after.y).toBeCloseTo(before.y, 0);
    expect(after.y + after.height).toBeCloseTo((await boxOf(dock, 'the voice dock')).y, 0);

    if (height === 560) {
      const header = await boxOf(page.locator('.topbar'), 'the top bar');
      await page.mouse.move(header.x + 20, header.y + header.height / 2);
      await page.mouse.wheel(0, 2000);
      await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
      await expect(page.getByRole('textbox', { name: `Message #${channel}` })).toBeInViewport();
    }
  }
});

// Issue #2133 at a normal desktop size. A row without menu actions is not
// positioned: a send still pending (as one is while its images upload), or a
// message its author removed (a tombstone has no menu, and stays). Its
// visually hidden text took .log-wrap as its containing block and sat at its
// unscrolled place, outside the transcript's clip: .centre grew thousands of
// pixels of empty overflow, a second scrollbar, and a wheel over the composer
// slid it away from the dock. .log is now that containing block.
for (const trigger of ['a pending send', 'a removed message'] as const) {
  test(`P2133 ${trigger} in an image-heavy chat adds no scroll to the conversation column`, async ({ meet, secrets, world }) => {
    const channel = `unpositioned-${Date.now().toString(36)}`;
    await must(world, world.tokens.operator, 'POST', '/v0/channels', { name: channel });
    const me = await member(world, channel, `${channel}-member`);
    const other = await member(world, channel, `${channel}-other`);
    secrets.push(me.token, other.token);
    const { page } = await meet({ viewport: { width: 1215, height: 900 } });
    const bytes = await tallImage(page);
    for (let i = 0; i < 6; i++) {
      await attach(world, other.token, channel, `Attachment ${String(i)}`, [{ filename: `sample-${String(i)}.png`, mediaType: 'image/png', bytes }]);
    }
    if (trigger === 'a removed message') {
      // The newest row: a message its author removed.
      const posted = await post(world, me.token, channel, 'soon removed');
      expect((await call(world, me.token, 'DELETE', `/v2/channels/${channel}/messages/${String(posted.id)}/own`)).status).toBe(204);
    }
    await servePresence(page, world, me.token, channel, []);
    await signIn(page, world, me.token, me.name);
    await openChannel(page, channel);
    await drawDesktopStrip(page);
    const log = messages(page);
    await expect(log.getByRole('button', { name: 'Enlarge sample-5.png' })).toBeVisible();

    if (trigger === 'a pending send') {
      // Hold the send, so its row stays pending.
      await page.route('**/v2/channels/*/messages', (route) => (route.request().method() === 'POST' ? new Promise<void>(() => undefined) : route.fallback()));
      await page.getByRole('textbox', { name: `Message #${channel}` }).fill('still sending');
      await page.keyboard.press('Enter');
      await expect(log.locator('.message.pending')).toHaveCount(1);
    } else {
      await expect(log.locator('.message:not(.has-actions)')).toHaveCount(1);
    }
    await log.evaluate((element) => { element.scrollTop = element.scrollHeight; });
    // The transcript itself overflows, so the check below is not vacuous.
    expect(await log.evaluate((element) => element.scrollHeight - element.clientHeight)).toBeGreaterThan(0);

    const centre = page.locator('.centre');
    const overflow = () => centre.evaluate((element) => element.scrollHeight - element.clientHeight);
    expect(await overflow(), 'the conversation column has no scroll of its own').toBeLessThanOrEqual(1);

    const composer = page.locator('.composer');
    const before = await boxOf(composer, 'the composer before the wheel');
    await page.mouse.move(before.x + before.width / 2, before.y + 8);
    await page.mouse.wheel(0, 3000);
    await page.evaluate(() => new Promise<void>((resolve) => {
      requestAnimationFrame(() => {
        requestAnimationFrame(() => { resolve(); });
      });
    }));
    expect(await centre.evaluate((element) => element.scrollTop)).toBe(0);
    const after = await boxOf(composer, 'the composer after the wheel');
    expect(after.y).toBeCloseTo(before.y, 0);
    expect(after.y + after.height).toBeCloseTo((await boxOf(page.locator('.dock'), 'the voice dock')).y, 0);
  });
}
