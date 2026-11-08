// Issue #1545 (links): every link in a message is recognisably external, shows
// the address it opens, opens without a Referer, is fetched by nobody before it
// is clicked, fits a phone, and is reachable by keyboard. A body that only looks
// like a link stays text. alice sends and bob reads (the browser and guest
// accounts of the fixture community).
import type { BrowserContext, Locator, Page } from '@playwright/test';
import { post } from './api';
import { consoleErrors, expect, expectFits, messages, signIn, test } from './fixtures';

interface Hit {
  readonly url: string;
  readonly referer: string | undefined;
}

interface Landing {
  readonly origin: string;
  readonly host: string;
  readonly hits: Hit[];
}

// landing stands in for a web site the links can point at. The browser context
// answers for it (no server, no network), and records what it is asked.
async function landing(...contexts: BrowserContext[]): Promise<Landing> {
  const hits: Hit[] = [];
  const host = 'links-1545.test';
  for (const context of contexts) {
    await context.route(`http://${host}/**`, async (route) => {
      hits.push({ url: route.request().url(), referer: route.request().headers()['referer'] });
      await route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>landing</title><p>landed</p>' });
    });
  }
  return { origin: `http://${host}`, host, hits };
}

// recordRequests lists every address the page asks for, from now on.
function recordRequests(page: Page): string[] {
  const urls: string[] = [];
  page.on('request', (r) => urls.push(r.url()));
  return urls;
}

const hostile = [
  'javascript:alert(1)',
  'data:text/html,<script>alert(1)</script>',
  'vbscript:msgbox(1)',
  'file:///etc/passwd',
  'http://user:pw@example.com/',
  'https://exa%6dple.com/',
  'https://xn--e1afmkfd.xn--p1ai/',
  'https://аpple.com/',
  'https://exa‮mple.com/',
  'https://exa​mple.com/',
  `https://example.com/${'a'.repeat(2049)}`,
  '<a href="javascript:alert(1)">literal tag</a>',
  '[label](javascript:alert(1))',
];

// A link is found by its destination; the cue is the glyph beside the text.
async function expectExternal(link: Locator, text: string, href: string): Promise<void> {
  await expect(link).toHaveAttribute('href', href);
  await expect(link).toHaveAttribute('title', href);
  await expect(link).toHaveAttribute('target', '_blank');
  await expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  await expect(link).toHaveAttribute('aria-label', `${text} (opens in a new tab)`);
  await expect(link.locator('.external-link-cue svg')).toBeVisible();
  await expect(link).toHaveAccessibleName(`${text} (opens in a new tab)`);
  await expect(link).toHaveText(text);
}

test('L1545 only the safe addresses become links, each with the cue and its canonical address as title', async ({ meet, world }) => {
  const alice = (await meet()).page;
  const bob = (await meet()).page;
  const site = await landing();
  const errors = [consoleErrors(alice), consoleErrors(bob)];
  await signIn(alice, world, world.tokens.browser, 'browser-test');
  await signIn(bob, world, world.tokens.guest, 'jamie-test');

  const tag = `l1545-${Date.now().toString(36)}`;
  const safe = [
    { text: 'https://Example.COM/Path?q=1', href: 'https://example.com/Path?q=1' },
    { text: `${site.origin}/ok`, href: `${site.origin}/ok` },
    { text: 'HTTP://EXAMPLE.org/', href: 'http://example.org/' },
  ];
  // Hostile entries get their own message; the safe ones share a last message.
  for (const [i, entry] of hostile.entries()) await post(world, world.tokens.browser, 'general', `${tag}-h${String(i)} ${entry}`);
  await post(world, world.tokens.browser, 'general', `${tag}-safe ${safe.map((s) => s.text).join(' ')}`);

  for (const page of [alice, bob]) {
    for (let i = 0; i < hostile.length; i++) {
      const line = messages(page).locator('li[data-message-id]').filter({ hasText: new RegExp(`${tag}-h${String(i)}\\b`) });
      await expect(line).toHaveCount(1);
      await expect(line.locator('a'), `hostile entry ${String(i)} is text`).toHaveCount(0);
    }
    const line = messages(page).locator('li[data-message-id]').filter({ hasText: `${tag}-safe` });
    await expect(line.locator('a')).toHaveCount(safe.length);
    for (const [i, s] of safe.entries()) await expectExternal(line.locator('a').nth(i), s.text, s.href);
    // Nothing in the log is inert-looking but live: every anchor is http or https.
    const schemes = await messages(page).locator('a').evaluateAll((as) => as.map((a) => new URL((a as HTMLAnchorElement).href).protocol));
    for (const scheme of schemes) expect(['http:', 'https:']).toContain(scheme);
  }
  for (const list of errors) expect(list, 'console errors').toEqual([]);
});

