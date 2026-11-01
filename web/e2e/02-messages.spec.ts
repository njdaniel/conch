// P3-02: a message is shown once to everyone who may see it, and the history
// agrees with what the pages show.
import type { Page } from '@playwright/test';
import { history, post } from './api';
import { createCredentialResponseV1, createPrincipalResponse } from '../src/schema/credential';
import { channelButton, expect, messages, openChannel, say, signIn, test } from './fixtures';
import { call, ok, type World } from './world';

// settled waits until the page has caught up with the server: a message posted
// now is on the page, so the replay that follows a reload has landed too. A
// count taken before that could be one of the history alone.
async function settled(world: World, page: Page, sentinel: string): Promise<void> {
  await post(world, world.tokens.nick, 'general', sentinel);
  await expect(messages(page).getByText(sentinel)).toHaveCount(1);
}

test('P3-02 a message sent in the browser appears once in both contexts and once in the history', async ({ meet, world }) => {
  const browser = (await meet()).page;
  const guest = (await meet()).page;
  await signIn(browser, world, world.tokens.browser, 'browser-test');
  await signIn(guest, world, world.tokens.guest, 'jamie-test');
  await expect(channelButton(guest, 'general')).toBeVisible();

  const body = 'p3-02 from the browser, sent by a person';
  await say(browser, 'general', body);

  await expect(messages(guest).getByText(body)).toHaveCount(1);
  // The sender's own pending line counts as one before the server has answered;
  // look again now that a second context has the stored message, so that a
  // duplicate from the reply and the socket frame would show.
  await expect(messages(browser).getByText(body)).toHaveCount(1);

  const stored = (await history(world, world.tokens.nick, 'general')).filter((m) => m.body === body);
  expect(stored).toHaveLength(1);

  // Still once after a reload: the history read and the live feed do not add up
  // to two.
  await browser.reload();
  await settled(world, browser, 'p3-02 sentinel after the first reload');
  await expect(messages(browser).getByText(body)).toHaveCount(1);
  await expect(messages(guest).getByText(body)).toHaveCount(1);
});

test('P3-02 a message posted through the API as nick-test appears once in the browser', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');

  const body = 'p3-02 from nick-test, posted through the API';
  await post(world, world.tokens.nick, 'general', body);

  await expect(messages(page).getByText(body)).toHaveCount(1);
  await expect(messages(page).getByRole('listitem').filter({ hasText: body })).toContainText('nick-test');

  await page.reload();
  await settled(world, page, 'p3-02 sentinel after the second reload');
  await expect(messages(page).getByText(body)).toHaveCount(1);
  expect((await history(world, world.tokens.browser, 'general')).filter((m) => m.body === body)).toHaveLength(1);
});

test('F14 a typed draft survives a reload, and sending it leaves an empty box after the next', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const box = page.getByLabel('Message #general');
  await box.fill('a draft I was typing');

  await page.reload();
  await expect(page.getByLabel('Message #general')).toHaveValue('a draft I was typing');

  const body = 'f14 sent after a reload';
  await page.getByLabel('Message #general').fill(body);
  await page.getByRole('button', { name: 'Send' }).click();
  await expect(messages(page).getByText(body)).toHaveCount(1);
  await page.reload();
  await expect(page.getByLabel('Message #general')).toHaveValue('');
});

test('F26 a posted URL is a link that opens safely, and a javascript: string is not', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');

  await say(page, 'general', 'f26 read https://example.com/a?b=1.');
  const line = messages(page).getByRole('listitem').filter({ hasText: 'f26 read' });
  const link = line.getByRole('link', { name: 'https://example.com/a?b=1' });
  await expect(link).toHaveAttribute('href', 'https://example.com/a?b=1');
  await expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  await expect(link).toHaveAttribute('target', '_blank');
  await expect(line).toContainText('https://example.com/a?b=1.');

  await say(page, 'general', 'f26 never javascript:alert(1) or <b>tags</b>');
  const plain = messages(page).getByRole('listitem').filter({ hasText: 'f26 never' });
  await expect(plain).toContainText('javascript:alert(1) or <b>tags</b>');
  await expect(plain.getByRole('link')).toHaveCount(0);
});

// Issue #1040: the log is one tab stop, and its controls are named by author
// and time. A channel of its own, an owner and a member with a credential each.
async function logCommunity(world: World, secrets: string[], label: string) {
  const stamp = Date.now().toString(36);
  const channel = `e1040-${label}-${stamp}`;
  const done = async (method: 'POST' | 'PUT', path: string, body?: unknown) => {
    const reply = await call(world, world.tokens.operator, method, path, body);
    if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
  };
  await done('POST', '/v0/channels', { name: channel });
  const people: { name: string; token: string }[] = [];
  for (const base of ['owner', 'member-1']) {
    const name = `${base}-${stamp}`;
    const made = await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, { kind: 'human', name });
    const issued = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, createCredentialResponseV1, { label: name });
    secrets.push(issued.token);
    await done('PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(made.principal.id)}`);
    people.push({ name, token: issued.token });
  }
  const [owner, member] = people as [(typeof people)[number], (typeof people)[number]];
  return { channel, owner, member };
}

