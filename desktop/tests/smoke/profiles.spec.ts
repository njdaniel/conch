// Saved server profiles in the packaged app (issue #945), against two
// disposable conchd with two accounts (same machine, separate principals):
// add both, switch, fence a delayed response and a delayed frame of the server
// that was left, remove, restart with session-only storage and look for the
// token on disk, and one instance per data root.
//
// Storage is forced session-only with --password-store=basic (Chromium's
// hardcoded-key backend, which the app classifies as not protected), so the run
// does not depend on a keyring. The protected restart is the unit tests' (a
// fake vault); a real keyring is not something a CI machine has.
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { startConchd, type Disposable } from './conchd.ts';
import { launch, livePage, type Launched } from './launch.ts';
import { delayProxy, type DelayProxy } from './proxy.ts';

const SESSION_ONLY = ['--password-store=basic'] as const;

const bar = (page: Page) => page.getByRole('banner', { name: 'Server' });
const messages = (page: Page) => page.getByRole('log', { name: 'Messages' });

function everyFile(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...everyFile(path));
    else if (entry.isFile()) out.push(path);
  }
  return out;
}

async function signIn(page: Page, name: string, password: string): Promise<void> {
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
}

// A click that makes main replace the window: the page it was made on closes
// under the click, which Playwright reports as an error, so the click is not
// awaited and the page that takes the old one's place is.
function swap(browser: Browser, from: Page, click: () => Promise<unknown>): Promise<Page> {
  void click().catch(() => undefined);
  return livePage(browser, from);
}

async function attach(app: Launched): Promise<{ browser: Browser; page: Page }> {
  const browser = await chromium.connectOverCDP(await app.cdpUrl());
  const page = await livePage(browser);
  return { browser, page };
}