test('L1545 a click opens the link in a new tab with no Referer, and nobody asked the host before', async ({ meet, world }) => {
  const aliceTab = await meet();
  const alice = aliceTab.page;
  const bob = await meet();
  const site = await landing(aliceTab.context, bob.context);
  const askedByAlice = recordRequests(alice);
  const asked = recordRequests(bob.page);
  const errors = [consoleErrors(alice), consoleErrors(bob.page)];
  await signIn(alice, world, world.tokens.browser, 'browser-test');
  await signIn(bob.page, world, world.tokens.guest, 'jamie-test');

  const tag = `l1545c-${Date.now().toString(36)}`;
  const href = `${site.origin}/click-${tag}`;
  await post(world, world.tokens.browser, 'general', `${tag} ${href}`);
  const link = messages(bob.page).locator('li[data-message-id]').filter({ hasText: tag }).locator('a');
  await expect(link).toHaveCount(1);
  await expectExternal(link, href, href);

  // Before the click: not prefetched, previewed or fetched by either page.
  await bob.page.waitForTimeout(500);
  expect(site.hits, 'the server was asked before the click').toEqual([]);
  // The linked site is a stand-in answered inside the browser contexts, not a
  // listener: conchd has no link fetcher (no unfurl or preview of message URLs
  // anywhere under internal/), so a server-side fetch has nothing to show.
  expect(askedByAlice.filter((u) => new URL(u).host === site.host), 'alice\'s page asked the host').toEqual([]);
  expect(asked.filter((u) => new URL(u).host === site.host), 'bob\'s page asked the host before the click').toEqual([]);

  const opened = bob.context.waitForEvent('page');
  await link.click();
  const tab = await opened;
  await tab.waitForLoadState('domcontentloaded');
  expect(tab.url()).toBe(href);
  await expect(tab.getByText('landed')).toBeVisible();
  expect(site.hits).toHaveLength(1);
  expect(site.hits[0]?.referer, 'a Referer was sent').toBeUndefined();
  expect(await tab.evaluate(() => (window.opener as unknown) ?? null), 'the new tab has an opener').toBeNull();
  // One activation, no dialog: the chat page is still the chat page.
  await expect(messages(bob.page)).toBeVisible();
  await tab.close();
  for (const list of errors) expect(list, 'console errors').toEqual([]);
});

test('L1545 a 300-character link wraps inside a 360 px column', async ({ meet, world }) => {
  const phone = await meet({ viewport: { width: 360, height: 740 } });
  const site = await landing();
  const errors = consoleErrors(phone.page);
  await signIn(phone.page, world, world.tokens.guest, 'jamie-test');
  const tag = `l1545w-${Date.now().toString(36)}`;
  const href = `${site.origin}/${'w'.repeat(300 - site.origin.length - 1)}`;
  expect(href).toHaveLength(300);
  await post(world, world.tokens.browser, 'general', `${tag} ${href}`);
  const line = messages(phone.page).locator('li[data-message-id]').filter({ hasText: tag });
  const link = line.locator('a');
  await expect(link).toHaveCount(1);
  await expect(link).toHaveAttribute('href', href);
  await link.scrollIntoViewIfNeeded();
  await expectFits(phone.page, 'a 300-character link');
  const log = messages(phone.page);
  expect(await log.evaluate((el) => el.scrollWidth <= el.clientWidth), 'the log scrolls sideways').toBe(true);
  const box = await link.boundingBox();
  const column = await log.boundingBox();
  expect(box, 'the link has a box').not.toBeNull();
  expect(column, 'the log has a box').not.toBeNull();
  if (box !== null && column !== null) expect(box.x + box.width).toBeLessThanOrEqual(column.x + column.width + 1);
  expect(errors).toEqual([]);
});

test('L1545 Tab reaches each link in reading order, with the focus ring showing', async ({ meet, world }) => {
  const bob = (await meet()).page;
  const errors = consoleErrors(bob);
  await signIn(bob, world, world.tokens.guest, 'jamie-test');
  const tag = `l1545k-${Date.now().toString(36)}`;
  const urls = [`https://example.com/${tag}/one`, `https://example.net/${tag}/two`, `https://example.org/${tag}/three`];
  await post(world, world.tokens.browser, 'general', `${tag} ${urls.join(' then ')}`);
  const line = messages(bob).locator('li[data-message-id]').filter({ hasText: tag });
  const links = line.locator('a');
  await expect(links).toHaveCount(urls.length);

  await links.first().focus();
  // Walk backwards from the first link to the start of the line would be brittle;
  // start at the link before the first stop and tab forward from there.
  await bob.keyboard.press('Shift+Tab');
  for (const url of urls) {
    await bob.keyboard.press('Tab');
    const link = line.locator(`a[href="${url}"]`);
    await expect(link).toBeFocused();
    await expect(link).toHaveCSS('outline-style', 'solid');
    expect(parseFloat(await link.evaluate((el) => getComputedStyle(el).outlineWidth))).toBeGreaterThanOrEqual(2);
    await expect(link).toBeInViewport();
  }
  expect(errors, 'console errors').toEqual([]);
});
