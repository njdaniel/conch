// The packaged app talking to a real conchd over REST (issue #942): add the
// address, sign in as a member, list channels, load history, post, sign out;
// malicious IPC; TLS; and the canary scans (no secret in the renderer, its
// storage, or main's log). The server is a disposable conchd on loopback
// built from this checkout; see conchd.ts.
import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { makePki, sendJson, startHttps, type Fake, type Pki } from '../unit/support.ts';
import { listedVersion, startConchd, type Disposable } from './conchd.ts';
import { launch, livePage, type Launched } from './launch.ts';

// A conch credential has this shape; none may be anywhere the renderer or the
// log can see, except a pasted one while it is being typed.
const TOKEN_SHAPE = /conch_[A-Za-z0-9_-]{20,}/;

async function attach(app: Launched): Promise<{ browser: Browser; page: Page }> {
  const browser = await chromium.connectOverCDP(await app.cdpUrl());
  const context = browser.contexts()[0];
  if (context === undefined) throw new Error('the app has no browser context');
  const page = context.pages()[0] ?? (await context.waitForEvent('page'));
  await page.waitForLoadState('load');
  return { browser, page };
}

// Everything the renderer holds that a secret could be in.
async function renderer(page: Page): Promise<string> {
  const stored = await page.evaluate(async () => {
    const databases = typeof indexedDB.databases === 'function' ? await indexedDB.databases() : [];
    return JSON.stringify({
      html: document.documentElement.outerHTML,
      local: Object.fromEntries(Object.entries(window.localStorage)),
      session: Object.fromEntries(Object.entries(window.sessionStorage)),
      cookie: document.cookie,
      databases,
      url: location.href,
      entries: performance.getEntries().map((e) => e.name),
    });
  });
  // (document.cookie is in the above; the app sets no cookie and sends none.)
  return stored;
}

// invoke calls a function of the bridge as the page does, and gives what the
// renderer received, or "rejected: <message>".
// heap is a snapshot of the page's JavaScript heap, taken over the debugging
// connection (it collects garbage first): what a script in the page could
// still reach, which the DOM and storage scans do not see.
async function heap(page: Page): Promise<string> {
  const cdp = await page.context().newCDPSession(page);
  const chunks: string[] = [];
  cdp.on('HeapProfiler.addHeapSnapshotChunk', ({ chunk }) => chunks.push(chunk));
  await cdp.send('HeapProfiler.enable');
  // Twice: a value the first pass finds held only by a finalizer goes in the second.
  await cdp.send('HeapProfiler.collectGarbage');
  await cdp.send('HeapProfiler.collectGarbage');
  await cdp.send('HeapProfiler.takeHeapSnapshot', { reportProgress: false });
  await cdp.detach();
  return chunks.join('');
}

async function invoke(page: Page, name: string, ...args: unknown[]): Promise<unknown> {
  return page.evaluate(
    async ({ name: fn, args: list }) => {
      const bridge = window.conchDesktop as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
      try {
        return (await bridge[fn]?.(...list)) ?? null;
      } catch (error) {
        return `rejected: ${error instanceof Error ? error.message : 'unknown'}`;
      }
    },
    { name, args },
  );
}

// Sign out is in Settings and asks first, as it does in the browser.
// Signing out replaces the window (a new web contents, a fresh session), so it
// gives the page that took the old one's place.
async function signOutThroughSettings(browser: Browser, page: Page): Promise<Page> {
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByRole('button', { name: 'Yes, sign out', exact: true }).click();
  const next = await livePage(browser, page);
  await expect(next.getByLabel('Credential')).toBeVisible();
  return next;
}

