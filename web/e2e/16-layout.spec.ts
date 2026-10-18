// Issue #536: the lobby's layout at a phone, a tablet and a desktop size, and
// at 200% zoom (a 1440×900 screen zoomed is a 720×450 window). At each, with a
// long channel name, a long person's name, long messages and a larger roster:
// nothing widens the page, the composer and the voice strip are on screen and
// not covered, and the members and settings panels open, fit, take focus and
// give it back on Escape. Media continuity through these panels is in
// 12-screen-share, which has LiveKit.
import type { BrowserContextOptions, Locator, Page, WebSocketRoute } from '@playwright/test';
import { voicePresenceV1, type VoicePresenceV1 } from '../src/schema/voice';
import { channelButton, expect, expectFits, messages, phones, signIn, test } from './fixtures';
import { call, ok, type World } from './world';

const sizes: readonly (readonly [string, BrowserContextOptions])[] = [
  ...phones,
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
    await channelButton(page, channel).click();
    await expect(messages(page).getByText(longWord)).toBeVisible();
    await expectFits(page, `${size}: the lobby`);
    await expectUncovered(page, page.getByLabel(`Message #${channel}`), `${size}: the composer`);
    await expectUncovered(page, page.getByRole('region', { name: 'Voice' }), `${size}: the voice strip`);

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
  show: (who: readonly number[]) => void;
}

async function servePresence(page: Page, world: World, token: string, channel: string, who: readonly number[]): Promise<Presence> {
  const real = await ok(world, token, 'GET', `/v1/channels/${encodeURIComponent(channel)}/voice`, voicePresenceV1);
  const doc = (people: readonly number[]): string => {
    const presence: VoicePresenceV1 = {
      ...real,
      configured: true,
      available: true,
      rooms: [
        {
          participants: people.map((id) => ({ principal_id: id, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' })),
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
    show: (people) => {
      current = doc(people);
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
  [phones[0][0], phones[0][1], true],
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
    await channelButton(page, channel).click();
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
      // Three chips, then "+5 more" for the rest.
      const more = strip.getByRole('button', { name: '+5 more' });
      await expect(more).toBeVisible();
      await expect(more).toHaveAttribute('aria-expanded', 'false');
      for (let i = 0; i < 8; i++) {
        if (i < 3) await expect(chips.nth(i)).toBeVisible();
        else await expect(chips.nth(i)).toBeHidden();
      }
      await more.click();
      await expect(strip.getByRole('button', { name: 'Show fewer' })).toHaveAttribute('aria-expanded', 'true');
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
    await expectUncovered(page, page.getByLabel(`Message #${channel}`), `${size}: the composer`);
  }
});