test('F15 with 30 messages held the log is one tab stop, and no control is named by an id', async ({ meet, secrets, world }) => {
  const { channel, owner, member } = await logCommunity(world, secrets, 'stop');
  for (let i = 1; i <= 30; i++) await post(world, i % 3 === 0 ? member.token : owner.token, channel, `f15 message ${String(i)}`);
  // The newest is the owner's, so the member's stop has one control: Reply.
  await post(world, owner.token, channel, 'f15 newest');
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await signIn(page, world, member.token, member.name);
  await openChannel(page, channel);
  await expect(messages(page).getByText('f15 newest')).toHaveCount(1);

  await page.locator('#conversation-heading').focus();
  const composer = page.getByLabel(`Message #${channel}`);
  const seen: string[] = [];
  let presses = 0;
  while (presses < 12 && !(await composer.evaluate((el) => el === document.activeElement))) {
    await page.keyboard.press('Tab');
    presses++;
    seen.push(await page.evaluate(() => {
      const el = document.activeElement;
      return (el?.closest('[data-message-id]')?.getAttribute('data-message-id') ?? '-') + ':' + (el?.getAttribute('aria-label') ?? el?.textContent ?? '');
    }));
  }
  // Search, the newest message, its Reply, the composer's Whisper and Attach,
  // then its box: six presses, not one per message. The log itself costs two,
  // the newest message and its one control, whatever history is held.
  expect(presses, seen.join(' | ')).toBeLessThanOrEqual(6);
  const inLog = seen.map((where) => where.split(':')[0]).filter((id) => id !== '-');
  expect(inLog, seen.join(' | ')).toHaveLength(2);
  expect(new Set(inLog).size, seen.join(' | ')).toBe(1);

  const names = await messages(page).evaluate((log) => [...log.querySelectorAll('[aria-label]')].map((el) => el.getAttribute('aria-label') ?? ''));
  expect(names.length).toBeGreaterThan(0);
  for (const name of names) expect(name).not.toMatch(/message \d/);
  await expect(messages(page).getByRole('button', { name: /^Reply to owner-.+'s message, \d\d:\d\d$/ }).first()).toBeAttached();
});

test('F15 arrow keys move between messages, and a keyboard alone edits and replies', async ({ meet, secrets, world }) => {
  const { channel, owner, member } = await logCommunity(world, secrets, 'keys');
  await post(world, owner.token, channel, 'f15 first from the owner');
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await signIn(page, world, member.token, member.name);
  await openChannel(page, channel);
  await say(page, channel, 'f15 mine to edit');
  const items = messages(page).locator('.messages > li');
  await expect(items).toHaveCount(2);

  // Tab into the log lands on the newest message; ArrowUp and Home go up.
  await page.locator('#conversation-heading').focus();
  await page.keyboard.press('Tab');
  await page.keyboard.press('Tab');
  await expect(items.nth(1)).toBeFocused();
  await page.keyboard.press('ArrowUp');
  await expect(items.nth(0)).toBeFocused();
  await page.keyboard.press('ArrowDown');
  await expect(items.nth(1)).toBeFocused();
  await page.keyboard.press('Home');
  await expect(items.nth(0)).toBeFocused();
  await page.keyboard.press('End');
  await expect(items.nth(1)).toBeFocused();
  await expect(items.nth(1)).toHaveCSS('outline-style', 'solid');

  // Edit, by keyboard alone: Tab to the ⋯, Enter, Enter on Edit, type, Enter.
  await page.keyboard.press('Tab');
  await expect(items.nth(1).getByRole('button', { name: /^More actions for member-1-.+'s message, \d\d:\d\d$/ })).toBeFocused();
  await page.keyboard.press('Enter');
  await page.keyboard.press('Enter');
  const box = items.nth(1).getByRole('textbox', { name: /^Edit member-1-.+'s message, \d\d:\d\d$/ });
  await expect(box).toBeFocused();
  await box.fill('f15 mine, edited');
  await box.press('Enter');
  await expect(items.nth(1)).toContainText('f15 mine, edited');
  await expect(items.nth(1).getByRole('button', { name: /^More actions for / })).toBeFocused();

  // Reply, by keyboard alone: up to the owner's message, Tab to Reply, Enter.
  await page.keyboard.press('Shift+Tab');
  await page.keyboard.press('Shift+Tab');
  await expect(items.nth(1)).toBeFocused();
  await page.keyboard.press('ArrowUp');
  await expect(items.nth(0)).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(items.nth(0).getByRole('button', { name: /^Reply to owner-.+'s message, \d\d:\d\d$/ })).toBeFocused();
  await page.keyboard.press('Enter');
  const thread = page.getByRole('region', { name: /^Thread around owner-.+'s message, \d\d:\d\d$/ });
  const reply = thread.getByRole('textbox', { name: /^Reply to owner-/ });
  await expect(reply).toBeFocused();
  await page.keyboard.type('f15 a reply by keyboard');
  await page.keyboard.press('Enter');
  await expect(thread).toContainText('f15 a reply by keyboard');
});
