// Issue #467: the lobby fits a phone. At each phone size the lobby, a
// conversation and the panels beside it are no wider than the window, and a
// flow driven by taps alone lands where it is aimed. The voice panel, which
// needs LiveKit to show its controls, is tried at phone size in 10-voice; the
// operator's invite panel in 11-invites. This spec runs after both, so the
// person it adds to the fixture channels touches no other spec.
import type { Page } from '@playwright/test';
import { makeInvite, post } from './api';
import { channelButton, conversation, expect, expectFits, messages, phones, signIn, statusLine, test } from './fixtures';
import { inviteUrl } from './world';

// Things a phone's width finds first: a word with no place to break, an
// address, and a line long enough to wrap many times. Each size posts its own.
const longWord = (size: string) => `p467-${size}-${'w'.repeat(160)}`;
const longAddress = (size: string) => `https://example.com/p467/${size}/${'segment/'.repeat(20)}end`;
const longLine = (size: string) => `p467 ${size} ${'a line that goes on for a while, '.repeat(12)}and ends`;

const unique = (label: string) => `p467-${label}-${String(Date.now())}`;

for (const [size, options] of phones) {
  test(`P467 at ${size} the lobby, a conversation, the account form and the members panel fit the screen`, async ({ meet, world }) => {
    for (const body of [longWord(size), longAddress(size), longLine(size)]) await post(world, world.tokens.nick, 'general', body);
    const { page } = await meet(options);
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
    await expect(messages(page).getByText(longWord(size))).toBeVisible();
    await expect(messages(page).getByText(longAddress(size))).toBeVisible();
    await expect(statusLine(page)).toContainText(world.origin);
    // On the shared server the voice section says voice is not set up.
    await expect(page.getByRole('region', { name: 'Voice' })).toContainText('Voice is not set up on this server');
    await expectFits(page, 'the lobby with general open');

    // The whisper picker lays its people out in the composer.
    await page.getByLabel('To:').selectOption({ label: 'Whisper to…' });
    await expect(conversation(page).getByRole('group', { name: 'Whisper to' })).toBeVisible();
    await expectFits(page, 'the composer with the whisper picker open');
    await page.getByLabel('To:').selectOption({ label: '#general' });

    await page.getByRole('button', { name: 'Change your name' }).tap();
    await expect(page.getByLabel('New name')).toBeVisible();
    await expectFits(page, 'the lobby with the account form open');
    await page.getByRole('button', { name: 'Change your name' }).tap();

    // The members panel starts closed on a narrow screen.
    const members = page.getByRole('button', { name: 'Members' });
    await expect(members).toHaveAttribute('aria-expanded', 'false');
    await members.tap();
    await expect(page.getByRole('region', { name: 'Members list' })).toBeVisible();
    await expectFits(page, 'the lobby with the members panel open');
  });

  test(`P467 at ${size} a member opens a channel, posts and makes an invite, all by tapping`, async ({ meet, secrets, world }) => {
    const made = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general', 'nets-demo'], voice: false, may_invite: true });
    const name = unique(size.slice(0, 3));
    const { page } = await meet(options);
    await page.goto(inviteUrl(world, made.code));
    await expectFits(page, 'the invite landing page');
    await page.getByLabel('Your name').tap();
    await page.getByLabel('Your name').fill(name);
    await page.getByRole('button', { name: 'Join' }).tap();
    await expect(statusLine(page)).toContainText(name);
    await expect(statusLine(page)).toContainText('Connected');
    await keepToken(page, secrets);
    await expectFits(page, 'the lobby of a new member');

    // Open a channel and back again, then post in it.
    await channelButton(page, 'nets-demo').tap();
    await expect(page.getByRole('heading', { name: 'nets-demo' })).toBeVisible();
    await channelButton(page, 'general').tap();
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
}

// keepToken hands the credential the invite gave this page to the fixture's
// end-of-test scan, as 11-invites does.
async function keepToken(page: Page, secrets: string[]): Promise<void> {
  const token = await page.evaluate(() => window.localStorage.getItem('conch.web.token'));
  if (token === null) throw new Error('no credential was kept');
  secrets.push(token);
}
