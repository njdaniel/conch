// UX-02: one person's read mark is shared by all their clients, and a channel
// read in one clears its count in the other once that one reads again.
import { post, readState } from './api';
import { channelButton, expect, signIn, test } from './fixtures';

test('UX-02 a channel read in one client clears its count in the same person\'s other client after its next read', async ({ meet, world }) => {
  // guest-test says something in nets-demo that nick-test has not read.
  const sent = await post(world, world.tokens.guest, 'nets-demo', 'ux-02 something nick-test has not read');

  // nick-test in two clients, in two contexts, both on general (03 revokes the second credential).
  const first = (await meet()).page;
  const second = (await meet()).page;
  await signIn(first, world, world.tokens.nick, 'nick-test');
  await signIn(second, world, world.tokens.nick, 'nick-test');

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

  // The second client has not read again: its count is as it was.
  await expect(badgeOf(second, 'nets-demo')).toBeVisible();
  // Opening another channel is its next read.
  await channelButton(second, 'test-private').click();
  await expect(badgeOf(second, 'nets-demo')).toHaveCount(0);
});
