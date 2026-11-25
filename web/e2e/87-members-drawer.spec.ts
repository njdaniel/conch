// Issue #2196: below the wide breakpoint the Members panel is drawn over the
// page with a backdrop, and it is a modal drawer: opened by keyboard, Tab and
// Shift+Tab never leave it (the rest of the page is inert, through the lobby's
// one inert owner), Escape closes it and focus returns to the Members button.
// At a wide window it is a side column and focus leaves it as before.
import type { Page } from '@playwright/test';
import { createChannelResponse } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse } from '../src/schema/credential';
import { runV1 } from '../src/schema/run';
import { openSession, post } from './api';
import { credentialField, expect, messages, openChannel, showView, signIn, statusLine, test } from './fixtures';
import { call, ok } from './world';

const members = (page: Page) => page.getByRole('button', { name: 'Members' });
const list = (page: Page) => page.getByRole('region', { name: 'Members list' });

// inDrawer says whether focus is inside the members panel now.
const inDrawer = (page: Page) => page.evaluate(() => document.getElementById('members-body')?.contains(document.activeElement) === true);

for (const [name, width, height] of [
  ['phone 390×844', 390, 844],
  ['tablet 1050×800', 1050, 800],
] as const) {
  test(`P2196 ${name}: Tab and Shift+Tab never leave the Members drawer, and Escape returns focus to Members`, async ({ meet, world }) => {
    const { page } = await meet({ viewport: { width, height } });
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await openChannel(page, 'general');
    await members(page).focus();
    await page.keyboard.press('Enter');
    await expect(list(page)).toBeFocused();
    // The drawer's stops, in Tab order from the list itself, until Tab comes
    // round to it again: focus reaches controls inside and wraps at both ends.
    const first = await page.evaluateHandle(() => document.activeElement);
    const stops = [first];
    for (let i = 0; i < 200; i++) {
      await page.keyboard.press('Tab');
      expect(await inDrawer(page), `Tab #${String(i + 1)} left the drawer, at ${await page.evaluate(() => document.activeElement?.outerHTML.slice(0, 120) ?? 'nothing')}`).toBe(true);
      const now = await page.evaluateHandle(() => document.activeElement);
      if (await page.evaluate(([a, b]) => a === b, [now, first] as const)) break;
      stops.push(now);
    }
    expect(stops.length, 'Tab reached a control inside the drawer').toBeGreaterThan(1);
    expect(stops.length, 'Tab came round to the list').toBeLessThan(200);
    // Tab from the last stop landed on the first (the loop's break), and
    // Shift+Tab from the first lands on the last.
    await page.keyboard.press('Shift+Tab');
    const last = stops[stops.length - 1];
    if (last === undefined) throw new Error('no stops');
    expect(await page.evaluate(([a, b]) => a === b, [await page.evaluateHandle(() => document.activeElement), last] as const), 'Shift+Tab from the first stop lands on the last').toBe(true);
    // And on, backwards, never out.
    for (let i = 0; i < 30; i++) {
      await page.keyboard.press('Shift+Tab');
      expect(await inDrawer(page), `Shift+Tab #${String(i + 1)} left the drawer`).toBe(true);
    }
    await page.keyboard.press('Escape');
    await expect(list(page)).toBeHidden();
    await expect(members(page)).toBeFocused();
  });
}

test('P2196 1440×900: the side column is not modal, and Shift+Tab leaves it', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await openChannel(page, 'general');
  await expect(list(page)).toBeVisible();
  await expect(page.locator('.centre')).not.toHaveAttribute('inert');
  await expect(page.locator('.topbar')).not.toHaveAttribute('inert');
  // Shift+Tab from the column's first stop goes to what precedes it on the
  // page (the column's length depends on how many people the world holds).
  await list(page).focus();
  await page.keyboard.press('Shift+Tab');
  expect(await inDrawer(page), 'Shift+Tab stayed in the side column').toBe(false);
  await expect(page.locator('body')).not.toBeFocused();
});

// Opening a run from the drawer closes it and shows the run in the centre. On a
// phone the centre is the Chat view's: from the Voice or Channels view the
// page goes to Chat first, so the run detail is visible and takes focus.
for (const from of ['Voice', 'Channels'] as const) {
  test(`P2196 phone 390×844: Open run from the drawer on the ${from} view shows the run detail in Chat, with focus`, async ({ meet, world }) => {
    const tag = `p2196-${String(Date.now())}`;
    const session = await openSession(world, world.tokens.worker, `${tag}-client`);
    const started = await ok(world, world.tokens.worker, 'POST', `/v1/sessions/${String(session.id)}/runs`, runV1, { channel: 'general', task: `${tag} a task` });
    const state = await call(world, world.tokens.worker, 'POST', `/v1/runs/${String(started.id)}/state`, { state: 'running' });
    expect(state.status).toBeLessThan(300);
    const { page } = await meet({ viewport: { width: 390, height: 844 }, hasTouch: true });
    await signIn(page, world, world.tokens.browser, 'browser-test');
    await openChannel(page, 'general');
    await showView(page, from);
    await members(page).focus();
    await page.keyboard.press('Enter');
    await list(page).getByRole('button', { name: 'Show sessions for worker-test' }).click();
    await list(page).getByRole('button', { name: new RegExp(`^Run #${String(started.id)} · `) }).click();
    await expect(list(page)).toBeHidden();
    const detail = page.getByRole('article', { name: 'Run detail' });
    await expect(detail).toBeVisible();
    await expect(page.getByRole('navigation', { name: 'Main navigation' }).getByRole('button', { name: 'Chat' })).toHaveAttribute('aria-current', 'page');
    // The run's heading takes focus, not the article around it.
    await expect(detail.getByRole('heading', { name: `Run #${String(started.id)}` })).toBeFocused();
  });
}

