// Issue #1493: favorites, collapsible groups and ordering of the channel list,
// kept in this browser per account and server. It is a view: nothing here
// changes a membership, and nothing touches voice.
import type { Page } from '@playwright/test';
import { makeChannel, memberOf } from './api';
import { channelButton, conversation, expect, expectFits, mainNav, phones, rememberPushToTalk, showView, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld, principalId } from './world';

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });
const actions = (page: Page, name: string) => channelButton(page, name).locator('xpath=..').getByRole('button', { name: `Actions for ${name}` });

async function choose(page: Page, name: string, item: string | RegExp, how: 'click' | 'tap' = 'click'): Promise<void> {
  await actions(page, name)[how]();
  await page.getByRole('menuitem', { name: item }).first()[how]();
}

// A fresh channel of this test that the browser account is in.
async function joined(world: Parameters<typeof makeChannel>[0], suffix: string, count: number): Promise<string[]> {
  const names: string[] = [];
  const id = await principalId(world, world.tokens.browser);
  for (let i = 1; i <= count; i++) {
    const name = `c1493-${suffix}-${String(Date.now())}-${String(i)}`;
    await makeChannel(world, name);
    await memberOf(world, name, id, true);
    names.push(name);
  }
  return names;
}

const favorites = (page: Page) => nav(page).getByRole('group', { name: 'Favorites' });
const orderIn = async (page: Page, group: string) => (await nav(page).getByRole('group', { name: group }).getByRole('button', { name: /^(c1493-[^⋯]*|general)$/ }).allTextContents()).map((t) => t.trim());
const arrangementKeys = (page: Page) => page.evaluate(() => Object.keys(window.localStorage).filter((k) => k.startsWith('conch.web.arrangement.v1.')));

test('C1493 two accounts in one browser keep separate arrangements, which survive a reload', async ({ meet, world }) => {
  const [a] = await joined(world, 'ident', 1);
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(channelButton(page, a ?? '')).toBeVisible();
  await choose(page, a ?? '', 'Favorite');
  await expect(favorites(page).getByRole('button', { name: a ?? '', exact: true })).toBeVisible();

  // A group, collapsed; both survive a reload.
  await nav(page).getByRole('button', { name: 'Edit groups' }).click();
  await page.getByLabel('New group name').fill('Raids');
  await page.getByRole('button', { name: 'Add group' }).click();
  await page.getByRole('button', { name: 'Done' }).click();
  await nav(page).getByRole('button', { name: /^Raids/ }).click();
  await expect(nav(page).getByRole('button', { name: /^Raids/ })).toHaveAttribute('aria-expanded', 'false');
  await page.reload();
  await expect(statusOf(page)).toContainText('Connected');
  await expect(favorites(page).getByRole('button', { name: a ?? '', exact: true })).toBeVisible();
  await expect(nav(page).getByRole('button', { name: /^Raids/ })).toHaveAttribute('aria-expanded', 'false');

  // Another account, same browser: nothing of the first, and nothing in its text.
  await page.evaluate(() => {
    window.localStorage.removeItem('conch.web.token');
    window.sessionStorage.removeItem('conch.web.token');
  });
  await signIn(page, world, world.tokens.nick, 'nick-test');
  await expect(favorites(page)).toHaveCount(0);
  await expect(nav(page).getByRole('button', { name: /^Raids/ })).toHaveCount(0);
  await expect(nav(page)).not.toContainText(a ?? '');

  // And back: the first account's arrangement is there again.
  await page.evaluate(() => {
    window.localStorage.removeItem('conch.web.token');
    window.sessionStorage.removeItem('conch.web.token');
  });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(favorites(page).getByRole('button', { name: a ?? '', exact: true })).toBeVisible();
  expect((await arrangementKeys(page)).length).toBeGreaterThanOrEqual(1);
});

async function stored(page: Page): Promise<{ favorites: number[] }[]> {
  const raws: string[] = await page.evaluate(() => {
    const out: string[] = [];
    for (let i = 0; i < window.localStorage.length; i++) {
      const key = window.localStorage.key(i);
      const value = key?.startsWith('conch.web.arrangement.v1.') === true ? window.localStorage.getItem(key) : null;
      if (value !== null) out.push(value);
    }
    return out;
  });
  return raws.map((raw) => JSON.parse(raw) as { favorites: number[] });
}

const statusOf = (page: Page) => page.getByRole('status', { name: 'Status' });

test('C1493 a favorite whose channel is revoked leaves the list and the stored arrangement', async ({ meet, world }) => {
  const [a, b] = await joined(world, 'revoke', 2);
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await choose(page, a ?? '', 'Favorite');
  await choose(page, b ?? '', 'Favorite');
  await expect(favorites(page).getByRole('button', { name: b ?? '', exact: true })).toBeVisible();

  await memberOf(world, b ?? '', await principalId(world, world.tokens.browser), false);
  await expect(channelButton(page, b ?? '')).toHaveCount(0);
  await expect(favorites(page).getByRole('button', { name: a ?? '', exact: true })).toBeVisible();
  await expect(nav(page)).not.toContainText(b ?? '');
  // The stored arrangement keeps one favorite: the revoked channel's id is gone.
  await expect
    .poll(async () => (await stored(page)).filter((v) => v.favorites.length === 1).length, { message: 'the stored arrangement was pruned' })
    .toBeGreaterThan(0);
});

