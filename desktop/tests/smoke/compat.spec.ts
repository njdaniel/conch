// Server compatibility in the packaged app (issue #946), against two
// disposable conchd built from this checkout, one stamped with the release
// desktop/build/compat.json lists and one stamped with another: a server on the
// list gets the client and its sign-in; one that is not gets the unsupported
// state, no sign-in form, and a sign-in the bridge itself refuses. The app is
// started with --allow-unlisted-server, which a packaged build must ignore.
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { listedVersion, startConchd, UNLISTED_VERSION, type Disposable } from './conchd.ts';
import { launch, livePage, type Launched } from './launch.ts';

// A click that makes main replace the window: the page it was made on closes
// under the click, so the click is not awaited and the page that takes the old
// one's place is.
function swap(browser: Browser, from: Page, click: () => Promise<unknown>): Promise<Page> {
  void click().catch(() => undefined);
  return livePage(browser, from);
}

test.describe('server compatibility', () => {
  let listed: Disposable;
  let unlisted: Disposable;
  let userData: string;
  let app: Launched;
  let browser: Browser;
  let page: Page;

  test.beforeAll(async () => {
    test.setTimeout(300_000);
    listed = await startConchd({ version: listedVersion() });
    unlisted = await startConchd({ version: UNLISTED_VERSION });
    userData = mkdtempSync(join(existsSync('/tmp') ? '/tmp' : tmpdir(), 'conch-desktop-compat-'));
    app = launch({ debugPort: true, userData, args: ['--password-store=basic', '--allow-unlisted-server'] });
    browser = await chromium.connectOverCDP(await app.cdpUrl());
    page = await livePage(browser);
  });

  test.afterAll(async () => {
    await browser.close();
    await app.stop();
    await listed.stop();
    await unlisted.stop();
    rmSync(userData, { recursive: true, force: true });
  });

  test('a server whose release is listed gets the client and its sign-in', async () => {
    await page.getByLabel('Server address').fill(listed.origin);
    page = await swap(browser, page, () => page.getByRole('button', { name: 'Continue' }).click());
    await expect(page.getByLabel('Name', { exact: true })).toBeVisible();
    await expect(page.getByText(`Version ${listedVersion()}`)).toBeVisible();
    await expect(page.getByText('App 0.0.0')).toBeVisible();
    await expect(page.getByRole('note', { name: 'Development build' })).toHaveCount(0);
  });

  test('a server whose release is not listed gets the unsupported state and no sign-in, flag or not', async () => {
    await page.getByRole('button', { name: 'Servers' }).click();
    await page.getByLabel('Server address').fill(unlisted.origin);
    page = await swap(browser, page, () => page.getByRole('button', { name: 'Continue' }).click());
    await expect(page.getByText(`This server’s version (${UNLISTED_VERSION}) has not been tested with this app (0.0.0).`)).toBeVisible();
    await expect(page.getByLabel('Name', { exact: true })).toHaveCount(0);
    await expect(page.getByLabel('Password', { exact: true })).toHaveCount(0);
    // The packaged build ignored --allow-unlisted-server: no warning, no way in.
    await expect(page.getByRole('note', { name: 'Development build' })).toHaveCount(0);
    const refused = await page.evaluate(
      async (password) => (window as unknown as { conchDesktop: { signInWithPassword(r: { name: string; password: string }): Promise<unknown> } }).conchDesktop.signInWithPassword({ name: 'ada', password }),
      unlisted.member.password,
    );
    expect(refused).toEqual({ ok: false, kind: 'network', reason: 'incompatible' });
  });

  test('the person can go back to the listed server from there', async () => {
    const host = new URL(listed.origin).host;
    page = await swap(browser, page, () => page.getByRole('button', { name: `Switch to ${host}` }).click());
    await expect(page.getByLabel('Name', { exact: true })).toBeVisible();
  });
});
