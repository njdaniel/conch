// A8 (issue #440): invites in the browser. The operator makes invites through
// the API, as with curl or conch; each person opens the link in a browser of
// their own, at desktop and phone sizes. This spec runs after the others but
// 12-phone, which counts nobody, so the people it adds to the fixture channels
// touch no other spec.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { invitesOf, makeInvite, post } from './api';
import { channelButton, expect, expectFits, fillAccount, showView, statusLine, test } from './fixtures';
import { joinSettingsResponseV1, type PutJoinSettingsRequestV1 } from '../src/schema/guest';
import { call, inviteUrl, ok, principalId, type World } from './world';

const desktop: BrowserContextOptions = { viewport: { width: 1280, height: 800 } };
const phone: BrowserContextOptions = { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true };
const sizes = [
  ['desktop', desktop],
  ['phone', phone],
] as const;

const goneHeading = "This link can't be used";
const noLongerWorks = 'Ask the person who sent it for a new one. If you already joined from this link, sign in instead.';

const unique = (label: string) => `a8-${label}-${String(Date.now())}`;
const channelButtons = (page: Page) => page.getByRole('navigation', { name: 'Channels' }).getByRole('button');

// join chooses a name and a password on the landing page (issue #713: the
// invite makes an account through POST /v2/invites/redeem) and waits for the
// lobby.
async function join(page: Page, name: string): Promise<void> {
  await fillAccount(page, name);
  await expect(statusLine(page)).toContainText(name);
  await expect(statusLine(page)).toContainText('Connected');
}

// keepToken reads the credential an invite gave this page and hands it to
// the fixture's end-of-test scan, so it is looked for in every page beside the
// world's.
async function keepToken(page: Page, secrets: string[]): Promise<string> {
  const token = await page.evaluate(() => window.localStorage.getItem('conch.web.token'));
  if (token === null) throw new Error('no credential was kept');
  secrets.push(token);
  return token;
}

// expectCodeGone checks that the code is nowhere in the page: not in the
// address, the page, the storage or the addresses it asked for, and not in any
// entry of the back history, each of which is visited. Then the page opens the
// client again (signed in, when it was), so the fixture's scan at the end
// looks at a live page rather than at where going back left it.
//
// One record keeps it, and no script can clear it: the browser's navigation
// timing entry names the address the document was loaded from, fragment and
// all, for as long as the document lives. Only this page's own scripts can
// read it (script-src 'self'), and a reload replaces it. It is checked to be
// that one entry and nothing else.
async function expectCodeGone(page: Page, inviteCode: string, world: World, signedIn: boolean): Promise<void> {
  const where = () =>
    page.evaluate(() =>
      [
        location.href,
        document.documentElement.outerHTML,
        JSON.stringify(performance.getEntries().filter((e) => e.entryType !== 'navigation').map((e) => e.name)),
        ...[window.localStorage, window.sessionStorage].flatMap((s) => Object.keys(s).map((k) => `${k}=${s.getItem(k) ?? ''}`)),
      ].join('\n'),
    );
  expect(await where(), 'the code in the address, the page, the storage or performance.getEntries()').not.toContain(inviteCode);
  const entries = await page.evaluate(() => history.length);
  for (let i = 1; i < entries; i++) {
    await page.goBack();
    expect(page.url(), `back ${String(i)}`).not.toContain(inviteCode);
  }
  await page.goto(world.appUrl);
  if (signedIn) await expect(statusLine(page)).toContainText('Connected');
  else await expect(page.getByLabel('Credential')).toBeVisible();
}

