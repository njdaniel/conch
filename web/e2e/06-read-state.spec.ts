// UX-02: one person's read mark is shared by all their clients, and a channel
// read in one clears its count in the other through the events socket.
import { post, readState } from './api';
import { channelButton, expect, signIn, test } from './fixtures';
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