test.describe('saved server profiles', () => {
  let a: Disposable;
  let b: Disposable;
  let proxyA: DelayProxy;
  let userData: string;
  let app: Launched;
  let browser: Browser;
  let page: Page;
  let hostA: string;
  let hostB: string;

  test.beforeAll(async () => {
    test.setTimeout(300_000);
    a = await startConchd();
    b = await startConchd();
    proxyA = await delayProxy(Number(new URL(a.origin).port));
    hostA = new URL(proxyA.origin).host;
    hostB = new URL(b.origin).host;
    // Under /tmp where it exists: the data root's ancestors must not be
    // writable by others (main/instance.ts).
    userData = mkdtempSync(join(existsSync('/tmp') ? '/tmp' : tmpdir(), 'conch-desktop-profiles-'));
    app = launch({ debugPort: true, userData, args: SESSION_ONLY });
    ({ browser, page } = await attach(app));
  });

  test.afterAll(async () => {
    await browser.close();
    await app.stop();
    await proxyA.stop();
    await a.stop();
    await b.stop();
    rmSync(userData, { recursive: true, force: true });
  });

  test('the data root is 0700, and says why sign-ins will not last when storage is session-only', async () => {
    await expect(page.getByRole('status', { name: 'Sign-in storage' })).toContainText('protects nothing');
    expect(statSync(userData).mode & 0o777).toBe(0o700);
  });

  test('adds the first server, and signs in with a credential the canary scan knows', async () => {
    await page.getByLabel('Server address').fill(proxyA.origin);
    page = await swap(browser, page, () => page.getByRole('button', { name: 'Continue' }).click());
    await expect(bar(page)).toContainText(hostA);
    await page.getByLabel('Credential').fill(a.member.token);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
    // The strip shows the account once the sign-in has bound the profile.
    await expect(bar(page)).toContainText(a.member.name);
    await expect(bar(page)).toContainText('until the app closes');
  });

  test('adds the second server, as another account, and the list shows both with their accounts', async () => {
    await page.getByRole('button', { name: 'Servers' }).click();
    await page.getByLabel('Server address').fill(b.origin);
    page = await swap(browser, page, () => page.getByRole('button', { name: 'Continue' }).click());
    await expect(bar(page)).toContainText(hostB);
    await expect(bar(page)).toContainText('not signed in yet');
    await signIn(page, b.other.name, b.other.password);
    await expect(bar(page)).toContainText(b.other.name);
    await page.getByRole('button', { name: 'Servers' }).click();
    const list = page.getByRole('region', { name: 'Servers' });
    await expect(list).toContainText(hostA);
    await expect(list).toContainText(a.member.name);
    await expect(list).toContainText(hostB);
    await expect(list).toContainText(b.other.name);
  });

  test('profiles.json holds no secret, and is 0600', () => {
    const file = join(userData, 'profiles.json');
    expect(statSync(file).mode & 0o777).toBe(0o600);
    const text = readFileSync(file, 'utf8');
    expect(text).not.toContain(a.member.token);
    expect(text).not.toContain(b.other.password);
    expect((JSON.parse(text) as { profiles: unknown[] }).profiles).toHaveLength(2);
  });

  test('switching replaces the window; the first server’s sign-in is still there, from main’s memory', async () => {
    page = await swap(browser, page, () => page.getByRole('button', { name: 'Switch to ' + hostA }).click());
    await expect(bar(page)).toContainText(hostA);
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
    await expect(bar(page)).toContainText(a.member.name);
  });

  test('another account on a bound server is refused, not swapped in', async () => {
    // Sign out of A, then try to sign in as the other account of A's server.
    await page.getByRole('button', { name: 'Settings' }).click();
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    page = await swap(browser, page, () => page.getByRole('button', { name: 'Yes, sign out', exact: true }).click());
    await expect(bar(page)).toContainText(hostA);
    await expect(bar(page)).toContainText(a.member.name);
    await signInRefused(page);
    // Back to the first account. (Signing out revoked the credential pasted
    // earlier, so it signs in with its password.)
    await signIn(page, a.member.name, a.member.password);
  });

  async function signInRefused(p: Page): Promise<void> {
    await p.getByLabel('Name', { exact: true }).fill(a.other.name);
    await p.getByLabel('Password', { exact: true }).fill(a.other.password);
    await p.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(bar(p)).toContainText('different account');
  }

  test('a delayed response and a delayed frame from the first server never reach the second', async () => {
    await expect(page.getByRole('status', { name: 'Status' })).toContainText('Connected');
    await page.evaluate(() => {
      window.localStorage.setItem('draft:general', 'A-secret-draft');
    });
    // A credential that works: the one pasted at the start was revoked by the
    // sign-out above, so the member signs in again over the API.
    const login = await fetch(`${a.origin}/v1/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: a.member.name, password: a.member.password }),
    });
    const fresh = ((await login.json()) as { token?: string }).token ?? '';
    expect(login.status).toBe(200);
    expect(fresh).not.toBe('');
    proxyA.hold();
    const before = proxyA.heldBytes();
    // Things only A knows, posted straight to A: the frame is held on its way
    // to the app, and so is the answer to the request the page makes now.
    const made = await a.api(a.operatorToken, 'POST', '/v0/channels', { name: 'a-only-channel' });
    expect(made.status).toBeLessThan(300);
    const posted = await a.api(fresh, 'POST', '/v2/channels/general/messages', { body: 'A-late-frame' });
    expect(posted.status).toBeLessThan(300);
    void page.evaluate(() => void window.conchDesktop?.request({ operation: 'listChannels' }));
    // The proxy has the frame (and the answer) in hand before the switch.
    await expect.poll(() => proxyA.heldBytes()).toBeGreaterThan(before);
    await expect.poll(() => proxyA.heldText()).toContain('A-late-frame');
    await page.getByRole('button', { name: 'Servers' }).click();
    page = await swap(browser, page, () => page.getByRole('button', { name: 'Switch to ' + hostB }).click());
    await expect(bar(page)).toContainText(hostB);
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
    // What A had been holding back now arrives, on connections main has closed.
    proxyA.release();
    await new Promise((r) => setTimeout(r, 2_000));
    const body = await page.evaluate(() => document.body.innerText);
    expect(body).not.toContain('A-late-frame');
    expect(body).not.toContain('a-only-channel');
    await expect(messages(page).getByText('A-late-frame')).toHaveCount(0);
    // A's drafts and preferences are not in B's page.
    expect(await page.evaluate(() => [window.localStorage.length, window.localStorage.getItem('draft:general')])).toEqual([0, null]);
    expect(app.output()).not.toContain('A-late-frame');
  });

  test('removing a server asks first, then deletes it and its directory; the other is untouched', async () => {
    const ids = (JSON.parse(readFileSync(join(userData, 'profiles.json'), 'utf8')) as { profiles: { id: string; origin: string }[] }).profiles;
    const bId = ids.find((p) => p.origin === b.origin)?.id ?? '';
    const aId = ids.find((p) => p.origin === proxyA.origin)?.id ?? '';
    expect(bId).toMatch(/^[0-9a-f]{32}$/);
    expect(existsSync(join(userData, 'profiles', bId))).toBe(true);
    await page.getByRole('button', { name: 'Servers' }).click();
    await page.getByRole('button', { name: `Remove ${hostB}…` }).click();
    await expect(page.getByRole('alertdialog')).toContainText('deletes its saved sign-in');
    expect(existsSync(join(userData, 'profiles', bId))).toBe(true);
    // B was the active server: removing it ends it, and the list is shown.
    page = await swap(browser, page, () => page.getByRole('button', { name: `Remove ${hostB}`, exact: true }).click());
    await expect(page.getByRole('region', { name: 'Servers' })).not.toContainText(hostB);
    await expect(page.getByRole('region', { name: 'Servers' })).toContainText(hostA);
    expect(existsSync(join(userData, 'profiles', bId))).toBe(false);
    expect(existsSync(join(userData, 'profiles', aId))).toBe(true);
    expect(readFileSync(join(userData, 'profiles.json'), 'utf8')).not.toContain(bId);
  });

  test('a restart on session-only storage asks to sign in again, says why, and leaves no token bytes under the data root', async () => {
    await browser.close();
    await app.stop();
    const files = everyFile(userData);
    expect(files.length).toBeGreaterThan(0);
    for (const file of files) {
      const bytes = readFileSync(file);
      expect(bytes.includes(a.member.token), file).toBe(false);
      expect(bytes.includes(b.other.password), file).toBe(false);
    }
    // Nothing of the vault was written either.
    expect(existsSync(join(userData, 'vault')) ? readdirSync(join(userData, 'vault')) : []).toEqual([]);

    app = launch({ debugPort: true, userData, args: SESSION_ONLY });
    ({ browser, page } = await attach(app));
    await expect(page.getByRole('status', { name: 'Sign-in storage' })).toContainText('You will sign in again next time');
    page = await swap(browser, page, () => page.getByRole('button', { name: 'Switch to ' + hostA }).click());
    await expect(bar(page)).toContainText(hostA);
    await expect(page.getByLabel('Credential')).toBeVisible();
    // The reason and the Retry are in the server list.
    await page.getByRole('button', { name: 'Servers' }).click();
    await expect(page.getByRole('status', { name: 'Sign-in storage' })).toContainText('You will sign in again next time');
    await page.getByRole('button', { name: 'Retry' }).click();
    await expect(page.getByRole('status', { name: 'Sign-in storage' })).toContainText('protects nothing');
  });
});

test.describe('one instance per data root', () => {
  test('a second launch on the same root exits at once; another root runs on its own; the first keeps its root', async () => {
    test.setTimeout(120_000);
    const root = mkdtempSync(join(existsSync('/tmp') ? '/tmp' : tmpdir(), 'conch-desktop-single-'));
    mkdirSync(root, { recursive: true });
    const first = launch({ userData: root, args: SESSION_ONLY });
    let second: Launched | undefined;
    let other: Launched | undefined;
    try {
      await expect.poll(() => first.output(), { timeout: 30_000 }).toContain('conch-desktop: versions');
      second = launch({ userData: root, args: SESSION_ONLY });
      const gone = await second.exited;
      expect(gone.code).toBe(0);
      // The first is still running, and the second wrote nothing under the root.
      expect(first.child.exitCode).toBeNull();
      expect(second.output()).not.toContain('conch-desktop: versions');
      other = launch({ args: SESSION_ONLY });
      await expect.poll(() => other?.output() ?? '', { timeout: 30_000 }).toContain('conch-desktop: versions');
      await new Promise((r) => setTimeout(r, 2_000));
      expect(other.child.exitCode).toBeNull();
      expect(first.child.exitCode).toBeNull();
    } finally {
      await other?.stop();
      await first.stop();
      rmSync(root, { recursive: true, force: true });
    }
  });
});
