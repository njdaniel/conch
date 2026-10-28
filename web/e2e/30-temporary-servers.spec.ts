// Temporary servers and password-only operators in a real browser (ADR-011
// items 2.3 and 3, ADR-012 item 5, issue #867). Each test starts a conchd of
// its own on a data directory under the run's root (as 23-operator-auth does),
// with the mode under test, makes its first operator with `conchd operator
// bootstrap --link --origin`, and opens that link in the page:
//
//   (a) CONCHD_OPERATOR_TOTP=optional: the link opens Setup with the token
//       filled in and nothing spent, the click redeems it, a password, Skip,
//       then a sign-in with name and password alone, and no banner anywhere;
//   (b) CONCHD_MODE=temporary: the same, with the banner on the sign-in page
//       and in the lobby;
//   (c) a normal server: setup still asks for the authenticator, no Skip.
//
// Only the process each test started is stopped; the shared server is never
// touched.
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { closeSync, mkdirSync, openSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Page } from '@playwright/test';
import { expect, statusLine, test } from './fixtures';
import { loadWorld, repoRoot, safePath, type World } from './world';

const operator = 'tmp-root';
const password = 'the temporary operator passphrase of the browser suite';
const banner = 'Temporary server: no backups, and it may be removed. Don’t keep anything here you need.';
// The origin the host prints its link for; the page is opened at the real one.
const printedOrigin = 'https://setup-link.invalid';

interface Server {
  readonly appUrl: string;
  readonly token: string;
  readonly process: ChildProcess;
}

function serverEnv(world: World, extra: Record<string, string>): Record<string, string | undefined> {
  const home = join(world.root, 'home');
  const key = btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))));
  return {
    PATH: safePath(),
    HOME: home,
    TMPDIR: join(world.root, 'tmp'),
    XDG_CONFIG_HOME: join(home, 'config'),
    XDG_DATA_HOME: join(home, 'data'),
    XDG_STATE_HOME: join(home, 'state'),
    XDG_CACHE_HOME: join(home, 'cache'),
    CONCHD_TOTP_KEY: key,
    ...extra,
  };
}

async function healthy(origin: string): Promise<boolean> {
  try {
    return (await fetch(`${origin}/healthz`, { signal: AbortSignal.timeout(2_000) })).ok;
  } catch {
    return false;
  }
}

async function startServer(label: string, extra: Record<string, string>): Promise<Server> {
  const world = loadWorld();
  const dir = join(world.root, `tmp-${label}-${String(Date.now())}`);
  const data = join(dir, 'data');
  mkdirSync(dir, { recursive: true });
  const env = serverEnv(world, extra);
  const conchd = join(world.root, 'bin', 'conchd');
  const made = spawnSync(conchd, ['operator', 'bootstrap', '--data', data, '--link', '--origin', printedOrigin, operator], { cwd: dir, env, encoding: 'utf8', timeout: 30_000 });
  if (made.status !== 0) throw new Error(`conchd operator bootstrap failed: ${made.stderr}`);
  const link = made.stdout.trim();
  const token = link.split('/app/#/setup/')[1];
  if (token === undefined || !token.startsWith('conch_setup_')) throw new Error('conchd operator bootstrap --link printed no setup link');

  const logPath = join(dir, 'conchd.log');
  const log = openSync(logPath, 'a');
  const child = spawn(conchd, ['serve', '--listen', '127.0.0.1:0', '--web-dir', join(repoRoot, 'web', 'dist'), '--data', data], {
    cwd: dir,
    env,
    stdio: ['ignore', log, log],
  });
  closeSync(log);
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const address = /conchd \S+ listening on (\S+:\d+) /.exec(readFileSync(logPath, 'utf8'))?.[1];
    if (address !== undefined && (await healthy(`http://${address}`))) {
      return { appUrl: `http://${address}/app/`, token, process: child };
    }
    if (child.exitCode !== null) break;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  child.kill('SIGTERM');
  throw new Error(`the ${label} conchd did not become healthy:\n${readFileSync(logPath, 'utf8')}`);
}

const tabToken = (page: Page) => page.evaluate(() => window.sessionStorage.getItem('conch.web.token'));

