import { history, makeChannel, memberOf, post, reply } from './api';
import { credentialField, expect, signIn, test } from './fixtures';
import { call, principalId } from './world';
import { hashForMessage } from '../src/route';

test('message links page back, open threads, and give one unavailable notice', async ({ meet, world }) => {
  const channel = 'message-links';
  await makeChannel(world, channel);
  for (const token of [world.tokens.nick, world.tokens.guest, world.tokens.observer]) {
    await memberOf(world, channel, await principalId(world, token), true);
  }
  const old = await post(world, world.tokens.guest, channel, 'old linked message');
  const response = await reply(world, world.tokens.guest, channel, old.id, 'linked thread reply');
  const whisper = await call(world, world.tokens.guest, 'POST', `/v2/channels/${channel}/messages`, {
    body: 'private linked whisper', audience: { kind: 'principals', principal_ids: [await principalId(world, world.tokens.observer)] },
  });
  expect(whisper.status).toBe(201);
  // The server response uses the generated message schema through api.history;
  // the sender can see this whisper while nick cannot.
  const hidden = (await history(world, world.tokens.guest, channel)).find((message) => message.body === 'private linked whisper');
  expect(hidden).toBeDefined();
  for (let i = 0; i < 110; i++) await post(world, world.tokens.guest, channel, `later ${String(i)}`);
  const recentReply = await reply(world, world.tokens.guest, channel, old.id, 'recent linked reply to older root');
  const page = (await meet()).page;
  await signIn(page, world, world.tokens.nick, 'nick-test');
  await page.goto(world.appUrl + hashForMessage(channel, old.id));
  const row = page.locator(`.message-linked[data-message-id="${String(old.id)}"]`);
  await expect(row).toBeFocused();
  await expect(row).toBeInViewport();
  await page.goto(world.appUrl + hashForMessage(channel, response.id));
  await expect(page.locator(`.thread-view .message-linked[data-message-id="${String(response.id)}"]`)).toBeFocused();
  await page.goto(world.appUrl + hashForMessage(channel, recentReply.id));
  await expect(page.locator('.thread-view .reply-composer textarea')).toBeVisible();
  const recentRow = page.locator(`.thread-view .message-linked[data-message-id="${String(recentReply.id)}"]`);
  await expect(recentRow).toBeFocused();
  await expect(recentRow).toBeInViewport();
  for (const id of [hidden?.id ?? 0, 9007199254740991]) {
    await page.goto(world.appUrl + hashForMessage(channel, id));
    await expect(page.getByText("This message isn't available", { exact: true })).toBeVisible();
    await expect(page.getByText('private linked whisper', { exact: true })).toHaveCount(0);
  }
  for (const destination of ['#/c/general/m/', '#/c/general/m/1/extra']) {
    await page.goto(world.appUrl + destination);
    await expect(page.getByText("This message isn't available", { exact: true })).toBeVisible();
  }
});

test('keyboard copy actions restore menu-button focus', async ({ meet, world }) => {
  const message = await post(world, world.tokens.guest, 'general', 'keyboard copy body');
  const { page } = await meet({ permissions: ['clipboard-read', 'clipboard-write'] });
  await signIn(page, world, world.tokens.nick, 'nick-test');
  await page.goto(world.appUrl + hashForMessage('general', message.id));
  const row = page.locator(`.message-linked[data-message-id="${String(message.id)}"]`);
  await expect(row).toBeFocused();
  const button = row.getByRole('button', { name: /More actions for/ });
  await button.focus();
  for (const action of ['Copy link', 'Copy text']) {
    await page.keyboard.press('Enter');
    const entry = page.getByRole('menuitem', { name: action, exact: true });
    await page.keyboard.press('End');
    if (action === 'Copy link') await page.keyboard.press('ArrowUp');
    await expect(entry).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(row.getByText(action === 'Copy link' ? 'Link copied.' : 'Text copied.', { exact: true })).toBeVisible();
    await expect(button).toBeFocused();
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(action === 'Copy link'
      ? world.appUrl + hashForMessage('general', message.id) : 'keyboard copy body');
  }
});

test('a signed-out message link survives sign-in', async ({ meet, world }) => {
  const message = await post(world, world.tokens.guest, 'general', 'link after sign-in');
  const page = (await meet()).page;
  await page.goto(world.appUrl + hashForMessage('general', message.id));
  await (await credentialField(page)).fill(world.tokens.nick);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await expect(page.locator(`.message-linked[data-message-id="${String(message.id)}"]`)).toBeFocused();
});
