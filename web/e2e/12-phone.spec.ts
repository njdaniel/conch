// Issue #467: the lobby fits a phone. At each phone size the lobby, a
// conversation and the panels beside it are no wider than the window, and a
// flow driven by taps alone lands where it is aimed. The voice panel, which
// needs LiveKit to show its controls, is tried at phone size in 10-voice; the
// operator's invite panel in 11-invites. This spec runs after both, so the
// person it adds to the fixture channels touches no other spec.
import type { Locator, Page } from '@playwright/test';
import { makeInvite, post } from './api';
import { accountPassword, channelButton, conversation, expect, expectFits, expectStateReadable, phoneSizes, mainNav, messages, phones, showView, signIn, statusLine, test } from './fixtures';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import type { InvitePermissionRequestV1 } from '../src/schema/generated';
import { call, inviteUrl, ok, type World } from './world';

// Things a phone's width finds first: a word with no place to break, an
// address, and a line long enough to wrap many times. Each size posts its own.
const longWord = (size: string) => `p467-${size}-${'w'.repeat(160)}`;
const longAddress = (size: string) => `https://example.com/p467/${size}/${'segment/'.repeat(20)}end`;
const longLine = (size: string) => `p467 ${size} ${'a line that goes on for a while, '.repeat(12)}and ends`;

const unique = (label: string) => `p467-${label}-${String(Date.now())}`;