test('C1493 channels are ordered and grouped from the keyboard alone, and the switcher follows that order', async ({ meet, world }) => {
  const [a, b, c] = await joined(world, 'keys', 3);
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  for (const name of [a, b, c]) await choose(page, name ?? '', 'Favorite');
  expect(await orderIn(page, 'Favorites')).toEqual([a, b, c]);
  await choose(page, b ?? '', 'Unfavorite');
  expect(await orderIn(page, 'Favorites')).toEqual([a, c]);

  // Keyboard: focus the ⋯, open it with Enter, pick with the arrows and Enter.
  await actions(page, c ?? '').focus();
  await page.keyboard.press('Enter');
  await page.getByRole('menuitem', { name: 'Move up' }).focus();
  await page.keyboard.press('Enter');
  expect(await orderIn(page, 'Favorites')).toEqual([c, a]);
  await expect(nav(page).getByRole('status').filter({ hasText: 'moved up' })).toContainText('moved up in favorites, position 1 of 2');
  await expect(channelButton(page, c ?? '')).toBeFocused();

  // Move b to a group, by keyboard.
  await nav(page).getByRole('button', { name: 'Edit groups' }).click();
  await page.getByLabel('New group name').fill('Play');
  await page.keyboard.press('Enter');
  await page.keyboard.press('Escape');
  await actions(page, b ?? '').focus();
  await page.keyboard.press('Enter');
  await page.getByRole('menuitem', { name: 'Move to group…' }).focus();
  await page.keyboard.press('Enter');
  await page.getByRole('combobox', { name: 'Group' }).selectOption({ label: 'Play' });
  await page.keyboard.press('Tab');
  await page.keyboard.press('Enter');
  await expect(nav(page).getByRole('group', { name: 'Play' }).getByRole('button', { name: b ?? '', exact: true })).toBeVisible();

  // The switcher's Alt+Down follows the drawn order: c, a (favorites), b (Play), then the rest.
  await channelButton(page, c ?? '').click();
  await page.getByRole('heading', { name: 'Conch' }).click();
  await page.keyboard.press('Alt+ArrowDown');
  await expect(conversation(page).getByRole('heading', { name: a ?? '' })).toBeVisible();
  await page.keyboard.press('Alt+ArrowDown');
  await expect(conversation(page).getByRole('heading', { name: b ?? '' })).toBeVisible();
});

for (const [size, options] of phones) {
  test(`C1493 at ${size} the arrangement is made with taps through the ⋯ menu and fits the screen`, async ({ meet, world }) => {
    const [a, b] = await joined(world, `phone${size}`, 2);
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await showView(page, 'Channels');
    await choose(page, a ?? '', 'Favorite', 'tap');
    await choose(page, b ?? '', 'Favorite', 'tap');
    await choose(page, b ?? '', 'Move up', 'tap');
    expect(await orderIn(page, 'Favorites')).toEqual([b, a]);
    await choose(page, a ?? '', 'Unfavorite', 'tap');
    await nav(page).getByRole('button', { name: 'Edit groups' }).tap();
    await page.getByLabel('New group name').fill('Phone group');
    await page.getByRole('button', { name: 'Add group' }).tap();
    await page.getByRole('button', { name: 'Done' }).tap();
    await choose(page, a ?? '', 'Move to group…', 'tap');
    await page.getByRole('combobox', { name: 'Group' }).selectOption({ label: 'Phone group' });
    await page.getByRole('button', { name: 'Move', exact: true }).tap();
    await expect(nav(page).getByRole('group', { name: 'Phone group' }).getByRole('button', { name: a ?? '', exact: true })).toBeVisible();
    await nav(page).getByRole('button', { name: /^Phone group/ }).tap();
    await expect(nav(page).getByRole('button', { name: /^Phone group/ })).toHaveAttribute('aria-expanded', 'false');
    await expectFits(page, 'the Channels view with favorites and a folded group');
    await expect(mainNav(page).getByRole('button', { name: 'Channels', exact: true })).toHaveAttribute('aria-current', 'page');
  });
}

test.describe('with a voice session', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;
  test.beforeAll(async ({}, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the arrangement voice test needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the voice leg of the arrangement suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  test('C1493 favoriting, grouping and reordering leave the voice session as it was', async ({ browser }) => {
    const other = `c1493-${String(Date.now())}-other`;
    expect((await vw.api(vw.operator, 'POST', '/v0/channels', { name: other })).status).toBeLessThan(300);
    expect((await vw.api(vw.operator, 'PUT', `/v1/channels/${other}/members/${String(vw.alice.id)}`)).status).toBeLessThan(300);
    const context = await browser.newContext();
    await rememberPushToTalk(context);
    const page = await context.newPage();
    try {
      await signIn(page, vw, vw.alice.token, vw.alice.name);
      const voice = page.getByRole('region', { name: 'Voice' });
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText(/Connected in #voice-room/, { timeout: 30_000 });

      await choose(page, other, 'Favorite');
      await choose(page, 'voice-room', 'Favorite');
      await choose(page, 'voice-room', 'Move up');
      await nav(page).getByRole('button', { name: 'Edit groups' }).click();
      await page.getByLabel('New group name').fill('Calls');
      await page.getByRole('button', { name: 'Add group' }).click();
      await page.getByRole('button', { name: 'Done' }).click();
      await choose(page, other, 'Move to group…');
      await page.getByRole('combobox', { name: 'Group' }).selectOption({ label: 'Calls' });
      await page.getByRole('button', { name: 'Move', exact: true }).click();
      await nav(page).getByRole('button', { name: /^Calls/ }).click();

      await expect(voice).toContainText(/Connected in #voice-room/);
      await expect(voice.getByRole('button', { name: 'Leave voice' })).toBeVisible();
      await expect(voice.getByRole('button', { name: 'Join voice' })).toHaveCount(0);
    } finally {
      await page.close();
      await context.close();
    }
  });
});
