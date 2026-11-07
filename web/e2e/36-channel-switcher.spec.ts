// Issue #1492: the quick channel switcher, keyboard navigation and unread jumps.
// The switcher searches only the channels the server lists for the signed-in
// person; a channel they lose leaves it; moving about never touches voice.
import type { Page } from '@playwright/test';
import { makeChannel, memberOf, post } from './api';
import { channelButton, conversation, expect, expectFits, mainNav, phones, rememberPushToTalk, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld, principalId, setChannelVisibility } from './world';

const switcher = (page: Page) => page.getByRole('dialog', { name: 'Find a channel' });
// Shortcuts leave a text field alone, so a test of them first takes focus out of the composer.
const blur = (page: Page) => page.getByRole('heading', { name: 'Conch' }).click();
const finder = (page: Page) => page.getByRole('combobox', { name: 'Find a channel' });

test('C1492 two accounts see only their own private channels in the switcher', async ({ meet, world }) => {
  // nick-test is in test-private; browser-test is not (04-private).
  const nick = (await meet()).page;
  const browser = (await meet()).page;
  await signIn(nick, world, world.tokens.nick, 'nick-test');
  await signIn(browser, world, world.tokens.browser, 'browser-test');

  await blur(nick);
  await nick.keyboard.press('Control+k');
  await finder(nick).fill('private');
  await expect(switcher(nick).getByRole('option', { name: /test-private/ })).toBeVisible();
  await expect(switcher(nick).getByRole('option', { name: /test-private/ })).toContainText('Private');
  await nick.keyboard.press('Escape');

  await blur(browser);
  await browser.keyboard.press('Control+k');
  await finder(browser).fill('private');
  await expect(switcher(browser).getByText('No channels match.')).toBeVisible();
  await expect(switcher(browser)).not.toContainText('test-private');
  // Not even the Join group names it: the directory lists only what the server lets this person see.
  await expect(browser.getByRole('option', { name: /test-private/ })).toHaveCount(0);
});

test('C1492 a channel revoked while the switcher is open leaves the list and opens nothing', async ({ meet, world }) => {
  const name = `c1492-${String(Date.now())}-room`;
  await makeChannel(world, name);
  const id = await principalId(world, world.tokens.browser);
  await memberOf(world, name, id, true);
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(channelButton(page, name)).toBeVisible();

  await blur(page);
  await page.keyboard.press('Control+k');
  await finder(page).fill('c1492');
  await expect(switcher(page).getByRole('option', { name: new RegExp(name) })).toBeVisible();

  await memberOf(world, name, id, false);
  await expect(switcher(page).getByRole('option', { name: new RegExp(name) })).toHaveCount(0);
  await expect(switcher(page).getByText('No channels match.')).toBeVisible();
  await page.keyboard.press('Enter');
  await expect(conversation(page).getByRole('heading', { name })).toHaveCount(0);
});

test('C1492 a public channel to join that turns private while the switcher is open leaves the Join group', async ({ meet, world }) => {
  const name = `c1492-${String(Date.now())}-turns`;
  await makeChannel(world, name);
  await setChannelVisibility(world, name, 'public');
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await blur(page);
  await page.keyboard.press('Control+k');
  await finder(page).fill(name);
  const row = switcher(page).getByRole('group', { name: 'Join' }).getByRole('option', { name: new RegExp(name) });
  await expect(row).toBeVisible();
  await setChannelVisibility(world, name, 'private');
  await expect(row).toHaveCount(0);
  await expect(switcher(page).getByText('No channels match.')).toBeVisible();
});

test('C1492 focus lands on the conversation heading after choosing another channel or the current one', async ({ meet, world }) => {
  const name = `c1492-${String(Date.now())}-focus`;
  await makeChannel(world, name);
  await memberOf(world, name, await principalId(world, world.tokens.browser), true);
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(channelButton(page, name)).toBeVisible();
  for (const target of [name, 'general']) {
    await blur(page);
    await page.keyboard.press('Control+k');
    await finder(page).fill(target);
    await page.keyboard.press('Enter');
    await expect(conversation(page).getByRole('heading', { name: target })).toBeVisible();
    await expect(page.locator('#conversation-heading')).toBeFocused();
  }
  // The current channel again.
  await blur(page);
  await page.keyboard.press('Control+k');
  await finder(page).fill('general');
  await page.keyboard.press('Enter');
  await expect(page.locator('#conversation-heading')).toBeFocused();
});

