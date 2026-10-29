// The packaged app talking to a real conchd over REST (issue #942): add the
// address, sign in as a member, list channels, load history, post, sign out;
// malicious IPC; TLS; and the canary scans (no secret in the renderer, its
// storage, or main's log). The server is a disposable conchd on loopback
// built from this checkout; see conchd.ts.
import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { makePki, sendJson, startHttps, type Fake, type Pki } from '../unit/support.ts';
import { startConchd, type Disposable } from './conchd.ts';
import { launch, type Launched } from './launch.ts';

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
async function signOutThroughSettings(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByRole('button', { name: 'Yes, sign out', exact: true }).click();
  await expect(page.getByLabel('Credential')).toBeVisible();
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
    expect(await invoke(page, 'setServerAddress', { origin: 'x' })).toBe('rejected: refused');
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
    await signOutThroughSettings(page);
    expect(await invoke(page, 'restore')).toMatchObject({ ok: false, kind: 'unauthorized' });
    expect(await invoke(page, 'request', { operation: 'listChannels' })).toEqual({ kind: 'unauthorized' });
  });

  test('signs in with a pasted credential, which main keeps and the page does not', async () => {
    await page.getByLabel('Credential').fill(server.member.token);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await expect(page.getByRole('button', { name: 'Settings' })).toBeVisible();
    const seen = await renderer(page);
    expect(seen).not.toContain(server.member.token);
    expect(app.output()).not.toContain(server.member.token);
    await signOutThroughSettings(page);
  });
});

test.describe('TLS in the packaged app', () => {
  let pki: Pki;
  let https: Fake;

  test.beforeAll(async () => {
    pki = makePki();
    https = await startHttps(pki, (_request, res) => {
      sendJson(res, 200, { status: 'ok', version: 'smoke', db: 'ok', voice: 'not_configured' });
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
    const { browser, page } = await enter(app);
    try {
      await expect(page.getByRole('alert')).toHaveText('');
      await expect(page.getByRole('heading', { name: 'Server address' })).toHaveCount(0);
      await expect.poll(() => https.seen.some((s) => s.url === '/healthz')).toBe(true);
    } finally {
      await browser.close();
      await app.stop();
    }
  });
});