// Issue #2196, round 4. Nothing in the centre is made focusable by a press on
// it: a tabindex on the centre or the run detail would draw the focus ring
// around the whole column at the next key, and break PageUp in the log.
test('P2196 1440×900: pressing the centre does not focus it, and PageUp still scrolls the log', async ({ meet, world }) => {
  const channel = `p2196-${String(Date.now())}`;
  await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, { name: channel });
  await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(await principalId(world, 'browser'))}`);
  for (let i = 0; i < 40; i++) await post(world, world.tokens.operator, channel, `p2196 line ${String(i)}`);
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await openChannel(page, channel);
  const main = page.locator('main.centre');
  await expect(main).not.toHaveAttribute('tabindex');
  await expect(messages(page)).toContainText('p2196 line 39');
  const log = messages(page);
  await log.evaluate((el) => { el.scrollTop = el.scrollHeight; });
  // A press in the log's padding, then Shift (a key that types nothing).
  await log.click({ position: { x: 1, y: 1 } });
  await page.keyboard.press('Shift');
  expect(await page.evaluate(() => document.activeElement?.matches('main.centre') === true), 'the centre took focus').toBe(false);
  expect(await main.evaluate((el) => el.matches(':focus-visible')), 'the focus ring is drawn around the centre').toBe(false);
  const before = await log.evaluate((el) => el.scrollTop);
  await page.keyboard.press('PageUp');
  await expect.poll(() => log.evaluate((el) => el.scrollTop), 'PageUp scrolls the log').toBeLessThan(before);
});

async function principalId(world: Parameters<typeof call>[0], who: 'browser'): Promise<number> {
  const me = await call(world, world.tokens[who], 'GET', '/v1/whoami');
  const id = (me.body as { id?: number }).id;
  if (id === undefined) throw new Error('no id in whoami');
  return id;
}

// A row's menu is open in the drawer; the browser's own Tab leaves it, and
// focus goes where Tab goes, inside the drawer, not to the drawer's body.
test('P2196 tablet 1050×800: Tab and Shift+Tab out of a row menu stay in the drawer, where the browser puts them', async ({ meet, world }) => {
  const suffix = Date.now().toString(36).slice(-5);
  const channel = `p2196m-${suffix}`;
  const name = `q${String(Math.floor(Math.random() * 100000)).padStart(5, '0')}m`;
  await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, { name: channel });
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, { kind: 'human', name })).principal.id;
  await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, { label: name });
  expect((await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`)).status).toBeLessThan(300);
  for (const key of ['Tab', 'Shift+Tab']) {
    const { page } = await meet({ viewport: { width: 1050, height: 800 } });
    await page.goto(world.appUrl);
    await (await credentialField(page)).fill(world.tokens.operator);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await page.getByRole('button', { name: 'Continue anyway' }).click();
    await expect(statusLine(page)).toContainText('Connected');
    await openChannel(page, channel);
    await members(page).focus();
    await page.keyboard.press('Enter');
    const more = list(page).getByRole('button', { name: `Actions for ${name}` }).first();
    await more.focus();
    await page.keyboard.press('Enter');
    await expect(list(page).getByRole('menu')).toBeVisible();
    await page.keyboard.press(key);
    await expect(list(page).getByRole('menu')).toHaveCount(0);
    // Past the browser's own move and any recovery after it.
    await page.waitForTimeout(300);
    expect(await inDrawer(page), `${key} left the drawer`).toBe(true);
    // Where the browser puts Shift+Tab from the removed menu is the row's
    // button. Where it puts Tab is its own (the stop after the button, or the
    // wrap to the drawer's first stop when its starting point fell at the end),
    // so only that it did not leave the drawer is held, above.
    if (key === 'Shift+Tab') await expect(more).toBeFocused();
  }
});

// The invite panel is hidden, not closed, while the drawer is over the page:
// the link it just made, shown only once, is there when the drawer goes.
test('P2196 tablet 1050×800: an invite link just made survives opening and closing Members', async ({ meet, world }) => {
  const { page } = await meet({ viewport: { width: 1050, height: 800 } });
  await page.goto(world.appUrl);
  await (await credentialField(page)).fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
  const invites = page.getByRole('region', { name: 'Invites' });
  await invites.getByRole('button', { name: 'Invite someone' }).click();
  await invites.getByRole('button', { name: 'Make link' }).click();
  const link = invites.locator('input[aria-label="Invite link"]');
  await expect(link).toBeVisible();
  const url = await link.inputValue();
  await members(page).focus();
  await page.keyboard.press('Enter');
  await expect(list(page)).toBeVisible();
  await expect(link, 'the invite panel is not painted over the drawer').toBeHidden();
  await page.keyboard.press('Escape');
  await expect(list(page)).toBeHidden();
  await expect(link).toBeVisible();
  expect(await link.inputValue()).toBe(url);
});