for (const [size, options] of phones) {
  test(`P467 at ${size} the lobby, a conversation, the account form, the report panel and the members panel fit the screen`, async ({ meet, world }) => {
    for (const body of [longWord(size), longAddress(size), longLine(size)]) await post(world, world.tokens.nick, 'general', body);
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
    await expect(messages(page).getByText(longWord(size))).toBeVisible();
    await expect(messages(page).getByText(longAddress(size))).toBeVisible();
    await expect(statusLine(page)).not.toContainText(world.origin);
    await expectFits(page, 'the lobby with general open');
    // On the shared server the voice section says voice is not set up. It is
    // the Voice view's; not in voice, the strip is not under Chat.
    await expect(page.getByRole('region', { name: 'Voice' })).toBeHidden();
    await showView(page, 'Voice');
    await expect(page.getByRole('region', { name: 'Voice' })).toContainText('Voice is not set up on this server');
    await expectFits(page, 'the Voice view');
    await showView(page, 'Channels');
    await expectFits(page, 'the Channels view');
    await showView(page, 'Chat');

    // The whisper picker lays its people out in the composer.
    await page.getByLabel('To:').selectOption({ label: 'Whisper to…' });
    await expect(conversation(page).getByRole('group', { name: 'Whisper to' })).toBeVisible();
    await expectFits(page, 'the composer with the whisper picker open');
    await page.getByLabel('To:').selectOption({ label: '#general' });

    // The account form and Report a problem are in Settings (issue #536).
    const settings = page.getByRole('button', { name: 'Settings' });
    await settings.tap();
    await expect(page.getByRole('dialog', { name: 'Settings' })).toBeVisible();
    await expectFits(page, 'the lobby with Settings open');
    await page.getByRole('button', { name: 'Change your name' }).tap();
    await expect(page.getByLabel('New name')).toBeVisible();
    await expectFits(page, 'the lobby with the account form open');
    await page.getByRole('button', { name: 'Change your name' }).tap();

    // The Report a problem panel (issue #511) stays on the screen.
    const report = page.getByRole('button', { name: 'Report a problem' });
    await report.tap();
    await expect(report).toHaveAttribute('aria-expanded', 'true');
    await expect(page.getByRole('form', { name: 'Report a problem' })).toBeVisible();
    await expectFits(page, 'the lobby with the report panel open');
    await expectOnScreen(page, page.locator(`[id="${(await report.getAttribute('aria-controls')) ?? ''}"]`), 'the report panel');
    await report.tap();
    await expect(page.getByRole('form', { name: 'Report a problem' })).toBeHidden();
    await page.getByRole('dialog', { name: 'Settings' }).getByRole('button', { name: 'Close', exact: true }).tap();
    await expect(settings).toBeFocused();

    // The members panel starts closed on a narrow screen.
    const members = page.getByRole('button', { name: 'Members' });
    await expect(members).toHaveAttribute('aria-expanded', 'false');
    await members.tap();
    await expect(page.getByRole('region', { name: 'Members list' })).toBeVisible();
    await expectFits(page, 'the lobby with the members panel open');
  });

  // Issue #589: the whisper picker takes height from the log. A reader at the
  // newest message stays there; one who has scrolled up is not moved.
  test(`P589 at ${size} the newest message stays in view when the whisper picker opens and closes`, async ({ meet, world }) => {
    const bodies = Array.from({ length: 40 }, (_, i) => `p589 ${size} line ${String(i + 1)}`);
    for (const body of bodies) await post(world, world.tokens.nick, 'general', body);
    const last = bodies[bodies.length - 1] ?? '';
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    const log = messages(page);
    const newest = log.getByRole('listitem').filter({ hasText: last });
    await expect(newest).toBeVisible();
    const scrolls = await log.evaluate((el) => el.scrollHeight > el.clientHeight);
    expect(scrolls, 'the conversation is longer than the log').toBe(true);
    await expectInside(page, newest, log, 'the newest message on opening the channel');

    const to = page.getByLabel('To:');
    const picker = conversation(page).getByRole('group', { name: 'Whisper to' });
    await to.selectOption({ label: 'Whisper to…' });
    await expect(picker).toBeVisible();
    await expectInside(page, newest, log, 'the newest message with the whisper picker open');
    await to.selectOption({ label: '#general' });
    await expect(picker).toBeHidden();
    await expectInside(page, newest, log, 'the newest message with the whisper picker closed');

    // Scrolled up by more than the 24px that still counts as the end.
    await log.evaluate((el) => {
      el.scrollTop = el.scrollHeight - el.clientHeight - 200;
    });
    await frames(page);
    const before = await firstVisible(log);
    await to.selectOption({ label: 'Whisper to…' });
    await expect(picker).toBeVisible();
    await frames(page);
    expect(Math.abs((await firstVisible(log)).top - before.top), `${before.text} moved with the picker open`).toBeLessThanOrEqual(1);
    await to.selectOption({ label: '#general' });
    await expect(picker).toBeHidden();
    await frames(page);
    const after = await firstVisible(log);
    expect(after.text, 'the first message in view').toBe(before.text);
    expect(Math.abs(after.top - before.top), `${before.text} moved with the picker closed`).toBeLessThanOrEqual(1);
  });

  test(`P467 at ${size} a member opens a channel, posts and makes an invite, all by tapping`, async ({ meet, secrets, world }) => {
    const made = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general', 'nets-demo'], voice: false, may_invite: true });
    const name = unique(size.slice(0, 3));
    const { page } = await meet(options);
    await page.goto(inviteUrl(world, made.code));
    await expectFits(page, 'the invite landing page');
    // An invite makes an account with a password (issue #713).
    await page.getByLabel('Your name').tap();
    await page.getByLabel('Your name').fill(name);
    await page.getByLabel('Choose a password').fill(accountPassword);
    await page.getByLabel('Type the password again').fill(accountPassword);
    await page.getByRole('button', { name: 'Create account', exact: true }).tap();
    await expect(statusLine(page)).toContainText(name);
    await expect(statusLine(page)).toContainText('Connected');
    await keepToken(page, secrets);
    await expectFits(page, 'the lobby of a new member');

    // Open a channel and back again, then post in it: each channel is chosen
    // in Channels, which returns to Chat.
    await tapChannel(page, 'nets-demo');
    await expect(page.getByRole('heading', { name: 'nets-demo' })).toBeVisible();
    await tapChannel(page, 'general');
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
    const body = `${name} posted by tapping`;
    const box = page.getByLabel('Message #general');
    await box.tap();
    await expect(box).toBeFocused();
    await box.fill(body);
    await page.getByRole('button', { name: 'Send' }).tap();
    await expect(messages(page).getByRole('listitem').filter({ hasText: body })).toHaveCount(1);
    await expect(messages(page).getByRole('listitem').filter({ hasText: body })).not.toContainText('Sending');
    await expectFits(page, 'a conversation after a post');

    // The invite panel: open it, pick a channel, make a link.
    const panel = page.getByRole('region', { name: 'Invites' });
    await panel.getByRole('button', { name: 'Invite someone' }).tap();
    await expect(panel.getByRole('button', { name: 'Make invite' })).toBeVisible();
    const channel = panel.getByRole('checkbox', { name: '#general' });
    await channel.tap();
    await expect(channel).toBeChecked();
    await panel.getByRole('button', { name: 'Make invite' }).tap();
    await expect(panel.getByRole('textbox', { name: 'Invite link' })).toBeVisible();
    await expect(panel.getByRole('list', { name: 'Your invites' }).getByRole('listitem')).toHaveCount(1);
    await expectFits(page, 'the invite panel with a new link');
    await panel.getByRole('button', { name: 'Done' }).tap();
    await expect(panel.getByRole('textbox', { name: 'Invite link' })).toHaveCount(0);
  });

  // Issue #601: the channel row shows there are more channels than fit, and
  // the buttons that are only an icon, and the audience select, are big
  // enough for a finger. A new person, made through the API rather than by an
  // invite (redeeming counts against the network's tries, which 13-sign-in-
  // link needs), in channels of their own, so the shared fixture people's
  // lists are left as they are. They may invite, so Invite someone is shown.
  test(`P601 at ${size} the navigation, the top bar's buttons and the audience select are finger-sized, and more channels than fit are a column to scroll`, async ({ meet, secrets, world }) => {
    const tag = `p601-${size.slice(0, 3)}-${String(Date.now())}`;
    const op = world.tokens.operator;
    const person: CreatePrincipalRequest = { kind: 'human', name: tag };
    const id = (await ok(world, op, 'POST', '/v0/principals', createPrincipalResponse, person)).principal.id;
    const label: CreateCredentialRequestV1 = { label: tag };
    const { token } = await ok(world, op, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, label);
    secrets.push(token);
    const mayInvite: InvitePermissionRequestV1 = { may_invite: true };
    await done(world, op, 'PUT', `/v1/principals/${String(id)}/invite-permission`, mayInvite);
    const names = Array.from({ length: 8 }, (_, i) => `${tag}-${String(i + 1)}`);
    for (const channel of names) {
      await done(world, op, 'POST', '/v0/channels', { name: channel });
      await done(world, op, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`);
    }
    const { page } = await meet(options);
    await signIn(page, world, token, tag);
    await expect(page.getByLabel('To:')).toBeVisible();

    for (const [what, target] of [
      ['Members', page.getByRole('button', { name: 'Members' })],
      ['Settings', page.getByRole('button', { name: 'Settings' })],
      ['Invite someone', page.getByRole('region', { name: 'Invites' }).getByRole('button', { name: 'Invite someone' })],
      ['the audience select', page.getByLabel('To:')],
    ] as const) {
      const box = await target.boundingBox();
      if (box === null) throw new Error(`${what} has no box on the screen`);
      const where = `${what} is ${String(box.width)}×${String(box.height)}`;
      expect(box.width, where).toBeGreaterThanOrEqual(44);
      expect(box.height, where).toBeGreaterThanOrEqual(44);
    }
    await expectFits(page, 'the lobby with more channels than fit');

    // The navigation's three buttons are finger-sized too, and the channels
    // are the Channels view: a column of full-width rows (issue #929, which
    // replaced the sideways row and its fade), none beyond the screen's edge,
    // each reachable by scrolling the view.
    for (const view of ['Chat', 'Channels', 'Voice'] as const) {
      const box = await mainNav(page).getByRole('button', { name: view, exact: true }).boundingBox();
      if (box === null) throw new Error(`${view} has no box on the screen`);
      expect(box.width, `${view} is ${String(box.width)}×${String(box.height)}`).toBeGreaterThanOrEqual(44);
      expect(box.height, `${view} is ${String(box.width)}×${String(box.height)}`).toBeGreaterThanOrEqual(44);
    }
    await showView(page, 'Channels');
    const screen = page.viewportSize()?.width ?? 0;
    let left: number | null = null;
    for (const name of names) {
      const row = channelButton(page, name);
      await row.scrollIntoViewIfNeeded();
      await expect(row).toBeInViewport();
      const box = await row.boundingBox();
      if (box === null) throw new Error(`${name} has no box on the screen`);
      const where = `${name} is at x ${String(box.x)}, ${String(box.width)}×${String(box.height)}`;
      expect(box.height, where).toBeGreaterThanOrEqual(44);
      expect(box.x + box.width, where).toBeLessThanOrEqual(screen);
      left ??= box.x;
      expect(Math.abs(box.x - left), `${where}: the rows are one column`).toBeLessThanOrEqual(1);
    }
    await expectFits(page, 'the Channels view with more channels than fit');
  });
}

// tapChannel opens a channel with touch, as a phone does: Channels, the
// channel, and Chat is back.
async function tapChannel(page: Page, name: string): Promise<void> {
  await mainNav(page).getByRole('button', { name: 'Channels', exact: true }).tap();
  await channelButton(page, name).tap();
  await expect(mainNav(page).getByRole('button', { name: 'Chat', exact: true })).toHaveAttribute('aria-current', 'page');
}

// expectOnScreen checks that an element, scrolled into view, lies wholly
// inside the window. A box that starts left of the window's edge cannot be
// scrolled to, so expectFits alone does not see it.
async function expectOnScreen(page: Page, element: Locator, what: string): Promise<void> {
  await element.scrollIntoViewIfNeeded();
  const box = await element.boundingBox();
  const screen = page.viewportSize();
  if (box === null || screen === null) throw new Error(`${what} has no box on the screen`);
  const where = `${what} is at x ${String(box.x)}, y ${String(box.y)}, ${String(box.width)}×${String(box.height)} on a ${String(screen.width)}×${String(screen.height)} screen`;
  expect(box.x, where).toBeGreaterThanOrEqual(0);
  expect(box.y, where).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width, where).toBeLessThanOrEqual(screen.width);
  expect(box.y + box.height, where).toBeLessThanOrEqual(screen.height);
}

// expectInside checks that an element lies wholly inside the visible box of
// the log that scrolls it, once the page has settled.
async function expectInside(page: Page, element: Locator, log: Locator, what: string): Promise<void> {
  await frames(page);
  const box = await element.boundingBox();
  const frame = await log.boundingBox();
  if (box === null || frame === null) throw new Error(`${what} has no box on the screen`);
  const where = `${what} is at y ${String(box.y)}, ${String(box.height)} high, in a log from y ${String(frame.y)}, ${String(frame.height)} high`;
  expect(box.y, where).toBeGreaterThanOrEqual(frame.y - 1);
  expect(box.y + box.height, where).toBeLessThanOrEqual(frame.y + frame.height + 1);
}

// firstVisible is the first message whose bottom is below the log's top: its
// text and where its top is on the screen.
async function firstVisible(log: Locator): Promise<{ readonly text: string; readonly top: number }> {
  return log.evaluate((el) => {
    const top = el.getBoundingClientRect().top;
    for (const li of el.querySelectorAll('li')) {
      const box = li.getBoundingClientRect();
      if (box.bottom > top) return { text: li.textContent, top: box.top };
    }
    throw new Error('no message is in view');
  });
}

// frames waits for two animation frames: a resize and the scroll it causes
// are both drawn by then.
async function frames(page: Page): Promise<void> {
  await page.evaluate(
    () =>
      new Promise<void>((done) => {
        requestAnimationFrame(() => {
          requestAnimationFrame(() => {
            done();
          });
        });
      }),
  );
}

// done makes an operator's call that must succeed and whose reply is not
// needed.
async function done(world: World, token: string, method: 'POST' | 'PUT', path: string, body?: unknown): Promise<void> {
  const reply = await call(world, token, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
}

// keepToken hands the credential the invite gave this page to the fixture's
// end-of-test scan, as 11-invites does.
async function keepToken(page: Page, secrets: string[]): Promise<void> {
  const token = await page.evaluate(() => window.localStorage.getItem('conch.web.token'));
  if (token === null) throw new Error('no credential was kept');
  secrets.push(token);
}

// Issue #1030: on a phone the top bar keeps the connection state whole, with no
// server address, for a member and for someone with four buttons (the
// notifications bell, Invite someone, Members and Settings); who they are is in
// the Settings button's name.
for (const [size, options] of phoneSizes.filter(([name]) => name === '320×640' || name === '390×844')) {
  for (const inviter of [false, true]) {
    test(`P1030 at ${size} the top bar of ${inviter ? 'someone who invites' : 'a member'} shows the state whole and no address`, async ({ meet, secrets, world }) => {
      const tag = `p1030-${size.slice(0, 3)}-${inviter ? 'op' : 'mem'}-${String(Date.now())}`;
      const op = world.tokens.operator;
      let token = world.tokens.browser;
      let name = 'browser-test';
      if (inviter) {
        const person: CreatePrincipalRequest = { kind: 'human', name: tag };
        const id = (await ok(world, op, 'POST', '/v0/principals', createPrincipalResponse, person)).principal.id;
        const label: CreateCredentialRequestV1 = { label: tag };
        token = (await ok(world, op, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, label)).token;
        secrets.push(token);
        const mayInvite: InvitePermissionRequestV1 = { may_invite: true };
        await done(world, op, 'PUT', `/v1/principals/${String(id)}/invite-permission`, mayInvite);
        await done(world, op, 'POST', '/v0/channels', { name: tag });
        await done(world, op, 'PUT', `/v1/channels/${encodeURIComponent(tag)}/members/${String(id)}`);
        name = tag;
      }
      const { page } = await meet(options);
      await signIn(page, world, token, name);
      await expect(page.getByLabel('To:')).toBeVisible();
      await expect(statusLine(page)).not.toContainText(world.origin);
      await expect(page.getByRole('button', { name: `Settings, signed in as ${name}` })).toBeVisible();
      await expectStateReadable(page, 'Connected');
    });
  }
}
