// UX-02: one person's read mark is shared by all their clients, and a channel
// read in one clears its count in the other through the events socket.
import { makeChannel, memberOf, post, readState } from './api';
import { channelButton, expect, expectFits, messages, openChannel, signIn, test } from './fixtures';
import { call, principalId } from './world';

test('UX-02 a channel read in one client clears its count in the same person\'s other client live', async ({ meet, world }) => {
  // jamie-test says something in nets-demo that nick-test has not read.
  const sent = await post(world, world.tokens.guest, 'nets-demo', 'ux-02 something nick-test has not read');

  // nick-test in two clients, in two contexts, both on general (03 revokes the second credential).
  const first = (await meet()).page;
  const second = (await meet()).page;
  let roster = false;
  second.on('websocket', (socket) => {
    socket.on('framereceived', (frame) => {
      if (String(frame.payload).includes('conch.session_roster.v1')) roster = true;
    });
  });
  await signIn(first, world, world.tokens.nick, 'nick-test');
  await signIn(second, world, world.tokens.nick, 'nick-test');
  await expect.poll(() => roster).toBe(true);

  // The badge sits beside the channel's button, in the same list item.
  const badgeOf = (page: typeof first, channel: string) => channelButton(page, channel).locator('xpath=..').getByRole('img', { name: / unread$/ });
  await expect(badgeOf(first, 'nets-demo')).toBeVisible();
  await expect(badgeOf(second, 'nets-demo')).toBeVisible();
  // general is open and read in both.
  await expect(badgeOf(first, 'general')).toHaveCount(0);

  // The first client opens nets-demo, reading to its newest message.
  await channelButton(first, 'nets-demo').click();
  await expect(badgeOf(first, 'nets-demo')).toHaveCount(0);
  await expect
    .poll(async () => (await readState(world, world.tokens.nick)).find((s) => s.last_read_id >= sent.id && s.unread === 0 && s.channel_id === sent.channel_id)?.channel_id, {
      message: 'the mark of nick-test in nets-demo has reached the message and nothing is unread',
    })
    .toBe(sent.channel_id);

  // The second client clears through the shared-mark event, without another read.
  await expect(badgeOf(second, 'nets-demo')).toHaveCount(0);
});

test('UX-02 a non-open channel updates live, suppresses own posts and scoped messages, and shares read marks', async ({ meet, world }) => {
  const first = (await meet()).page;
  const second = (await meet()).page;
  const reads: string[] = [];
  let roster = false;
  second.on('request', (request) => {
    const url = new URL(request.url());
    if (url.pathname === '/v1/read-state') reads.push(request.url());
  });
  second.on('websocket', (socket) => {
    socket.on('framereceived', (frame) => {
      if (String(frame.payload).includes('conch.session_roster.v1')) roster = true;
    });
  });
  await signIn(first, world, world.tokens.nick, 'nick-test');
  await signIn(second, world, world.tokens.nick, 'nick-test');
  await expect.poll(() => roster).toBe(true);
  await expect.poll(() => reads.length).toBeGreaterThan(0);
  const badge = channelButton(second, 'nets-demo').locator('xpath=..').getByRole('img', { name: / unread$/ });
  await expect(badge).toHaveCount(0);
  reads.length = 0;

  await post(world, world.tokens.guest, 'nets-demo', 'ux-02 live one');
  await post(world, world.tokens.guest, 'nets-demo', 'ux-02 live two');
  await expect(badge).toHaveText('2');
  const channelId = (await readState(world, world.tokens.nick)).find((state) => state.unread === 2)?.channel_id;
  expect(channelId).toBeDefined();
  expect(reads).toHaveLength(0);

  await post(world, world.tokens.nick, 'nets-demo', 'ux-02 my own non-open post');
  const nick = await principalId(world, world.tokens.nick);
  const observer = await principalId(world, world.tokens.observer);
  for (const recipient of [observer, nick]) {
    const reply = await call(world, world.tokens.guest, 'POST', '/v2/channels/nets-demo/messages', {
      body: recipient === nick ? 'ux-02 whisper to the reader' : 'ux-02 whisper to someone else',
      audience: { kind: 'principals', principal_ids: [recipient] },
    });
    expect(reply.status).toBe(201);
  }
  await expect(badge).toHaveText('3');
  expect(reads).toHaveLength(0);

  await channelButton(first, 'nets-demo').click();
  await expect(badge).toHaveCount(0);
  await second.reload();
  await expect(badge).toHaveCount(0);
});


