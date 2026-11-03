// A8 (issues #440, #990): member links in the browser. The operator makes links
// through the API, as with curl or conch; each person opens the link in a
// browser of their own, at desktop and phone sizes. This spec runs after the others but
// 12-phone, which counts nobody, so the people it adds to the fixture channels
// touch no other spec.
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { makeMemberLink, memberLinksOf, post, withDefaultChannels } from './api';
import { channelButton, expect, expectFits, fillAccount, showView, signIn, statusLine, test } from './fixtures';
import { joinSettingsResponseV1, type PutJoinSettingsRequestV1 } from '../src/schema/guest';
import { call, inviteUrl, ok, principalId, setChannelVisibility, type World } from './world';

const desktop: BrowserContextOptions = { viewport: { width: 1280, height: 800 } };
const phone: BrowserContextOptions = { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true };
const sizes = [
  ['desktop', desktop],
  ['phone', phone],
] as const;

const goneHeading = "This link can't be used";
const noLongerWorks = 'Ask the person who sent it for a new one. If you already joined from this link, sign in instead.';

const unique = (label: string) => `a8-${label}-${String(Date.now())}`;
// The channels' own buttons, not the ⋯ of a row's menu (issue #1135).
const channelButtons = (page: Page) => page.getByRole('navigation', { name: 'Channels' }).getByRole('button', { name: /^(?!Actions for |Browse channels$|New channel$)/ });

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
  test(`A8 a member link is opened (${size}), a name chosen, and the lobby shows only the default channels`, async ({ meet, secrets, world }) => {
    await withDefaultChannels(world, ['general'], async () => {
      const made = await makeMemberLink(world, world.tokens.operator);
      const name = unique(size);
      const { page } = await meet(options);
      await page.goto(inviteUrl(world, made.code));

      await expect(page.getByRole('heading', { name: 'You are invited to Conch' })).toBeVisible();
      const gives = page.getByLabel('What the invite gives');
      await expect(gives).toContainText('p3-operator');
      await expect(gives).toContainText('#general');
      // Every joiner has voice, so the page has no voice row to show.
      await expect(gives).not.toContainText('Voice');
      // An operator's default link expires (7 days): the page says when.
      await expect(gives.locator('time')).toBeVisible();
      await expect(page.getByText(world.origin)).toBeVisible();
      // Never the guest flow.
      await expect(page.getByRole('button', { name: /guest/i })).toHaveCount(0);
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
      // Every member may make member links, a joiner included (issue #986):
      // the brake is on until an operator turns it off.
      await expect(page.getByRole('button', { name: 'Invite someone' })).toHaveCount(1);
      // The server agrees: the new person is a member, not an operator, with
      // the password chosen on the page (POST /v2/invites/redeem, issue #713).
      const token = await keepToken(page, secrets);
      const who = await call(world, token, 'GET', '/v1/whoami');
      expect(who.body).toMatchObject({ name, role: 'member', identity_class: 'member', auth: { password_set: true } });
      await expectCodeGone(page, made.code, world, true);
    });
  });
}