test('C1492 Ctrl+K, Escape focus, Alt+Arrow and the unread jump work from the keyboard', async ({ meet, world }) => {
  const name = `c1492-${String(Date.now())}-unread`;
  await makeChannel(world, name);
  await memberOf(world, name, await principalId(world, world.tokens.browser), true);
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(channelButton(page, name)).toBeVisible();

  // The page moves focus to the conversation heading when a channel opens; wait for that, then type.
  await expect(page.locator('#conversation-heading')).toBeFocused();
  const composer = page.locator('#composer-text');
  await expect(composer).toBeEnabled();
  await composer.click();
  await composer.fill('c1492 half a thought');

  // The composer keeps the page's shortcuts, and its draft is as it was.
  await page.keyboard.press('Control+k');
  await expect(finder(page)).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(composer).toBeFocused();
  await expect(composer).toHaveValue('c1492 half a thought');
  await page.keyboard.press('Alt+ArrowDown');
  await expect(conversation(page).getByRole('heading', { name: 'general' })).toHaveCount(0);
  await expect(composer).not.toHaveValue('c1492 half a thought');
  await page.keyboard.press('Alt+ArrowUp');
  await expect(conversation(page).getByRole('heading', { name: 'general' })).toBeVisible();
  await expect(composer).toHaveValue('c1492 half a thought');
  await composer.fill('');

  // Another text field keeps its keys: the search box.
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await page.locator('#channel-search').focus();
  await page.keyboard.press('Control+k');
  await expect(switcher(page)).toHaveCount(0);
  await page.getByRole('button', { name: /^(Search|Close search)/ }).first().click();
  await expect(page.locator('#channel-search')).toHaveCount(0);

  await blur(page);
  // Escape returns focus to the control that opened it.
  const entry = page.getByRole('button', { name: /Find a channel/ });
  await entry.focus();
  await page.keyboard.press('Enter');
  await expect(finder(page)).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(entry).toBeFocused();

  // Nothing unread elsewhere: a status line, and the channel stays.
  await page.keyboard.press('Alt+Shift+ArrowDown');
  await expect(page.locator('.nav-status')).toHaveText('No unread channels');

  // Something unread: the jump goes there.
  await post(world, world.tokens.operator, name, 'c1492 unread');
  await expect(channelButton(page, name).locator('xpath=..').getByRole('img', { name: / unread$/ })).toBeVisible();
  await page.keyboard.press('Alt+Shift+ArrowDown');
  await expect(conversation(page).getByRole('heading', { name })).toBeVisible();

  // Alt+Up and Alt+Down walk the sidebar.
  await page.keyboard.press('Alt+ArrowUp');
  await expect(conversation(page).getByRole('heading', { name })).toHaveCount(0);
});

for (const [size, options] of phones) {
  test(`C1492 at ${size} Ctrl+K from the Chat view opens a visible switcher that takes focus`, async ({ meet, world }) => {
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await expect(mainNav(page).getByRole('button', { name: 'Chat', exact: true })).toHaveAttribute('aria-current', 'page');
    await expect(page.locator('#composer-text')).toBeEnabled();
    await page.locator('#composer-text').focus();
    await page.keyboard.press('Control+k');
    await expect(finder(page)).toBeVisible();
    await expect(finder(page)).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(switcher(page)).toHaveCount(0);
    // Closed, it blocks nothing: the shortcut works again.
    await page.keyboard.press('Control+k');
    await expect(finder(page)).toBeFocused();
  });


  test(`C1492 at ${size} the switcher opens by tap, fits the screen and opens the channel`, async ({ meet, world }) => {
    const name = `c1492-${size}-${String(Date.now())}-phone`;
    await makeChannel(world, name);
    await memberOf(world, name, await principalId(world, world.tokens.browser), true);
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await mainNav(page).getByRole('button', { name: 'Channels', exact: true }).tap();
    await page.getByRole('button', { name: /Find a channel/ }).tap();
    await expect(finder(page)).toBeVisible();
    await finder(page).fill('c1492');
    await expectFits(page, 'the channel switcher');
    await switcher(page).getByRole('option', { name: new RegExp(name) }).tap();
    await expect(switcher(page)).toHaveCount(0);
    await expect(mainNav(page).getByRole('button', { name: 'Chat', exact: true })).toHaveAttribute('aria-current', 'page');
    await expect(conversation(page).getByRole('heading', { name })).toBeVisible();
  });
}

test.describe('with a voice session', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;
  test.beforeAll(async ({}, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the switcher voice test needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the voice leg of the switcher suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  test('C1492 switching channels by the switcher and the shortcuts leaves the voice session as it was', async ({ browser }) => {
    const other = `c1492-${String(Date.now())}-other`;
    const made = await vw.api(vw.operator, 'POST', '/v0/channels', { name: other });
    expect(made.status).toBeLessThan(300);
    expect((await vw.api(vw.operator, 'PUT', `/v1/channels/${other}/members/${String(vw.alice.id)}`)).status).toBeLessThan(300);
    const context = await browser.newContext();
    await rememberPushToTalk(context);
    const page = await context.newPage();
    try {
      await signIn(page, vw, vw.alice.token, vw.alice.name);
      const voice = page.getByRole('region', { name: 'Voice' });
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
      const before = await voice.innerText();

      await blur(page);
      await page.keyboard.press('Control+k');
      await finder(page).fill('other');
      // Browsing a result does nothing to voice.
      await expect(voice).toContainText(/Connected in #voice-room/);
      await page.keyboard.press('Enter');
      await expect(conversation(page).getByRole('heading', { name: other })).toBeVisible();
      await page.keyboard.press('Alt+ArrowUp');
      await page.keyboard.press('Alt+ArrowDown');
      await page.keyboard.press('Alt+Shift+ArrowDown');

      await expect(voice).toContainText(/Connected in #voice-room/);
      await expect(voice.getByRole('button', { name: 'Leave voice' })).toBeVisible();
      await expect(voice.getByRole('button', { name: 'Join voice' })).toHaveCount(0);
      expect((await voice.innerText()).includes('Connected in #voice-room')).toBe(before.includes('Connected in #voice-room'));
    } finally {
      await page.close();
      await context.close();
    }
  });
});