// setUpFromLink opens the setup link, checks that the address is clean and the
// field filled, and that nothing has been spent (the token still redeems on
// the click), then redeems and sets the password.
async function setUpFromLink(page: Page, server: Server): Promise<void> {
  const redeems: string[] = [];
  page.on('request', (request) => {
    if (request.url().includes('/v1/setup/redeem')) redeems.push(request.url());
  });
  await page.goto(`${server.appUrl}#/setup/${server.token}`);
  await expect(page.getByRole('heading', { name: 'Set up an operator account' })).toBeVisible();
  const field = page.getByLabel('Setup token');
  await expect(field).toHaveValue(server.token);
  expect(page.url()).not.toContain('setup');
  expect(page.url()).not.toContain('#');
  // Loading the page spends nothing: the token goes to the server on the
  // click alone.
  await page.waitForLoadState('networkidle');
  expect(redeems).toEqual([]);
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByLabel('Choose a password').fill(password);
  await page.getByLabel('Type it again').fill(password);
  await page.getByRole('button', { name: 'Set password' }).click();
  await expect(page.getByRole('heading', { name: 'Your authenticator app' })).toBeVisible();
}

async function skipAndSignIn(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Skip' }).click();
  await expect(page.getByRole('heading', { name: 'Setup is complete' })).toBeVisible();
  await expect(page.getByText(/Sign in with your name and your password\./)).toBeVisible();
  await page.getByRole('button', { name: 'Go to sign-in' }).click();
  // The setup credential is gone with Skip: the page holds no sign-in.
  expect(await tabToken(page)).toBeNull();
  await page.getByLabel('Name', { exact: true }).fill(operator);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(statusLine(page)).toContainText(operator);
  await expect(page.getByLabel('Code from your authenticator app')).toHaveCount(0);
  await expect(page.getByRole('heading', { name: 'This is an operator credential' })).toHaveCount(0);
}

test('G30a with CONCHD_OPERATOR_TOTP=optional an operator follows the setup link, skips the authenticator, and signs in with a password alone; no banner', async ({ meet, secrets }) => {
  test.setTimeout(150_000);
  const server = await startServer('optional', { CONCHD_OPERATOR_TOTP: 'optional' });
  try {
    secrets.push(server.token, password);
    const { page } = await meet();
    await page.goto(server.appUrl);
    await expect(page.getByText(/Version/)).toBeVisible();
    await expect(page.getByText(banner)).toHaveCount(0);

    await setUpFromLink(page, server);
    await skipAndSignIn(page);
    const token = await tabToken(page);
    secrets.push(token ?? '');
    await expect(page.getByText(banner)).toHaveCount(0);
    await page.getByRole('button', { name: 'Settings' }).click();
    await expect(page.getByText(/Version/)).toBeVisible();
    await expect(page.getByText(banner)).toHaveCount(0);
  } finally {
    server.process.kill('SIGTERM');
  }
});

test('G30b with CONCHD_MODE=temporary the same path shows the banner on the sign-in page and in the lobby', async ({ meet, secrets }) => {
  test.setTimeout(150_000);
  const server = await startServer('temporary', { CONCHD_MODE: 'temporary' });
  try {
    secrets.push(server.token, password);
    const { page } = await meet();
    await page.goto(server.appUrl);
    await expect(page.getByText(banner)).toBeVisible();

    await setUpFromLink(page, server);
    await skipAndSignIn(page);
    secrets.push((await tabToken(page)) ?? '');
    await page.getByRole('button', { name: 'Settings' }).click();
    await expect(page.getByText(banner)).toBeVisible();
    // Fixed: nothing on it dismisses it.
    await expect(page.getByText(banner).getByRole('button')).toHaveCount(0);
  } finally {
    server.process.kill('SIGTERM');
  }
});

test('G30c on a normal server setup still asks for the authenticator, with no Skip, and no banner', async ({ meet, secrets }) => {
  test.setTimeout(150_000);
  const server = await startServer('normal', {});
  try {
    secrets.push(server.token, password);
    const { page } = await meet();
    await page.goto(server.appUrl);
    await expect(page.getByText(banner)).toHaveCount(0);
    await setUpFromLink(page, server);
    await expect(page.getByRole('button', { name: 'Show the key' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Skip' })).toHaveCount(0);
    await expect(page.getByText(banner)).toHaveCount(0);
  } finally {
    server.process.kill('SIGTERM');
  }
});