test.describe('the packaged app against a real conchd', () => {
  let server: Disposable;
  let app: Launched;
  let browser: Browser;
  let page: Page;

  test.beforeAll(async () => {
    test.setTimeout(240_000);
    server = await startConchd();
    app = launch({ debugPort: true });
    ({ browser, page } = await attach(app));
  });

  test.afterAll(async () => {
    await browser.close();
    await app.stop();
    await server.stop();
  });

  test('asks for the server address first, and refuses a bad one by name', async () => {
    await expect(page.getByRole('heading', { name: 'Server address' })).toBeVisible();
    for (const [typed, words] of [
      ['http://example.org', /secure \(https\)/],
      ['https://user:pw@example.org', /user name and password/],
      ['https://example.org/app', /no path/],
      ['not an address', /not a web address/],
    ] as const) {
      await page.getByLabel('Server address').fill(typed);
      await page.getByRole('button', { name: 'Continue' }).click();
      await expect(page.getByRole('alert')).toHaveText(words);
    }
  });

  test('takes the address, then the existing sign-in views, and signs a member in with a password', async () => {
    await page.getByLabel('Server address').fill(server.origin);
    await page.getByRole('button', { name: 'Continue' }).click();
    // Adding the server makes it the active profile: a new window.
    page = await livePage(browser, page);
    await page.getByLabel('Name', { exact: true }).fill(server.member.name);
    await page.getByLabel('Password', { exact: true }).fill(server.member.password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Settings' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
  });

  test('lists channels, posts, and loads history through main', async () => {
    const channels = (await invoke(page, 'request', { operation: 'listChannels' })) as { kind: string; status: number; body: { channels: { name: string }[] } };
    expect(channels.kind).toBe('reply');
    expect(channels.body.channels.map((c) => c.name)).toContain('general');
    const posted = (await invoke(page, 'request', { operation: 'postMessage', params: { channel: 'general' }, body: { body: 'hello from the desktop smoke' } })) as { kind: string; status: number };
    expect(posted.kind).toBe('reply');
    expect(posted.status).toBeLessThan(300);
    const history = (await invoke(page, 'request', { operation: 'listMessages', params: { channel: 'general' } })) as { body: { messages: { body: string }[] } };
    expect(history.body.messages.map((m) => m.body)).toContain('hello from the desktop smoke');
    // The server saw it from the member, not from the operator.
    const seen = (await server.api(server.operatorToken, 'GET', '/v2/channels/general/messages')).body as { messages: { body: string; author?: { name?: string } }[] };
    expect(seen.messages.some((m) => m.body === 'hello from the desktop smoke')).toBe(true);
  });

  test('refuses malicious IPC: an operation that is not the renderer’s, a bad shape, a path that escapes', async () => {
    const before = app.output();
    expect(await invoke(page, 'request', { operation: 'wsTicket' })).toBe('rejected: refused');
    expect(await invoke(page, 'request', { operation: 'login', body: { name: 'ada', password: 'x' } })).toBe('rejected: refused');
    expect(await invoke(page, 'request', { operation: 'voiceSession', params: { channel: 'general' } })).toBe('rejected: refused');
    expect(await invoke(page, 'request', { operation: 'messagesSocket' })).toBe('rejected: refused');
    expect(await invoke(page, 'request', { operation: 'listMessages', params: { channel: '../v1/credentials' } })).toBe('rejected: refused');
    expect(await invoke(page, 'request', { url: 'https://example.com/', method: 'GET' })).toBe('rejected: refused');
    expect(await invoke(page, 'profileSwitch', 'not-an-id')).toBe('rejected: refused');
    expect(await invoke(page, 'profileRemove', 'A'.repeat(32))).toBe('rejected: refused');
    expect(await invoke(page, 'signInWithPassword', { name: 1 })).toBe('rejected: refused');
    const lines = app.output().slice(before.length).split('\n').filter((l) => l.includes('refused an IPC call'));
    expect(lines.length).toBeGreaterThanOrEqual(8);
    // A refusal says which check, never what was sent.
    expect(lines.join('\n')).not.toContain('example.com');
    // And it signed nobody out.
    expect(((await invoke(page, 'restore')) as { ok: boolean }).ok).toBe(true);
  });

  test('keeps no secret in the renderer, its storage, or main’s log', async () => {
    const seen = await renderer(page);
    expect(seen).not.toMatch(TOKEN_SHAPE);
    expect(seen).not.toContain(server.member.password);
    expect(app.output()).not.toMatch(TOKEN_SHAPE);
    expect(app.output()).not.toContain(server.member.password);
    const answers = JSON.stringify([await invoke(page, 'restore'), await invoke(page, 'serverAddress'), await invoke(page, 'request', { operation: 'whoami' })]);
    expect(answers).not.toMatch(TOKEN_SHAPE);
  });

  test('survives a reload of the page: main still holds the sign-in', async () => {
    expect(page.url()).toMatch(/^conch-app:\/\/ui\/(#.*)?$/);
    await page.reload();
    await expect(page.getByRole('button', { name: 'Settings' })).toBeVisible();
  });

  test('signs out through the API and forgets locally', async () => {
    page = await signOutThroughSettings(browser, page);
    expect(await invoke(page, 'restore')).toMatchObject({ ok: false, kind: 'unauthorized' });
    expect(await invoke(page, 'request', { operation: 'listChannels' })).toEqual({ kind: 'unauthorized' });
  });

  test('signs in with a pasted credential, which main keeps and the page does not, in its DOM, storage or memory', async () => {
    // Typed, not filled: fill() leaves the value in Playwright's own utility
    // world in the same renderer, which the heap scan would then find.
    await page.getByLabel('Credential').click();
    await page.keyboard.type(server.member.token);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await expect(page.getByRole('button', { name: 'Settings' })).toBeVisible();
    // The page keeps the credential without waiting for main (credentials.write
    // hands it over and goes on): scan only once main says it is in force, so
    // the scan does not race the handoff itself.
    await expect.poll(async () => ((await invoke(page, 'restore')) as { ok?: boolean }).ok).toBe(true);
    const seen = await renderer(page);
    expect(seen).not.toContain(server.member.token);
    expect(app.output()).not.toContain(server.member.token);
    // Nor in the page's memory: the client pinned the stand-in.
    expect(await heap(page)).not.toContain(server.member.token);
    page = await signOutThroughSettings(browser, page);
  });

  // An operator's pasted credential is checked, then waits behind the
  // operator warning. Declined, it was never in force: nothing runs as the
  // operator, and a reload does not sign the window in.
  async function pasteOperator(): Promise<void> {
    await page.getByLabel('Credential').click();
    await page.keyboard.type(server.operatorToken);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await expect(page.getByRole('heading', { name: 'This is an operator credential' })).toBeVisible();
    expect(await invoke(page, 'restore')).toMatchObject({ ok: false, kind: 'unauthorized' });
    expect(await invoke(page, 'request', { operation: 'listChannels' })).toEqual({ kind: 'unauthorized' });
  }

  test('an operator credential declined at the warning is never in force, and a reload does not find it', async () => {
    await pasteOperator();
    // Declining signs nobody out of anything: the client only drops what it
    // held, so the window stays.
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await expect(page.getByLabel('Credential')).toBeVisible();
    expect(await invoke(page, 'restore')).toMatchObject({ ok: false, kind: 'unauthorized' });
    expect(await invoke(page, 'request', { operation: 'listChannels' })).toEqual({ kind: 'unauthorized' });
    // (Main still has it set aside until it lapses: keeping it needs the
    // credential itself, which a script in the page could as well paste anew.)
    await page.reload();
    await expect(page.getByLabel('Credential')).toBeVisible();
    expect(await invoke(page, 'restore')).toMatchObject({ ok: false, kind: 'unauthorized' });
  });

  test('a reload while the operator warning is showing signs nobody in', async () => {
    await pasteOperator();
    await page.reload();
    await expect(page.getByLabel('Credential')).toBeVisible();
    expect(await invoke(page, 'restore')).toMatchObject({ ok: false, kind: 'unauthorized' });
  });

  test('an operator credential is in force once the person continues past the warning, and the page holds only the stand-in', async () => {
    // This server's saved entry is bound to the member who signed in above, and
    // another account never takes its place: the operator signs in on a second
    // entry for the same server (issue #945).
    await page.getByRole('button', { name: 'Servers' }).click();
    await page.getByLabel('Server address').fill(server.origin);
    await page.getByRole('button', { name: 'Continue' }).click();
    page = await livePage(browser, page);
    await pasteOperator();
    await page.getByRole('button', { name: 'Continue anyway' }).click();
    await expect(page.getByRole('button', { name: 'Settings' })).toBeVisible();
    await expect.poll(async () => ((await invoke(page, 'restore')) as { ok?: boolean }).ok).toBe(true);
    expect(await renderer(page)).not.toContain(server.operatorToken);
    expect(await heap(page)).not.toContain(server.operatorToken);
    page = await signOutThroughSettings(browser, page);
  });
});

test.describe('TLS in the packaged app', () => {
  let pki: Pki;
  let https: Fake;

  test.beforeAll(async () => {
    pki = makePki();
    https = await startHttps(pki, (_request, res) => {
      sendJson(res, 200, { status: 'ok', version: listedVersion(), db: 'ok', voice: 'not_configured' });
    });
  });

  test.afterAll(async () => {
    await https.close();
    pki.cleanup();
  });

  async function enter(app: Launched): Promise<{ browser: Browser; page: Page }> {
    const attached = await attach(app);
    await attached.page.getByLabel('Server address').fill(https.origin);
    await attached.page.getByRole('button', { name: 'Continue' }).click();
    return attached;
  }

  test('says a certificate this computer does not trust is that, and does not go on', async () => {
    const app = launch({ debugPort: true });
    const { browser, page } = await enter(app);
    try {
      await expect(page.getByRole('alert')).toContainText('does not trust that server’s certificate');
      expect(https.seen).toHaveLength(0);
    } finally {
      await browser.close();
      await app.stop();
    }
  });

  test('trusts it when the CA is in the trust store the app loads (the private-CA procedure)', async () => {
    const app = launch({ debugPort: true, env: { SSL_CERT_FILE: pki.caPath } });
    const { browser, page: first } = await enter(app);
    try {
      // The address was good: the saved server is now the active one, in a new window.
      const page = await livePage(browser, first);
      await expect(page.getByRole('heading', { name: 'Server address' })).toHaveCount(0);
      await expect(page.getByRole('banner', { name: 'Server' })).toContainText('localhost');
      await expect.poll(() => https.seen.some((s) => s.url === '/healthz')).toBe(true);
    } finally {
      await browser.close();
      await app.stop();
    }
  });
});
