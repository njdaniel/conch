// The packaged app's attachments, links and clipboard through main (issue
// #947), against a disposable conchd: attach an image and send it, see its
// blob: preview, read its bytes back through main, a hostile link refused, no
// window made, a save refused without the person's own input, the clipboard
// write allowed after a click and the read never.
//
// What this does not do: drive the native save dialog. The packaged app has
// no inspector (the fuses, launch.ts) and no test hook, so the dialog cannot
// be stubbed from outside; the save path rule is covered by the unit tests of
// main/files.ts with a stubbed dialog and a real temporary directory.
import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { startConchd, type Disposable } from './conchd.ts';
import { launch, type Launched } from './launch.ts';

// A 1x1 PNG.
const PNG_BASE64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==';

async function attach(app: Launched): Promise<{ browser: Browser; page: Page }> {
  const browser = await chromium.connectOverCDP(await app.cdpUrl());
  const context = browser.contexts()[0];
  if (context === undefined) throw new Error('the app has no browser context');
  const page = context.pages()[0] ?? (await context.waitForEvent('page'));
  await page.waitForLoadState('load');
  return { browser, page };
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

test.describe('attachments, links and the clipboard in the packaged app', () => {
  let server: Disposable;
  let app: Launched;
  let browser: Browser;
  let page: Page;

  test.beforeAll(async () => {
    test.setTimeout(240_000);
    server = await startConchd();
    app = launch({ debugPort: true });
    ({ browser, page } = await attach(app));
    await page.getByLabel('Server address').fill(server.origin);
    await page.getByRole('button', { name: 'Continue' }).click();
    await page.getByLabel('Name', { exact: true }).fill(server.member.name);
    await page.getByLabel('Password', { exact: true }).fill(server.member.password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
  });

  test.afterAll(async () => {
    await browser.close();
    await app.stop();
    await server.stop();
  });

  test('attaches an image, sends it, and previews it as a blob: image', async () => {
    await page.getByLabel('Choose files').setInputFiles({ name: 'dot.png', mimeType: 'image/png', buffer: Buffer.from(PNG_BASE64, 'base64') });
    await expect(page.getByRole('list', { name: 'Files to send' }).getByText('dot.png')).toBeVisible();
    await page.getByLabel('Message #general').fill('an attachment from the desktop smoke');
    await page.getByRole('button', { name: 'Send', exact: true }).click();
    const enlarge = page.getByRole('button', { name: 'Enlarge dot.png' });
    await expect(enlarge).toBeVisible();
    expect(await enlarge.locator('img').getAttribute('src')).toMatch(/^blob:/);
    // The server holds it, uploaded by the member through main.
    const stored = (await server.api(server.member.token, 'GET', '/v2/channels/general/messages')).body as { messages: { body: string; payload?: { data?: { file_ids?: number[] } } }[] };
    const sent = stored.messages.find((m) => m.body === 'an attachment from the desktop smoke');
    expect(sent?.payload?.data?.file_ids).toHaveLength(1);
  });

  test('main returns the file’s bytes for a preview, and nothing is written or opened', async () => {
    const stored = (await server.api(server.member.token, 'GET', '/v2/channels/general/messages')).body as { messages: { payload?: { data?: { file_ids?: number[] } } }[] };
    const id = stored.messages.flatMap((m) => m.payload?.data?.file_ids ?? [])[0];
    expect(id).toBeGreaterThan(0);
    expect(await invoke(page, 'downloadFile', id)).toEqual({ kind: 'bytes', data: PNG_BASE64 });
    expect(await invoke(page, 'downloadFile', 999_999)).toMatchObject({ kind: 'reply', status: 404 });
  });

  test('refuses a save that no click of the person’s asked for, and a save that names a path', async () => {
    // The earlier tests clicked; wait out the window in which a save is allowed.
    await page.waitForTimeout(2_500);
    const before = app.output();
    expect(await invoke(page, 'saveFile', 1)).toBe('rejected: refused');
    expect(await invoke(page, 'saveFile', '/tmp/conch-smoke-evil')).toBe('rejected: refused');
    expect(await invoke(page, 'saveFile', 1, '/tmp/conch-smoke-evil')).toBe('rejected: refused');
    expect(app.output().slice(before.length)).toContain('refused an IPC call (gesture)');
    expect(app.output().slice(before.length)).not.toContain('conch-smoke-evil');
  });

  test('refuses an upload past the cap or with a path or URL in it, without sending it', async () => {
    const big = Buffer.alloc(10 * 1024 * 1024 + 1).toString('base64');
    expect(await invoke(page, 'uploadFile', { channel: 'general', filename: 'big.bin', mediaType: 'application/octet-stream', data: big })).toBe('rejected: refused');
    expect(await invoke(page, 'uploadFile', { channel: '../v1/credentials', filename: 'a', mediaType: 'x/y', data: '' })).toBe('rejected: refused');
    expect(await invoke(page, 'uploadFile', { url: 'https://example.com/', channel: 'general', filename: 'a', mediaType: 'x/y', data: '' })).toBe('rejected: refused');
  });

  test('opens no window for a script, whatever the address, even right after the person typed', async () => {
    const before = app.output();
    // Typing is input, but not an act that activates anything.
    await page.getByLabel('Message #general').press('x');
    for (const url of ['file:///etc/passwd', 'javascript:alert(1)', 'conch-app://ui/', 'https://example.com/', 'https://user:pw@example.com/']) {
      expect(await page.evaluate((u) => window.open(u) === null, url), url).toBe(true);
    }
    await page.waitForTimeout(500);
    expect(browser.contexts().flatMap((c) => c.pages())).toHaveLength(1);
    const lines = app.output().slice(before.length);
    expect(lines).toContain('refused a new window (no user gesture)');
    expect(lines).not.toContain('example.com');
  });

  test('lets the page write text to the clipboard after a click, and never read it', async () => {
    // Any click of the person's own is a gesture; the page cannot make one.
    expect(await page.evaluate("navigator.clipboard.writeText('no click yet').then(() => 'written', () => 'refused')")).toBe('refused');
    await page.mouse.click(5, 5);
    expect(await page.evaluate("navigator.clipboard.writeText('from the smoke test').then(() => 'written', (e) => String(e))")).toBe('written');
    const read = await page.evaluate("navigator.clipboard.readText().then(() => 'read', () => 'refused')");
    expect(read).toBe('refused');
  });

  test('one click is one action: a save, then a window, then a second save find no gesture', async () => {
    await page.waitForTimeout(1_500);
    const before = app.output();
    await page.mouse.click(5, 5);
    // File 999999 does not exist, so a save that is let through fails before
    // any dialog; what is measured is that the click was used up.
    expect(await invoke(page, 'saveFile', 999_999)).toBe('failed');
    expect(await page.evaluate("window.open('https://user:pw@example.com/y') === null")).toBe(true);
    expect(await invoke(page, 'saveFile', 999_999)).toBe('rejected: refused');
    const lines = app.output().slice(before.length);
    expect(lines).toContain('refused a new window (no user gesture)');
    expect(lines).toContain('refused an IPC call (gesture)');
    expect(lines).not.toContain('refused a link');
  });

  test('a clicked link with credentials in its address is refused by main, not opened', async () => {
    // Last, and not awaited: main shows a modal message (and without a display
    // that ends the headless app), so the log line is the evidence.
    await page.waitForTimeout(1_500);
    const before = app.output();
    await page.evaluate(() => {
      const a = document.createElement('a');
      a.id = 'hostile-link';
      a.href = 'https://user:pw@example.com/x';
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.textContent = 'hostile link';
      a.style.cssText = 'position:fixed;left:0;top:0;z-index:99999;background:#fff';
      document.body.append(a);
    });
    page.locator('#hostile-link').click().catch(() => undefined);
    await expect.poll(() => app.output().slice(before.length)).toMatch(/refused a link \(userinfo\)/);
    expect(app.output()).not.toContain('example.com');
  });
});

// Enter on a focused link is the keyboard's way to open it: main asks before it
// does. Its own app, since the message box ends the headless one.
test.describe('a link opened with the keyboard', () => {
  test('asks first: main logs that it is confirming, and has opened nothing', async () => {
    const app = launch({ debugPort: true });
    const { browser, page } = await attach(app);
    try {
      await page.evaluate(() => {
        const a = document.createElement('a');
        a.id = 'kb-link';
        a.href = 'https://example.com/keyboard';
        a.target = '_blank';
        a.rel = 'noopener noreferrer';
        a.textContent = 'keyboard link';
        document.body.prepend(a);
      });
      await page.bringToFront();
      await page.waitForTimeout(1_500);
      page.locator('#kb-link').press('Enter').catch(() => undefined);
      await expect.poll(() => app.output()).toContain('confirming a link opened with the keyboard');
      expect(app.output()).not.toContain('could not open a link');
      expect(app.output()).not.toContain('example.com');
    } finally {
      await browser.close().catch(() => undefined);
      await app.stop();
    }
  });
});