for (const [size, options] of sizes) {
  test(`A8 an operator's invite is opened (${size}), a name chosen, and the lobby shows only its channels`, async ({ meet, secrets, world }) => {
    const made = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general'], voice: false });
    const name = unique(size);
    const { page } = await meet(options);
    await page.goto(inviteUrl(world, made.code));

    await expect(page.getByRole('heading', { name: 'You are invited to Conch' })).toBeVisible();
    const gives = page.getByLabel('What the invite gives');
    await expect(gives).toContainText('p3-operator');
    await expect(gives).toContainText('#general');
    await expect(gives).toContainText('Not included');
    await expect(page.getByText(world.origin)).toBeVisible();
    // Out of the address as soon as the page read it.
    expect(page.url()).not.toContain(made.code);
    expect(page.url()).not.toContain('#');
    // The landing page fits the screen: nothing wider than the window.
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    expect(await page.evaluate(() => window.innerWidth)).toBe(options.viewport?.width);

    await join(page, name);
    await showView(page, 'Channels');
    await expect(channelButton(page, 'general')).toBeVisible();
    await expect(channelButtons(page)).toHaveCount(1);
    await showView(page, 'Chat');
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
    // A joiner the operator did not let invite sees no invite controls.
    await expect(page.getByRole('button', { name: 'Invite someone' })).toHaveCount(0);
    // The server agrees: the new person is a member, not an operator, with
    // the password chosen on the page (POST /v2/invites/redeem, issue #713).
    const token = await keepToken(page, secrets);
    const who = await call(world, token, 'GET', '/v1/whoami');
    expect(who.body).toMatchObject({ name, role: 'member', identity_class: 'member', auth: { password_set: true } });
    await expectCodeGone(page, made.code, world, true);
  });
}

// Issue #585 (F07): a joiner whose invite left voice out is told so, not that
// voice could not be read, and the page asks nothing of voice: a socket conchd
// refuses before the upgrade would be retried for ever.
test('A8 a joiner without voice is told so, and the page asks for no voice route', async ({ meet, secrets, world }) => {
  const made = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general'], voice: false });
  const { page } = await meet(desktop);
  const asked: string[] = [];
  const voiceRoute = /\/v1\/channels\/[^/]+\/voice(\/|$|\?)|\/v1\/voice\//;
  page.on('request', (r) => {
    if (voiceRoute.test(new URL(r.url()).pathname)) asked.push(r.url());
  });
  page.on('websocket', (ws) => {
    if (voiceRoute.test(new URL(ws.url()).pathname)) asked.push(ws.url());
  });
  await page.goto(inviteUrl(world, made.code));
  await join(page, unique('no-voice'));
  await keepToken(page, secrets);
  const voice = page.getByRole('region', { name: 'Voice' });
  await expect(voice).toContainText('Your account does not include voice. Ask whoever invited you.');
  await expect(voice).not.toContainText('Voice status could not be read.');
  await expect(voice.getByRole('button')).toHaveCount(0);
  // Long enough for a retried socket to show: the feed's first wait is 1 s.
  await new Promise((r) => setTimeout(r, 3000));
  expect(asked, 'requests to the voice routes').toEqual([]);
});

test('A8 a personal link opened again says it can no longer be used', async ({ meet, secrets, world }) => {
  const made = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general'], voice: false });
  const first = await meet(desktop);
  await first.page.goto(inviteUrl(world, made.code));
  await join(first.page, unique('once'));
  await keepToken(first.page, secrets);

  const again = await meet(phone);
  await again.page.goto(inviteUrl(world, made.code));
  await expect(again.page.getByRole('heading', { name: goneHeading })).toBeVisible();
  await expect(again.page.getByRole('alert')).toHaveText(noLongerWorks);
  await expect(again.page.getByLabel('Your name')).toHaveCount(0);
  // Nobody is signed in here: the one button is Sign in, for someone who
  // joined from the link already and has a password (issue #713); the
  // credential form is not on this page (issue #593).
  await expect(again.page.getByRole('button')).toHaveText(['Sign in']);
  await expect(again.page.getByLabel('Credential')).toHaveCount(0);
  await expectCodeGone(again.page, made.code, world, false);
});