// Issue #585 (F07): a member without voice is told so, not that voice could
// not be read, and the page asks nothing of voice: a socket conchd refuses
// before the upgrade would be retried for ever. Every joiner has voice
// (issue #986), so the operator takes it away (issue #984), and the member
// signs in on a second browser, whose every request is watched.
test('A8 a joiner without voice is told so, and the page asks for no voice route', async ({ meet, secrets, world }) => {
  await withDefaultChannels(world, ['general'], async () => {
    const made = await makeMemberLink(world, world.tokens.operator);
    const { page } = await meet(desktop);
    await page.goto(inviteUrl(world, made.code));
    const name = unique('no-voice');
    await join(page, name);
    const token = await keepToken(page, secrets);
    const who = await call(world, token, 'GET', '/v1/whoami');
    const id = (who.body as { id: number }).id;
    const off = await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(id)}/voice-permission`, { voice_allowed: false });
    expect(off.status, JSON.stringify(off.body)).toBeLessThan(300);

    const second = (await meet(desktop)).page;
    const asked: string[] = [];
    const voiceRoute = /\/v1\/channels\/[^/]+\/voice(\/|$|\?)|\/v1\/voice\//;
    second.on('request', (r) => {
      if (voiceRoute.test(new URL(r.url()).pathname)) asked.push(r.url());
    });
    second.on('websocket', (ws) => {
      if (voiceRoute.test(new URL(ws.url()).pathname)) asked.push(ws.url());
    });
    await signIn(second, world, token, name);
    const voice = second.getByRole('region', { name: 'Voice' });
    await expect(voice).toContainText('Your account does not include voice. Ask whoever invited you.');
    await expect(voice).not.toContainText('Voice status could not be read.');
    await expect(voice.getByRole('button')).toHaveCount(0);
    // Long enough for a retried socket to show: the feed's first wait is 1 s.
    await new Promise((r) => setTimeout(r, 3000));
    expect(asked, 'requests to the voice routes').toEqual([]);
  });
});

// Issue #1082 (audit 4 F5): a name the server's rules refuse is said in the
// page's own words, never the server's "Principal name ..." line.
test('F5 a name with an invisible character gets a plain line on the name field', async ({ meet, world }) => {
  const made = await makeMemberLink(world, world.tokens.operator);
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await page.goto(inviteUrl(world, made.code));
  await fillAccount(page, 'ali​ce');
  await expect(page.getByText('A name cannot contain invisible characters.')).toBeVisible();
  await expect(page.getByLabel('Your name')).toBeFocused();
  await expect(page.locator('body')).not.toContainText(/principal|schema:/i);
});

test('A8 a one-use link opened again says it can no longer be used', async ({ meet, secrets, world }) => {
  await withDefaultChannels(world, ['general'], async () => {
    const made = await makeMemberLink(world, world.tokens.operator, { kind: 'member', max_uses: 1 });
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
});

// A member's own links (issue #990): made in the panel with a use limit,
// opened in another browser (the inviter's name is taken, another is not),
// listed with who joined, used up, and a second made and closed. A member is
// offered no "Never expires" and no way to disable the people a link brought in.
test('A8 a joiner makes a link that another browser redeems, sees who joined, then closes one', async ({ meet, secrets, world }) => {
  await withDefaultChannels(world, ['general'], async () => {
    const made = await makeMemberLink(world, world.tokens.operator);
    const inviterName = unique('inviter');
    const inviter = await meet({ ...desktop, permissions: ['clipboard-read', 'clipboard-write'] });
    const page = inviter.page;
    await page.goto(inviteUrl(world, made.code));
    await join(page, inviterName);
    await expect(channelButtons(page)).toHaveCount(1);
    await keepToken(page, secrets);

    const panel = page.getByRole('region', { name: 'Invites' });
    await panel.getByRole('button', { name: 'Invite someone' }).click();
    // A member's form: no kind, no channels, no voice, and no "Never expires".
    await expect(panel.getByRole('radio')).toHaveCount(0);
    await expect(panel.getByRole('checkbox', { name: /^#/ })).toHaveCount(0);
    await expect(panel.getByText('Joiners may invite others')).toHaveCount(0);
    await expect(panel.getByRole('combobox')).toHaveValue('7 days');
    await expect(panel.getByRole('combobox').locator('option')).toHaveText(['1 day', '7 days', '30 days']);
    await panel.getByRole('checkbox', { name: 'Limit the number of people' }).check();
    await panel.getByRole('spinbutton').fill('1');
    await panel.getByRole('button', { name: 'Make link' }).click();
    const shown = panel.getByRole('textbox', { name: 'Invite link' });
    await expect(shown).toBeVisible();
    const box = panel.getByRole('group', { name: 'New invite link' });
    await expect(box).toContainText('Anyone with this link can join');
    await panel.getByRole('button', { name: 'Copy link' }).click();
    await expect(panel.getByText('Copied.')).toBeVisible();
    const link = await page.evaluate(() => navigator.clipboard.readText());
    expect(link).toBe(await shown.inputValue());
    expect(link).toContain('/app/#/invite/conch_inv_');
    const list = panel.getByRole('list', { name: 'Your links' });
    await expect(list.getByRole('listitem')).toHaveCount(1);
    await expect(list.getByRole('listitem')).toContainText('Open');
    await expect(list.getByRole('listitem')).toContainText('0 of 1 joined');
    await expect(panel.getByText(/^Read at \d\d:\d\d$/)).toBeVisible();
    // The server made it as asked: a member's link, one use, the default 7 days.
    const mine = (await memberLinksOf(world, world.tokens.operator)).filter((i) => i.created_by_name === inviterName);
    expect(mine).toHaveLength(1);
    expect(mine[0]?.max_uses).toBe(1);
    const lifetime = Date.parse(mine[0]?.expires_at ?? '') - Date.now();
    expect(lifetime).toBeGreaterThan(7 * 24 * 60 * 60 * 1000 - 5 * 60_000);
    expect(lifetime).toBeLessThan(7 * 24 * 60 * 60 * 1000 + 60_000);

    // Someone else, on a phone: the inviter's name is taken, another is not.
    const joiner = await meet(phone);
    await joiner.page.goto(link);
    await expect(joiner.page.getByLabel('What the invite gives')).toContainText(inviterName);
    await fillAccount(joiner.page, inviterName);
    await expect(joiner.page.getByRole('alert')).toHaveText('That name is taken. Choose another.');
    await expect(joiner.page.getByLabel('Your name')).toHaveValue(inviterName);
    const joinerName = unique('joiner');
    await join(joiner.page, joinerName);
    await showView(joiner.page, 'Channels');
    await expect(channelButton(joiner.page, 'general')).toBeVisible();
    await expect(channelButtons(joiner.page)).toHaveCount(1);
    const inviteCode = decodeURIComponent(link.slice(link.indexOf('#/invite/') + '#/invite/'.length));
    await keepToken(joiner.page, secrets);
    await expectCodeGone(joiner.page, inviteCode, world, true);

    // The inviter's list says it was used up, and who joined.
    await panel.getByRole('button', { name: 'Refresh' }).click();
    await expect(list.getByRole('listitem')).toContainText('Used up');
    await expect(list.getByRole('listitem')).toContainText('1 of 1 joined');
    await expect(list.getByRole('button', { name: 'Close' })).toHaveCount(0);
    await list.getByRole('button', { name: 'Joiners (1)' }).click();
    await expect(panel.getByRole('list', { name: 'Joiners of this link' })).toContainText(joinerName);
    // Members get no control to disable the people a link brought in (O2).
    await expect(panel.getByRole('button', { name: /disable/i })).toHaveCount(0);

    // The used link, opened in a fresh browser where nobody is signed in: the
    // dead-link page, with Sign in its one button (issue #713). It previews
    // and does not redeem, so it spends nothing of the per-address redeem
    // limit the sign-in link spec counts on.
    const late = await meet(desktop);
    await late.page.goto(link);
    await expect(late.page.getByRole('heading', { level: 1 })).toHaveText(goneHeading);
    await expect(late.page.getByRole('alert')).toHaveText(noLongerWorks);
    await expect(late.page.getByRole('button')).toHaveText(['Sign in']);
    await expect(late.page.getByLabel('Credential')).toHaveCount(0);
    await expectCodeGone(late.page, inviteCode, world, false);

    await panel.getByRole('button', { name: 'Hide joiners' }).click();
    await panel.getByRole('button', { name: 'Done' }).click();
    await expect(shown).toHaveCount(0);
    await panel.getByRole('button', { name: 'Make link' }).click();
    await expect(shown).toBeVisible();
    await expect(list.getByRole('listitem')).toHaveCount(2);
    const open = list.getByRole('listitem').filter({ hasText: 'Open' });
    await expect(open).toHaveCount(1);
    await open.getByRole('button', { name: 'Close' }).click();
    await expect(list.getByRole('listitem').first()).toContainText('Closed');
    await expect(list.getByRole('button', { name: 'Close' })).toHaveCount(0);
  });
});

// A member whose right to make links an operator turned off is told why and
// is offered no form (issue #990); the server refuses the call all the same.
test('A8 a member whose right to make links is off is told why and offered no form', async ({ meet, secrets, world }) => {
  await withDefaultChannels(world, ['general'], async () => {
    const made = await makeMemberLink(world, world.tokens.operator);
    const { page } = await meet(desktop);
    await page.goto(inviteUrl(world, made.code));
    await join(page, unique('brake'));
    const token = await keepToken(page, secrets);
    const id = (((await call(world, token, 'GET', '/v1/whoami')).body ?? {}) as { id: number }).id;
    const off = await call(world, world.tokens.operator, 'PUT', `/v1/principals/${String(id)}/invite-permission`, { may_invite: false });
    expect(off.status, JSON.stringify(off.body)).toBeLessThan(300);

    // Signed in afresh, the page reads the right as off.
    await page.reload();
    await expect(statusLine(page)).toContainText('Connected');
    const panel = page.getByRole('region', { name: 'Invites' });
    await panel.getByRole('button', { name: 'Invite someone' }).click();
    await expect(panel.getByText('You may not make invite links. An operator has turned that off for you.')).toBeVisible();
    await expect(panel.getByRole('button', { name: 'Make link' })).toHaveCount(0);
    expect((await call(world, token, 'POST', '/v2/invites', { kind: 'member' })).status).toBe(403);
  });
});

// The operator's panel, at phone size, where the lobby fits the screen (issue
// #467): "Never expires" is offered to operators alone, the default is the
// server's 7 days, and the panel fits with every control an operator gets. The
// test does not sign out: that would revoke the world's operator credential.
test('A8 an operator makes a link with its defaults and one that never expires, and closes them', async ({ meet, world }) => {
  const operatorId = await principalId(world, world.tokens.operator);
  const { page } = await meet(phone);
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');

  const panel = page.getByRole('region', { name: 'Invites' });
  await panel.getByRole('button', { name: 'Invite someone' }).click();
  await expect(panel.getByRole('combobox')).toHaveValue('7 days');
  await expect(panel.getByRole('combobox').locator('option')).toHaveText(['1 day', '7 days', '30 days', 'Never expires']);
  await expect(panel.getByRole('checkbox', { name: 'Limit the number of people' })).not.toBeChecked();
  // With a cap's field shown too, then without it again.
  await panel.getByRole('checkbox', { name: 'Limit the number of people' }).check();
  await expect(panel.getByRole('spinbutton')).toBeVisible();
  await expectFits(page, "the operator's link form");
  await panel.getByRole('checkbox', { name: 'Limit the number of people' }).uncheck();
  const before = Date.now();
  await panel.getByRole('button', { name: 'Make link' }).click();
  await expect(panel.getByRole('textbox', { name: 'Invite link' })).toBeVisible();

  // What the server made: uncapped, a week long.
  const first = (await memberLinksOf(world, world.tokens.operator)).filter((i) => i.created_by === operatorId)[0];
  if (first === undefined) throw new Error('the operator has no member link');
  expect(first.max_uses).toBeUndefined();
  const lifetime = Date.parse(first.expires_at ?? '') - before;
  expect(lifetime).toBeGreaterThan(7 * 24 * 60 * 60 * 1000 - 60_000);
  expect(lifetime).toBeLessThan(7 * 24 * 60 * 60 * 1000 + 60_000);

  // Then one that never expires.
  await panel.getByRole('button', { name: 'Done' }).click();
  await panel.getByRole('combobox').selectOption('Never expires');
  await panel.getByRole('button', { name: 'Make link' }).click();
  await expect(panel.getByRole('textbox', { name: 'Invite link' })).toBeVisible();
  const never = (await memberLinksOf(world, world.tokens.operator)).filter((i) => i.created_by === operatorId && i.expires_at === undefined)[0];
  if (never === undefined) throw new Error('the operator has no link that never expires');

  const list = panel.getByRole('list', { name: 'Your links' });
  const item = list.getByRole('listitem').filter({ hasText: 'never expires' });
  await expect(item).toHaveCount(1);
  await expectFits(page, "the operator's link panel with a new link");
  await item.getByRole('button', { name: 'Close' }).click();
  await expect(list.getByRole('listitem').filter({ hasText: 'never expires' }).first()).toContainText('Closed');
  const closed = (await memberLinksOf(world, world.tokens.operator)).find((i) => i.id === never.id);
  expect(closed?.closed_at).toBeDefined();
  // The first link is the newest open one left: closed too, so the world is as
  // it was. (Other specs' links of the operator stay as they are.)
  await list.getByRole('listitem').filter({ hasText: 'Open' }).first().getByRole('button', { name: 'Close' }).click();
  await expect.poll(async () => (await memberLinksOf(world, world.tokens.operator)).find((i) => i.id === first.id)?.closed_at).toBeDefined();
});

// Issue #987 (open sign-up), replacing #1034's F9 check: a newcomer who makes
// an account at the bare address is the member a member link makes, with no
// history bound, so a default channel with messages in it shows them its
// earlier messages, not an empty log. (The page's words for a channel whose
// earlier messages are hidden are unit-tested, src/chat.test.tsx: no route
// makes such a member now.) At the narrowest width. The joining settings are
// put back when the test is done.
test('F9 a newcomer from open sign-up reads the default channel\'s earlier messages', async ({ meet, secrets, world }) => {
  const channel = `f9-channel-a-${String(Date.now())}`;
  const op = world.tokens.operator;
  const created = await call(world, op, 'POST', '/v0/channels', { name: channel });
  expect(created.status).toBe(201);
  await post(world, op, channel, 'f9 sent before the newcomer came');
  // A default channel must be public (issue #987).
  await setChannelVisibility(world, channel, 'public');
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
    await expect(log.getByText('f9 sent before the newcomer came')).toBeVisible();
    await expect(log.getByText("Messages sent before you joined aren't shown.")).toHaveCount(0);
  } finally {
    await ok(world, op, 'PUT', '/v1/join/settings', joinSettingsResponseV1, { open_joining: 'off', open_channels: [] } satisfies PutJoinSettingsRequestV1);
    await setChannelVisibility(world, channel, 'private');
  }
});

// Issue #1083 (browser UX audit 4, F6): pausing every invite asks first, and
// says what it stops. Cancel and Escape send nothing; the yes pauses, and an
// invite then answers paused. Un-ticking resumes at once. At both sizes; on a
// phone the buttons are at least 44 px tall. The invites are put back.
for (const [size, options] of sizes) {
  test(`F6 at ${size} pausing all invites asks first, and an invite then answers paused`, async ({ meet, world }) => {
    const op = world.tokens.operator;
    const isPhone = options === phone;
    const paused = async () => (await call(world, op, 'GET', '/v1/invites/settings')).body as { paused?: boolean } | null;
    const attempt = () => call(world, op, 'POST', '/v2/invites', { kind: 'member' });
    try {
      const { page } = await meet(options);
      await page.goto(world.appUrl);
      await page.getByLabel('Credential').fill(op);
      await page.getByRole('button', { name: 'Use credential' }).click();
      await page.getByRole('button', { name: 'Continue anyway' }).click();
      await expect(statusLine(page)).toContainText('Connected');
      await page.getByRole('button', { name: 'Settings' }).click();
      // On a phone the administration sections are a list (issue #1018).
      if (isPhone) await page.getByRole('button', { name: 'Server', exact: true }).tap();
      await page.getByRole('button', { name: 'Joining' }).click();
      const region = page.getByRole('region', { name: 'Joining' });
      const box = region.getByLabel('Pause all invites');
      const press = async (target: Locator) => (isPhone ? target.tap() : target.click());
      await expect(box).toBeEnabled();

      // Ticking asks, with Cancel focused; nothing is sent and the box stays unticked.
      await press(box);
      const question = region.getByRole('group', { name: /^Pause every invite\?/ });
      await expect(question).toContainText('No one can make, preview or use an invite until you switch this back, including links already sent.');
      await expect(question.getByRole('button', { name: 'Cancel' })).toBeFocused();
      await expect(box).not.toBeChecked();
      if (isPhone) {
        for (const name of ['Yes, pause', 'Cancel']) expect((await question.getByRole('button', { name }).boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);
      }
      expect((await paused())?.paused).toBe(false);
      await page.keyboard.press('Escape');
      await expect(question).toHaveCount(0);
      await expect(box).toBeFocused();
      await press(box);
      await press(question.getByRole('button', { name: 'Cancel' }));
      await expect(question).toHaveCount(0);
      await expect(box).not.toBeChecked();
      expect((await paused())?.paused).toBe(false);

      // Yes pauses, and an invite answers paused.
      await press(box);
      await press(question.getByRole('button', { name: 'Yes, pause' }));
      await expect(box).toBeChecked();
      await expect(region.getByText('Invites are paused: no one can make, preview or use an invite.')).toBeVisible();
      expect((await paused())?.paused).toBe(true);
      const refused = await attempt();
      expect(refused.status).toBe(503);
      expect(JSON.stringify(refused.body)).toContain('invites_paused');

      // Un-ticking resumes at once, with no question.
      await press(box);
      await expect(box).not.toBeChecked();
      await expect(question).toHaveCount(0);
      expect((await paused())?.paused).toBe(false);
    } finally {
      await call(world, op, 'PUT', '/v1/invites/settings', { paused: false });
    }
  });
}
