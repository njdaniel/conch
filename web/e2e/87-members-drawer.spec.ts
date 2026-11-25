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

// The drawer's stops as Lobby's Tab wrap reads them: the list itself, then the
// controls inside it that are tabbable, shown and not inert.
type End = 'first' | 'last';
const drawerStop = (page: Page, end: End) => page.evaluate((which) => {
  const body = document.getElementById('members-body');
  if (body === null) throw new Error('no drawer');
  const stops = [body, ...body.querySelectorAll<HTMLElement>('a[href], button, input, select, textarea, summary, [tabindex]')].filter(
    (el) => el.tabIndex >= 0 && !el.matches(':disabled') && el.closest('[hidden], [inert]') === null,
  );
  const stop = which === 'first' ? stops[0] : stops[stops.length - 1];
  if (stop === undefined) throw new Error('no stops');
  stop.focus();
}, end);
const onStop = (page: Page, end: End) => page.evaluate((which) => {
  const body = document.getElementById('members-body');
  if (body === null) return false;
  const stops = [body, ...body.querySelectorAll<HTMLElement>('a[href], button, input, select, textarea, summary, [tabindex]')].filter(
    (el) => el.tabIndex >= 0 && !el.matches(':disabled') && el.closest('[hidden], [inert]') === null,
  );
  return document.activeElement === (which === 'first' ? stops[0] : stops[stops.length - 1]);
}, end);

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
    // The ends of the drawer, however many people it holds: Tab from the
    // last stop lands on the first, Shift+Tab from the first on the last.
    await drawerStop(page, 'last');
    await page.keyboard.press('Tab');
    expect(await onStop(page, 'first'), 'Tab from the last stop lands on the first').toBe(true);
    await page.keyboard.press('Shift+Tab');
    expect(await onStop(page, 'last'), 'Shift+Tab from the first stop lands on the last').toBe(true);
    // And on, both ways, never out.
    for (const key of ['Tab', 'Shift+Tab']) {
      for (let i = 0; i < 30; i++) {
        await page.keyboard.press(key);
        expect(await inDrawer(page), `${key} #${String(i + 1)} left the drawer, at ${await page.evaluate(() => document.activeElement?.outerHTML.slice(0, 120) ?? 'nothing')}`).toBe(true);
      }
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

// A row's menu is open in the drawer, and the browser's own Tab leaves it:
// from a row in the middle, Tab goes to the next stop and Shift+Tab to the
// row's ⋯, with the page still holding focus; from the drawer's last stop,
// Tab wraps to its first and does not leave the page for the browser's chrome.
test('P2196 tablet 1050×800: Tab and Shift+Tab out of a row menu stay in the drawer, and wrap at its last stop', async ({ meet, world }) => {
  const suffix = Date.now().toString(36).slice(-5);
  const channel = `p2196m-${suffix}`;
  const name = `q${String(Math.floor(Math.random() * 100000)).padStart(5, '0')}m`;
  await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, { name: channel });
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, { kind: 'human', name })).principal.id;
  await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, { label: name });
  expect((await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`)).status).toBeLessThan(300);
  const hasFocus = (page: Page) => page.evaluate(() => document.hasFocus());
  const open = async () => {
    const { page } = await meet({ viewport: { width: 1050, height: 800 } });
    await page.goto(world.appUrl);
    await (await credentialField(page)).fill(world.tokens.operator);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await page.getByRole('button', { name: 'Continue anyway' }).click();
    await expect(statusLine(page)).toContainText('Connected');
    await openChannel(page, channel);
    await members(page).focus();
    await page.keyboard.press('Enter');
    await expect(list(page)).toBeFocused();
    return page;
  };

  for (const key of ['Tab', 'Shift+Tab']) {
    const page = await open();
    // The People list's row, not the Community's.
    const more = list(page).getByRole('list', { name: /^(Online|Offline)/ }).getByRole('button', { name: `Actions for ${name}` });
    await more.focus();
    // The stop after the button, with the menu shut: marked now.
    await page.evaluate(() => {
      const body = document.getElementById('members-body');
      const at = document.activeElement;
      if (body === null || at === null) throw new Error('no drawer');
      const stops = [body, ...body.querySelectorAll<HTMLElement>('a[href], button, input, select, textarea, summary, [tabindex]')].filter(
        (el) => el.tabIndex >= 0 && !el.matches(':disabled') && el.closest('[hidden], [inert]') === null,
      );
      stops[stops.indexOf(at as HTMLElement) + 1]?.setAttribute('data-next-stop', '');
    });
    await page.keyboard.press('Enter');
    await expect(list(page).getByRole('menu')).toBeVisible();
    await page.keyboard.press(key);
    await expect(list(page).getByRole('menu')).toHaveCount(0);
    // Past the browser's own move and any recovery after it.
    await page.waitForTimeout(300);
    expect(await hasFocus(page), `${key} left the page for the browser's chrome`).toBe(true);
    expect(await inDrawer(page), `${key} left the drawer`).toBe(true);
    if (key === 'Shift+Tab') await expect(more).toBeFocused();
    else expect(await page.evaluate(() => document.activeElement?.hasAttribute('data-next-stop') === true), `Tab landed on the next stop, not on ${await page.evaluate(() => document.activeElement?.outerHTML.slice(0, 140) ?? '')} (next: ${await page.evaluate(() => document.querySelector('[data-next-stop]')?.outerHTML.slice(0, 140) ?? 'gone')})`).toBe(true);
  }

  // From the drawer's last stop, an operator's Community row: its ⋯ menu open.
  const page = await open();
  await drawerStop(page, 'last');
  expect(await page.evaluate(() => document.activeElement?.classList.contains('more-actions') === true), 'the last stop is a row ⋯').toBe(true);
  await page.keyboard.press('Enter');
  await expect(list(page).getByRole('menu')).toBeVisible();
  // Whether the page handled the Tab itself (the wrap), as seen after its
  // handlers by a listener on the window: a Tab it left to the browser would
  // go on to the browser's chrome in a window that has one.
  await page.evaluate(() => {
    window.addEventListener('keydown', (event) => {
      if (event.key === 'Tab') Object.assign(window, { tabHandled: event.defaultPrevented });
    });
  });
  await page.keyboard.press('Tab');
  expect(await page.evaluate(() => (window as unknown as { tabHandled?: boolean }).tabHandled), 'the page wrapped the Tab itself').toBe(true);
  await expect(list(page).getByRole('menu')).toHaveCount(0);
  await page.waitForTimeout(300);
  expect(await hasFocus(page), 'Tab left the page for the browser\'s chrome').toBe(true);
  expect(await onStop(page, 'first'), 'Tab wrapped to the first stop').toBe(true);
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