test('A8 a joiner who may invite makes a link that another browser redeems, then closes one', async ({ meet, secrets, world }) => {
  const made = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general', 'nets-demo'], voice: false, may_invite: true });
  const inviterName = unique('inviter');
  const inviter = await meet({ ...desktop, permissions: ['clipboard-read', 'clipboard-write'] });
  const page = inviter.page;
  await page.goto(inviteUrl(world, made.code));
  await join(page, inviterName);
  await expect(channelButtons(page)).toHaveCount(2);
  await keepToken(page, secrets);

  // A member gets personal invites only.
  const panel = page.getByRole('region', { name: 'Invites' });
  await panel.getByRole('button', { name: 'Invite someone' }).click();
  await expect(panel.getByRole('radio')).toHaveCount(0);
  await expect(panel.getByText('Joiners may invite others')).toHaveCount(0);
  await panel.getByRole('checkbox', { name: '#general' }).check();
  await panel.getByRole('button', { name: 'Make invite' }).click();
  const shown = panel.getByRole('textbox', { name: 'Invite link' });
  await expect(shown).toBeVisible();
  // A personal link says it lets in one person (issue #593).
  const box = panel.getByRole('group', { name: 'New invite link' });
  await expect(box).toContainText('The first person to open this link joins, and then it stops working.');
  await expect(box).not.toContainText('Anyone with this link');
  await panel.getByRole('button', { name: 'Copy link' }).click();
  await expect(panel.getByText('Copied.')).toBeVisible();
  const link = await page.evaluate(() => navigator.clipboard.readText());
  expect(link).toBe(await shown.inputValue());
  expect(link).toContain('/app/#/invite/conch_inv_');
  const list = panel.getByRole('list', { name: 'Your invites' });
  await expect(list.getByRole('listitem')).toHaveCount(1);
  await expect(list.getByRole('listitem')).toContainText('Open');
  await expect(panel.getByText(/^Read at \d\d:\d\d$/)).toBeVisible();

  // Someone else, on a phone: the inviter's name is taken, another is not.
  const joiner = await meet(phone);
  await joiner.page.goto(link);
  await expect(joiner.page.getByLabel('What the invite gives')).toContainText(inviterName);
  await fillAccount(joiner.page, inviterName);
  await expect(joiner.page.getByRole('alert')).toHaveText('That name is taken. Choose another.');
  await expect(joiner.page.getByLabel('Your name')).toHaveValue(inviterName);
  await join(joiner.page, unique('joiner'));
  await showView(joiner.page, 'Channels');
  await expect(channelButton(joiner.page, 'general')).toBeVisible();
  await expect(channelButtons(joiner.page)).toHaveCount(1);
  const inviteCode = decodeURIComponent(link.slice(link.indexOf('#/invite/') + '#/invite/'.length));
  await keepToken(joiner.page, secrets);
  await expectCodeGone(joiner.page, inviteCode, world, true);

  // The inviter's list says it was used. A second one is made and closed.
  await panel.getByRole('button', { name: 'Refresh' }).click();
  await expect(list.getByRole('listitem')).toContainText('Used');
  await expect(list.getByRole('button', { name: 'Close' })).toHaveCount(0);

  // The used link, opened in a fresh browser where nobody is signed in: the
  // dead-invite page, with Sign in its one button (issue #713). It
  // previews and does not redeem, so it spends nothing of the per-address
  // redeem limit the sign-in link spec counts on.
  const late = await meet(desktop);
  await late.page.goto(link);
  await expect(late.page.getByRole('heading', { level: 1 })).toHaveText(goneHeading);
  await expect(late.page.getByRole('alert')).toHaveText(noLongerWorks);
  await expect(late.page.getByRole('button')).toHaveText(['Sign in']);
  await expect(late.page.getByLabel('Credential')).toHaveCount(0);
  await expectCodeGone(late.page, inviteCode, world, false);

  await panel.getByRole('button', { name: 'Done' }).click();
  await expect(shown).toHaveCount(0);
  await panel.getByRole('button', { name: 'Make invite' }).click();
  await expect(shown).toBeVisible();
  await expect(list.getByRole('listitem')).toHaveCount(2);
  const open = list.getByRole('listitem').filter({ hasText: 'Open' });
  await expect(open).toHaveCount(1);
  await open.getByRole('button', { name: 'Close' }).click();
  await expect(list.getByRole('listitem').first()).toContainText('Closed');
  await expect(list.getByRole('button', { name: 'Close' })).toHaveCount(0);
});

