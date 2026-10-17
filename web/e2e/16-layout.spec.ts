// Issue #536: the lobby's layout at a phone, a tablet and a desktop size, and
// at 200% zoom (a 1440×900 screen zoomed is a 720×450 window). At each, with a
// long channel name, a long person's name, long messages and a larger roster:
// nothing widens the page, the composer and the voice strip are on screen and
// not covered, and the members and settings panels open, fit, take focus and
// give it back on Escape. Media continuity through these panels is in
// 12-screen-share, which has LiveKit.
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { channelButton, expect, expectFits, messages, phones, signIn, test } from './fixtures';
import { call, type World } from './world';

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
  readonly name: string;
  readonly token: string;
}

async function member(world: World, channel: string, name: string): Promise<Member> {
  const op = world.tokens.operator;
  const made = (await must(world, op, 'POST', '/v0/principals', { kind: 'human', name })) as { principal: { id: number } };
  const cred = (await must(world, op, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, { label: name })) as { token: string };
  await must(world, op, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(made.principal.id)}`);
  return { name, token: cred.token };
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
