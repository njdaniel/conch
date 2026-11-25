// Staying signed in across a restart, in the packaged app (issue #2077),
// against a disposable conchd and a REAL Secret Service: an isolated
// gnome-keyring on a private session bus (./keyring.ts), never the
// developer's own keyring or bus.
//
// What it proves: with an unlocked keyring and "Keep me signed in on this
// computer" on, a member's password sign-in is sealed with the keyring's key
// (v11) and nothing readable is left on disk; the app quits and starts again
// signed in, with no password; sign-out deletes the entry, and the next start
// asks again. With the choice off, or with no Secret Service at all, nothing is
// written, the sign-in form says why, and the next start asks again.
//
// What it does not prove: Nick's desktop (COSMIC, his gnome-keyring, a .deb
// install, a keyring unlocked by PAM at login, a prompt to unlock one that is
// locked), KWallet, the XDG Secret portal, or a server revoking a kept sign-in
// between runs (the unit tests cover that). The desktop name is GNOME here,
// which Chromium maps to libsecret as it does COSMIC (the #2077 diagnosis,
// summarised in docs/design/linux-desktop.md §4.1).
//
// It needs dbus-daemon, gnome-keyring-daemon and busctl. Without them it is
// skipped and says so; CI's desktop-smoke job installs gnome-keyring and dbus
// so it runs there.
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';

import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { startConchd, type Disposable } from './conchd.ts';
import { secretBusMissing, startSecretBus, type SecretBus } from './keyring.ts';
import { launch, livePage, type Launched } from './launch.ts';

const missing = secretBusMissing();

const bar = (page: Page) => page.getByRole('banner', { name: 'Server' });
const keep = (page: Page) => page.getByRole('checkbox', { name: 'Keep me signed in on this computer' });
const staying = (page: Page) => page.getByRole('status', { name: 'Staying signed in' });
// The returning member's password form (web/src/SignIn.tsx), not any field
// labelled Password: the signed-in client has its own (password change).
const signInForm = (page: Page) => page.locator('form#password-sign-in');

function everyFile(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...everyFile(path));
    else if (entry.isFile()) out.push(path);
  }
  return out;
}

function vaultFiles(userData: string): string[] {
  const vault = join(userData, 'vault');
  return existsSync(vault) ? everyFile(vault) : [];
}

// No file under the data root holds the password. (The bearer itself never
// reaches the test; that the one credential file is v11-sealed is checked
// where it is written.)
function nothingReadable(userData: string, secrets: readonly string[]): void {
  for (const file of everyFile(userData)) {
    const bytes = readFileSync(file);
    for (const secret of secrets) expect(bytes.includes(secret), file).toBe(false);
  }
}

interface Run {
  readonly app: Launched;
  readonly browser: Browser;
  readonly page: Page;
}

async function start(userData: string, bus: SecretBus): Promise<Run> {
  const app = launch({ debugPort: true, userData, env: bus.env });
  const browser = await chromium.connectOverCDP(await app.cdpUrl());
  return { app, browser, page: await livePage(browser) };
}

async function quit(run: Run): Promise<void> {
  await run.browser.close();
  await run.app.stop();
}

async function addServer(run: Run, origin: string): Promise<Page> {
  await run.page.getByLabel('Server address').fill(origin);
  void run.page
    .getByRole('button', { name: 'Continue' })
    .click()
    .catch(() => undefined);
  return livePage(run.browser, run.page);
}

async function signIn(page: Page, name: string, password: string): Promise<void> {
  await page.getByLabel('Name', { exact: true }).fill(name);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
}

async function signOut(run: Run, page: Page): Promise<Page> {
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  void page
    .getByRole('button', { name: 'Yes, sign out', exact: true })
    .click()
    .catch(() => undefined);
  return livePage(run.browser, page);
}