// The test does not sign out: that would revoke the world's operator
// credential. It adds the operator to general, which the panel offers, since
// an operator's own channels are the ones the page lists. It runs at phone
// size, where the lobby fits the screen (issue #467), and checks that the
// panel, with every control an operator gets, fits too.
test('A8 an operator makes an open link with its defaults, and closes it', async ({ meet, world }) => {
  const operatorId = await principalId(world, world.tokens.operator);
  const added = await call(world, world.tokens.operator, 'PUT', `/v1/channels/general/members/${String(operatorId)}`);
  expect(added.status).toBeLessThan(300);

  const { page } = await meet(phone);
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');

  const panel = page.getByRole('region', { name: 'Invites' });
  await panel.getByRole('button', { name: 'Invite someone' }).click();
  await panel.getByRole('radio', { name: 'Open link: anyone with it' }).check();
  await expect(panel.getByRole('combobox')).toHaveValue('24 hours');
  await expect(panel.getByRole('checkbox', { name: 'Limit the number of people' })).not.toBeChecked();
  await expect(panel.getByRole('checkbox', { name: 'Joiners may invite others' })).not.toBeChecked();
  await panel.getByRole('checkbox', { name: '#general' }).check();
  // With a cap's field shown too, then without it again.
  await panel.getByRole('checkbox', { name: 'Limit the number of people' }).check();
  await expect(panel.getByRole('spinbutton')).toBeVisible();
  await expectFits(page, 'the operator\'s invite form');
  await panel.getByRole('checkbox', { name: 'Limit the number of people' }).uncheck();
  const before = Date.now();
  await panel.getByRole('button', { name: 'Make invite' }).click();
  await expect(panel.getByRole('textbox', { name: 'Invite link' })).toBeVisible();

  // What the server made: open, uncapped, no right to invite, a day long.
  const mine = (await invitesOf(world, world.tokens.operator)).filter((i) => i.created_by === operatorId && i.kind === 'open');
  const open = mine[0];
  if (open === undefined) throw new Error('the operator has no open link');
  expect(open.max_uses).toBeUndefined();
  expect(open.may_invite).toBe(false);
  expect(open.voice).toBe(false);
  expect(open.channels.map((c) => c.name)).toEqual(['general']);
  const lifetime = Date.parse(open.expires_at) - before;
  expect(lifetime).toBeGreaterThan(24 * 60 * 60 * 1000 - 60_000);
  expect(lifetime).toBeLessThan(24 * 60 * 60 * 1000 + 60_000);

  const list = panel.getByRole('list', { name: 'Your invites' });
  const item = list.getByRole('listitem').filter({ hasText: 'Open link' }).filter({ hasText: 'Open ·' });
  await expect(item).toHaveCount(1);
  await expectFits(page, 'the operator\'s invite panel with a new link');
  await item.getByRole('button', { name: 'Close' }).click();
  await expect(list.getByRole('listitem').filter({ hasText: 'Open link' }).first()).toContainText('Closed');
  const closed = (await invitesOf(world, world.tokens.operator)).find((i) => i.id === open.id);
  expect(closed?.closed_at).toBeDefined();
});

// Issue #1034 (browser UX audit 3, F9): a newcomer who makes an account at the
// bare address is admitted to a channel under a history bound, so a channel
// with messages in it reads empty to them. The page says what is true: it
// does not claim the channel is empty. (People an operator adds, and those a
// personal invite admits, read all of the channel's history and never see this
// state in a channel that has messages.) At the narrowest width. The joining
// settings are put back when the test is done.
test('F9 a newcomer admitted under a history bound is told what is not shown, not that the channel is empty', async ({ meet, secrets, world }) => {
  const channel = `f9-channel-a-${String(Date.now())}`;
  const op = world.tokens.operator;
  const created = await call(world, op, 'POST', '/v0/channels', { name: channel });
  expect(created.status).toBe(201);
  await post(world, op, channel, 'f9 sent before the newcomer came');
  await ok(world, op, 'PUT', '/v1/join/settings', joinSettingsResponseV1, { open_joining: 'account', open_channels: [channel] } satisfies PutJoinSettingsRequestV1);
  try {
    const { page } = await meet({ viewport: { width: 320, height: 640 } });
    await page.goto(world.appUrl);
    await page.getByRole('region', { name: 'Join' }).getByRole('button', { name: 'Create an account' }).click();
    await join(page, unique('f9'));
    await keepToken(page, secrets);
    // At this width one view shows at a time: choose the channel.
    await showView(page, 'Channels');
    await channelButton(page, channel).click();
    const log = page.getByRole('log', { name: 'Messages' });
    await expect(log.getByText('No messages here that you can see yet.')).toBeVisible();
    await expect(log.getByText("Messages sent before you joined aren't shown.")).toBeVisible();
    await expect(log.getByText('Write a message to start.')).toBeVisible();
    await expect(page.getByText(/Say hello|No messages in #/)).toHaveCount(0);
  } finally {
    await ok(world, op, 'PUT', '/v1/join/settings', joinSettingsResponseV1, { open_joining: 'off', open_channels: [] } satisfies PutJoinSettingsRequestV1);
  }
});