for (const width of [1440, 390]) {
  test(`opening unread position loads across pages and focuses the first visible unread at ${String(width)}px`, async ({ meet, world }) => {
    const channel = `unread-${String(width)}`;
    await makeChannel(world, channel);
    const nick = await principalId(world, world.tokens.nick);
    const guest = await principalId(world, world.tokens.guest);
    const observer = await principalId(world, world.tokens.observer);
    for (const id of [nick, guest, observer]) await memberOf(world, channel, id, true);
    const mark = await post(world, world.tokens.guest, channel, 'already read');
    const marked = await call(world, world.tokens.nick, 'PUT', `/v1/channels/${channel}/read-state`, { last_read_id: mark.id });
    expect(marked.status).toBe(200);
    await post(world, world.tokens.nick, channel, 'my own post after the mark');
    const hidden = await call(world, world.tokens.guest, 'POST', `/v2/channels/${channel}/messages`, {
      body: 'hidden whisper after the mark',
      audience: { kind: 'principals', principal_ids: [observer] },
    });
    expect(hidden.status).toBe(201);
    const firstUnread = await post(world, world.tokens.guest, channel, 'first visible unread');
    for (let i = 0; i < 110; i++) await post(world, world.tokens.guest, channel, `later unread ${String(i)}`);
    const state = (await readState(world, world.tokens.nick)).find((s) => s.channel_id === mark.channel_id);
    expect(state?.unread).toBe(111);

    const page = (await meet({ viewport: { width, height: 844 }, hasTouch: width < 700 })).page;
    await signIn(page, world, world.tokens.nick, 'nick-test');
    await openChannel(page, channel);
    await expect(messages(page).getByRole('separator', { name: 'New messages' })).toHaveCount(0);
    const jump = page.getByRole('button', { name: 'Jump to first unread' });
    await expect(jump).toBeVisible();
    await jump.focus();
    await page.keyboard.press('Enter');
    const row = messages(page).locator(`:scope > .messages > [data-message-id="${String(firstUnread.id)}"]`);
    await expect(row).toBeFocused();
    const divider = messages(page).getByRole('separator', { name: 'New messages' });
    await expect(divider).toHaveCount(1);
    await expect(divider).toBeInViewport();
    await expect(jump).toHaveCount(0);
    await expect(messages(page).getByText('hidden whisper after the mark')).toHaveCount(0);
    await expectFits(page, 'unread jump');
    await expect(divider).not.toHaveAttribute('tabindex');
    await expect(divider.locator('xpath=..')).not.toHaveAttribute('data-message-id');
    await expect(row).toBeFocused();
    await post(world, world.tokens.guest, channel, 'new live post keeps the opening divider');
    await expect(messages(page).getByText('new live post keeps the opening divider')).toHaveCount(1);
    await expect(divider).toHaveCount(1);
  });
}

// The read state can answer after the open channel's history has been drawn
// at the newest message (CI is slow enough for that). The divider then lands
// above the view; it must not push the newest message below the log's end, nor
// scroll to itself: only Jump to first unread does that.
test('a divider placed after the history keeps a reader at the newest message', async ({ meet, world }) => {
  const mark = await post(world, world.tokens.guest, 'general', 'late divider already read');
  const marked = await call(world, world.tokens.nick, 'PUT', '/v1/channels/general/read-state', { last_read_id: mark.id });
  expect(marked.status).toBe(200);
  const bodies = Array.from({ length: 40 }, (_, i) => `late divider unread ${String(i + 1)}`);
  for (const body of bodies) await post(world, world.tokens.guest, 'general', body);

  const page = (await meet({ viewport: { width: 360, height: 740 }, hasTouch: true })).page;
  let release = (): void => undefined;
  const held = new Promise<void>((done) => { release = done; });
  await page.route('**/v1/read-state', async (route) => {
    await held;
    await route.continue();
  });
  await signIn(page, world, world.tokens.nick, 'nick-test');
  const log = messages(page);
  const newest = log.getByRole('listitem').filter({ hasText: bodies[bodies.length - 1] ?? '' });
  await expect(newest).toBeVisible();
  const divider = log.getByRole('separator', { name: 'New messages' });
  await expect(divider).toHaveCount(0);
  release();
  await expect(divider).toHaveCount(1);
  const gap = await log.evaluate((el) => new Promise<number>((done) => {
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        done(el.scrollHeight - el.scrollTop - el.clientHeight);
      });
    });
  }));
  expect(gap, 'the log is still at its end once the divider is placed').toBeLessThanOrEqual(1);
  await expect(newest).toBeInViewport();
  await expect(divider).not.toBeInViewport();
  await expect(page.getByRole('button', { name: 'Jump to first unread' })).toBeVisible();
});
