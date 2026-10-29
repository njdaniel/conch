// Issue #536: the lobby's layout at a phone, a tablet and a desktop size, and
// at 200% zoom (a 1440×900 screen zoomed is a 720×450 window). At each, with a
// long channel name, a long person's name, long messages and a larger roster:
// nothing widens the page, the composer and the voice strip are on screen and
// not covered, and the members and settings panels open, fit, take focus and
// give it back on Escape. Media continuity through these panels is in
// 12-screen-share, which has LiveKit.
import type { BrowserContextOptions, Locator, Page, WebSocketRoute } from '@playwright/test';
import { voicePresenceV1, type VoicePresenceV1 } from '../src/schema/voice';
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
// in this test; the product shows only who the server reports.
interface Presence {
  // show sends a new document on every socket the page has open.
  show: (who: readonly number[], sharing?: number) => void;
}

async function servePresence(page: Page, world: World, token: string, channel: string, who: readonly number[]): Promise<Presence> {
  const real = await ok(world, token, 'GET', `/v1/channels/${encodeURIComponent(channel)}/voice`, voicePresenceV1);
  const doc = (people: readonly number[], sharing?: number): string => {
    const presence: VoicePresenceV1 = {
      ...real,
      configured: true,
      available: true,
      rooms: [
        {
          participants: people.map((id) => ({ principal_id: id, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z', ...(id === sharing ? { sharing: true } : {}) })),
        },
      ],
    };
    expect(voicePresenceV1(presence), 'the faked presence document').toEqual([]);
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
  // The roster is in principal order, and the viewer is "You".
  const names = ['You', ...others.map((o) => o.name)];

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
