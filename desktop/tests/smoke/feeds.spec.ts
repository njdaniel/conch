// The packaged app's live feeds through main (issue #943), beside a browser
// user on the same disposable conchd: the first usable demo. Two principals on
// one machine, ada in the desktop app and grace in Playwright's own Chromium
// at the server's /app/; a message posted on either side appears live on the
// other. Then what main holds: the sockets it counts in its log, closed at a
// reload, at sign-out and when the renderer crashes, and no bearer or ticket in
// the renderer, a URL or the log.
import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { startConchd, type Disposable } from './conchd.ts';
import { launch, livePage, type Launched } from './launch.ts';

const TOKEN_SHAPE = /conch_[A-Za-z0-9_-]{20,}/;

const messages = (page: Page) => page.getByRole('log', { name: 'Messages' });
const statusLine = (page: Page) => page.getByRole('status', { name: 'Status' });

async function say(page: Page, body: string): Promise<void> {
  await page.getByLabel('Message #general').fill(body);
  await page.getByRole('button', { name: 'Send' }).click();
}

async function signIn(page: Page, name: string, password: string): Promise<void> {
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
}

// The feed lines main's log has, and how many sockets the last one says main
// holds: open, and closed but not yet gone (main counts every socket until its
// close arrives or it is destroyed; it logs the kind and the reason, never the
// address).
function feedLines(output: string): string[] {
  return output.split('\n').filter((line) => line.startsWith('conch-desktop: feed '));
}

function counts(output: string): { readonly open: number; readonly closing: number } {
  const last = feedLines(output).at(-1);
  const m = last === undefined ? null : /; (\d+) open, (\d+) closing$/.exec(last);
  return { open: Number(m?.[1] ?? 0), closing: Number(m?.[2] ?? 0) };
}

const openSockets = (output: string): number => counts(output).open;
const heldSockets = (output: string): number => counts(output).open + counts(output).closing;

test.describe('live feeds through main, beside a browser user', () => {
  let server: Disposable;
  let app: Launched;
  let cdp: Browser;
  let desktop: Page;
  let browser: Browser;
  let web: Page;

  test.beforeAll(async () => {
    test.setTimeout(240_000);
    server = await startConchd({ web: true });
    app = launch({ debugPort: true });
    cdp = await chromium.connectOverCDP(await app.cdpUrl());
    const context = cdp.contexts()[0];
    if (context === undefined) throw new Error('the app has no browser context');
    desktop = context.pages()[0] ?? (await context.waitForEvent('page'));
    await desktop.waitForLoadState('load');
    browser = await chromium.launch();
    web = await (await browser.newContext()).newPage();
  });

  test.afterAll(async () => {
    await browser.close();
    await cdp.close();
    await app.stop();
    await server.stop();
  });

  test('the desktop app signs in, and main opens its feeds with nothing in a URL but the route', async () => {
    await desktop.getByLabel('Server address').fill(server.origin);
    await desktop.getByRole('button', { name: 'Continue' }).click();
    // Adding the server makes it the active profile: a new window.
    desktop = await livePage(cdp, desktop);
    await signIn(desktop, server.member.name, server.member.password);
    // Connected is the messages feed open: the renderer's own state machine,
    // fed by main.
    await expect(statusLine(desktop)).toContainText('Connected');
    await expect.poll(() => openSockets(app.output())).toBeGreaterThanOrEqual(2);
    // Through undici's Agent: the frame-size cap at the header and a
    // connection main can destroy (main/api/socket.ts).
    expect(app.output()).not.toContain('not undici’s Agent');
  });

  test('a browser user signs in on the same server as another principal', async () => {
    await web.goto(`${server.origin}/app/`);
    await signIn(web, server.other.name, server.other.password);
    await expect(statusLine(web)).toContainText('Connected');
  });

  test('a message posted in the browser appears live in the desktop app', async () => {
    const body = 'from the browser, live in the desktop app';
    await say(web, body);
    await expect(messages(desktop).getByText(body)).toHaveCount(1);
  });

  test('a message posted in the desktop app appears live in the browser', async () => {
    const body = 'from the desktop app, live in the browser';
    await say(desktop, body);
    await expect(messages(web).getByText(body)).toHaveCount(1);
    await expect(messages(desktop).getByText(body)).toHaveCount(1);
  });

  test('a reload closes the old page’s sockets in main, and the new page’s feeds are live again', async () => {
    const before = feedLines(app.output()).length;
    await desktop.reload();
    await expect(statusLine(desktop)).toContainText('Connected');
    const since = feedLines(app.output()).slice(before);
    expect(since.some((line) => line.includes('closed (document)'))).toBe(true);
    const body = 'from the browser, after the desktop reloaded';
    await say(web, body);
    await expect(messages(desktop).getByText(body)).toHaveCount(1);
  });

  test('keeps the bearer and any ticket out of the renderer, its storage, its requests and main’s log', async () => {
    const seen = await desktop.evaluate(() =>
      JSON.stringify({
        html: document.documentElement.outerHTML,
        local: Object.fromEntries(Object.entries(window.localStorage)),
        session: Object.fromEntries(Object.entries(window.sessionStorage)),
        entries: performance.getEntries().map((e) => e.name),
      }),
    );
    expect(seen).not.toMatch(TOKEN_SHAPE);
    expect(seen).not.toContain('ticket');
    // The renderer made no request of its own: no socket, no fetch.
    expect(seen).not.toContain('/v2/ws');
    expect(app.output()).not.toMatch(TOKEN_SHAPE);
    expect(app.output()).not.toContain('ticket');
    expect(app.output()).not.toContain('/v2/ws');
  });

  test('sign-out closes every socket main holds', async () => {
    await desktop.getByRole('button', { name: 'Settings' }).click();
    await desktop.getByRole('button', { name: 'Sign out', exact: true }).click();
    await desktop.getByRole('button', { name: 'Yes, sign out', exact: true }).click();
    // Sign-out replaces the window: a new web contents in a fresh session.
    desktop = await livePage(cdp, desktop);
    await expect(desktop.getByLabel('Credential')).toBeVisible();
    await expect.poll(() => heldSockets(app.output())).toBe(0);
  });

  test('a renderer crash closes every socket it owned', async () => {
    await signIn(desktop, server.member.name, server.member.password);
    await expect(statusLine(desktop)).toContainText('Connected');
    await expect.poll(() => openSockets(app.output())).toBeGreaterThanOrEqual(2);
    const before = feedLines(app.output()).length;
    const session = await desktop.context().newCDPSession(desktop);
    // The crashed page never answers the command: it is not waited for.
    void session.send('Page.crash').catch(() => undefined);
    await expect.poll(() => heldSockets(app.output())).toBe(0);
    expect(feedLines(app.output()).slice(before).some((line) => line.includes('closed (document)'))).toBe(true);
  });
});