test.describe('staying signed in across a restart (issue #2077)', () => {
  test.skip(missing !== null, missing ?? '');

  let server: Disposable;
  let userData: string;
  let bus: SecretBus | null = null;
  let run: Run | null = null;

  test.beforeAll(async () => {
    test.setTimeout(240_000);
    server = await startConchd();
  });

  test.afterAll(async () => {
    await server.stop();
  });

  test.beforeEach(() => {
    userData = mkdtempSync(join(existsSync('/tmp') ? '/tmp' : '.', 'conch-desktop-persist-'));
  });

  test.afterEach(async () => {
    if (run !== null) await quit(run).catch(() => undefined);
    run = null;
    await bus?.stop();
    bus = null;
    rmSync(userData, { recursive: true, force: true });
  });

  test('with an unlocked keyring and the choice on: kept, the relaunch is signed in without a password, sign-out ends it', async () => {
    test.setTimeout(180_000);
    bus = await startSecretBus({ keyring: 'unlocked' });
    run = await start(userData, bus);
    let page = await addServer(run, server.origin);
    await expect(keep(page)).toBeChecked();
    await expect(keep(page)).toBeEnabled();
    await expect(staying(page)).toContainText('Your sign-in will be kept in the system keyring.');
    await signIn(page, server.member.name, server.member.password);
    await expect(bar(page)).toContainText(server.member.name);
    await expect(bar(page)).not.toContainText('until the app closes');

    // One credential, sealed with the keyring's key; the password nowhere.
    const sealed = vaultFiles(userData);
    expect(sealed).toHaveLength(1);
    expect(readFileSync(sealed[0] ?? '').subarray(0, 3).toString('latin1')).toBe('v11');
    nothingReadable(userData, [server.member.password]);

    await quit(run);
    run = await start(userData, bus);
    page = run.page;
    // Straight into the client: no sign-in form, no password.
    await expect(page.getByRole('heading', { name: 'general' })).toBeVisible();
    await expect(bar(page)).toContainText(server.member.name);
    await expect(signInForm(page)).toHaveCount(0);

    page = await signOut(run, page);
    await expect(signInForm(page)).toBeVisible();
    await expect.poll(() => vaultFiles(userData)).toEqual([]);

    await quit(run);
    run = await start(userData, bus);
    await expect(signInForm(run.page)).toBeVisible();
    await expect(run.page.getByRole('heading', { name: 'general' })).toHaveCount(0);
  });

  test('with the choice off: nothing is written, and the relaunch asks again', async () => {
    test.setTimeout(180_000);
    bus = await startSecretBus({ keyring: 'unlocked' });
    run = await start(userData, bus);
    const page = await addServer(run, server.origin);
    await keep(page).uncheck();
    await expect(keep(page)).not.toBeChecked();
    await signIn(page, server.member.name, server.member.password);
    expect(vaultFiles(userData)).toEqual([]);
    nothingReadable(userData, [server.member.password]);

    await quit(run);
    run = await start(userData, bus);
    await expect(signInForm(run.page)).toBeVisible();
    // A new window: the choice is on again by default.
    await expect(keep(run.page)).toBeChecked();
  });

  test('with no Secret Service: the sign-in form says no keyring is unlocked, nothing is written, and the relaunch asks again', async () => {
    test.setTimeout(180_000);
    bus = await startSecretBus({ keyring: 'none' });
    run = await start(userData, bus);
    const page = await addServer(run, server.origin);
    await expect(staying(page)).toContainText('No system keyring is unlocked, so you’ll be asked to sign in each time Conch starts.');
    await expect(keep(page)).not.toBeChecked();
    await expect(keep(page)).toBeDisabled();
    await signIn(page, server.member.name, server.member.password);
    await expect(bar(page)).toContainText('until the app closes');
    // Not even a ciphertext sealed with Chromium's built-in key (v10), which
    // the build before #2077 wrote here.
    expect(vaultFiles(userData)).toEqual([]);
    nothingReadable(userData, [server.member.password]);

    await quit(run);
    run = await start(userData, bus);
    await expect(signInForm(run.page)).toBeVisible();
    await expect(run.page.getByRole('heading', { name: 'general' })).toHaveCount(0);
  });
});
