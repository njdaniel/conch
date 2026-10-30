// G7a (issue #714, design §5.3): an operator's own sign-in in a real browser,
// against a conchd of its own. The suite's community has no TOTP key, and
// its operator holds only a script's credential; this spec starts a second
// conchd, with a key, on a data directory under the run's root, makes its
// first operator with `conchd operator bootstrap` (a setup token, no script
// credential), and drives the page: the setup (password, the authenticator's
// key behind Reveal, a code to confirm), then a password that alone signs
// nobody in, a wrong code, a replayed code, a challenge that runs out of
// time, the recent proof an action asks for over a form that keeps what was
// chosen, and a session that ends under a draft, which is kept for the same
// operator signing in again. The authenticator is computed here (RFC 6238,
// with Web Crypto), from the key the page reveals.
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { closeSync, mkdirSync, openSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Page } from '@playwright/test';
import { joinSettingsResponseV1 } from '../src/schema/guest';
import { expect, statusLine, test } from './fixtures';
import { loadWorld, repoRoot, safePath, type World } from './world';

const operator = 'g7a-root';
const password = 'the operator passphrase of the browser suite';

// The second server: its address, its data and its process, which this
// spec started and stops.
interface Server {
  readonly origin: string;
  readonly appUrl: string;
  readonly setupToken: string;
  readonly process: ChildProcess;
}
let server: Server | undefined;

function theServer(): Server {
  if (server === undefined) throw new Error('the operator spec’s conchd did not start');
  return server;
}

// The authenticator: its key (base32, as the page shows it) and the last
// time step the server accepted, which it never accepts again.
let secret: Uint8Array<ArrayBuffer> | undefined;
let lastStep = -1;
let lastCode = '';

function base32(text: string): Uint8Array<ArrayBuffer> {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const ch of text.replace(/[\s=]/g, '').toUpperCase()) {
    const v = alphabet.indexOf(ch);
    if (v < 0) throw new Error('the key is not base32');
    bits += v.toString(2).padStart(5, '0');
  }
  const bytes: number[] = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) bytes.push(parseInt(bits.slice(i, i + 8), 2));
  return Uint8Array.from(bytes);
}

// codeAt is RFC 6238 with the server's parameters: HMAC-SHA-1, six digits,
// thirty-second steps.
async function codeAt(key: Uint8Array<ArrayBuffer>, step: number): Promise<string> {
  const counter = new DataView(new ArrayBuffer(8));
  counter.setBigUint64(0, BigInt(step));
  const hmac = await crypto.subtle.importKey('raw', key, { name: 'HMAC', hash: 'SHA-1' }, false, ['sign']);
  const mac = new DataView(await crypto.subtle.sign('HMAC', hmac, counter.buffer));
  const offset = mac.getUint8(mac.byteLength - 1) & 0x0f;
  const value = (mac.getUint32(offset) & 0x7fffffff) % 1_000_000;
  return String(value).padStart(6, '0');
}

const stepNow = () => Math.floor(Date.now() / 30_000);

// freshCode is a code the server will take: of a step after the last one it
// accepted, and within a step of now. When the next one is two steps ahead,
// it waits for the clock.
async function freshCode(): Promise<string> {
  if (secret === undefined) throw new Error('no authenticator yet');
  let step = Math.max(stepNow(), lastStep + 1);
  while (step > stepNow() + 1) {
    await new Promise((resolve) => setTimeout(resolve, 1_000));
    step = Math.max(stepNow(), lastStep + 1);
  }
  lastStep = step;
  lastCode = await codeAt(secret, step);
  return lastCode;
}

// wrongCode is six digits that no step near now has.
async function wrongCode(): Promise<string> {
  const key = secret;
  if (key === undefined) throw new Error('no authenticator yet');
  const near = new Set(await Promise.all([-1, 0, 1, 2].map((d) => codeAt(key, stepNow() + d))));
  for (let n = 0; ; n++) {
    const code = String(n).padStart(6, '0');
    if (!near.has(code)) return code;
  }
}

// keyFor is the base64 of 32 random bytes: a TOTP key, as CONCHD_TOTP_KEY
// takes one.
function keyFor(): string {
  return btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))));
}

function serverEnv(world: World, key: string): Record<string, string | undefined> {
  const home = join(world.root, 'home');
  return {
    PATH: safePath(),
    HOME: home,
    TMPDIR: join(world.root, 'tmp'),
    XDG_CONFIG_HOME: join(home, 'config'),
    XDG_DATA_HOME: join(home, 'data'),
    XDG_STATE_HOME: join(home, 'state'),
    XDG_CACHE_HOME: join(home, 'cache'),
    CONCHD_TOTP_KEY: key,
  };
}

async function healthy(origin: string): Promise<boolean> {
  try {
    return (await fetch(`${origin}/healthz`, { signal: AbortSignal.timeout(2_000) })).ok;
  } catch {
    return false;
  }
}

test.beforeAll(async () => {
  const world = loadWorld();
  const dir = join(world.root, `g7a-${String(Date.now())}`);
  const data = join(dir, 'data');
  mkdirSync(dir, { recursive: true });
  const env = serverEnv(world, keyFor());
  const conchd = join(world.root, 'bin', 'conchd');
  const made = spawnSync(conchd, ['operator', 'bootstrap', '--data', data, operator], { cwd: dir, env, encoding: 'utf8', timeout: 30_000 });
  if (made.status !== 0) throw new Error(`conchd operator bootstrap failed: ${made.stderr}`);
  const setupToken = made.stdout.trim();
  if (!setupToken.startsWith('conch_setup_')) throw new Error('conchd operator bootstrap printed no setup token');

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
      server = { origin: `http://${address}`, appUrl: `http://${address}/app/`, setupToken, process: child };
      return;
    }
    if (child.exitCode !== null) break;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  child.kill('SIGTERM');
  throw new Error(`the operator spec’s conchd did not become healthy:\n${readFileSync(logPath, 'utf8')}`);
});

test.afterAll(() => {
  // The process this spec started, and no other.
  server?.process.kill('SIGTERM');
});

// The session the page keeps for this tab.
const tabToken = (page: Page) => page.evaluate(() => window.sessionStorage.getItem('conch.web.token'));
const lastingToken = (page: Page) => page.evaluate(() => window.localStorage.getItem('conch.web.token'));

async function passwordStep(page: Page): Promise<void> {
  await page.getByLabel('Name', { exact: true }).fill(operator);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByLabel('Code from your authenticator app')).toBeVisible();
}

async function codeStep(page: Page, code: string): Promise<void> {
  await page.getByLabel('Code from your authenticator app').fill(code);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
}

async function signInFully(page: Page, secrets: string[]): Promise<string> {
  await passwordStep(page);
  await codeStep(page, await freshCode());
  await expect(statusLine(page)).toContainText(operator);
  const token = await tabToken(page);
  if (token === null) throw new Error('the operator’s session was not kept in the tab');
  secrets.push(token);
  return token;
}

test('G7a an operator sets up from a host-made token, then a password alone signs nobody in, and a code does', async ({ meet, secrets }) => {
  test.setTimeout(150_000);
  const { origin, appUrl, setupToken } = theServer();
  secrets.push(setupToken, password);
  const { page } = await meet();
  await page.goto(appUrl);

  // Setup: the token, a password, the key behind Reveal, and a code.
  await page.getByRole('button', { name: 'Set up an operator account' }).click();
  await page.getByLabel('Setup token').fill(setupToken);
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByLabel('Choose a password').fill(password);
  await page.getByLabel('Type it again').fill(password);
  await page.getByRole('button', { name: 'Set password' }).click();
  await expect(page.getByRole('heading', { name: 'Your authenticator app' })).toBeVisible();
  await expect(page.getByLabel('Authenticator key')).toHaveCount(0);
  await expect(page.getByText(/it cannot know about another tab, another app/)).toBeVisible();
  await page.getByRole('button', { name: 'Show the key' }).click();
  const key = page.getByLabel('Authenticator key');
  await expect(key).toBeVisible();
  const shown = (await key.locator('dd code').textContent()) ?? '';
  secret = base32(shown);
  expect(secret.length).toBe(20);
  await expect(key).toContainText('6 digits, every 30 seconds');
  await page.getByLabel('Code the app shows now').fill(await freshCode());
  await page.getByRole('button', { name: 'Finish setup' }).click();
  await expect(page.getByRole('heading', { name: 'Setup is complete' })).toBeVisible();
  await page.getByRole('button', { name: 'Go to sign-in' }).click();
  await expect(page.getByText('Setup is complete. Sign in with your name, your password and a code')).toBeVisible();
  expect(await tabToken(page)).toBeNull();
  expect(await lastingToken(page)).toBeNull();

  // The password alone: no lobby, nothing kept.
  await passwordStep(page);
  await expect(page.getByText(/You are not signed in yet/)).toBeVisible();
  await expect(statusLine(page)).toHaveCount(0);
  await expect(page.getByRole('navigation', { name: 'Channels' })).toHaveCount(0);
  expect(await tabToken(page)).toBeNull();

  // A wrong code keeps the step; the right one signs in, for this tab alone,
  // with no operator warning.
  await codeStep(page, await wrongCode());
  await expect(page.getByText('That code is not right. Type the code the app shows now.')).toBeVisible();
  await codeStep(page, await freshCode());
  await expect(statusLine(page)).toContainText(operator);
  await expect(statusLine(page)).toContainText('Connected');
  await expect(page.getByRole('heading', { name: 'This is an operator credential' })).toHaveCount(0);
  const token = await tabToken(page);
  expect(token).not.toBeNull();
  secrets.push(token ?? '');
  expect(await lastingToken(page)).toBeNull();
  // A second tab of the same browser is not signed in.
  const other = await page.context().newPage();
  await other.goto(appUrl);
  await expect(other.getByLabel('Password', { exact: true })).toBeVisible();
  await other.close();
  const who = await fetch(`${origin}/v1/whoami`, { headers: { Authorization: `Bearer ${token ?? ''}` } });
  expect(((await who.json()) as { auth?: { idle_expires_at?: string } }).auth?.idle_expires_at).toBeDefined();
});

test('G7a a replayed code is refused, and an action that needs a recent proof asks for one over the form, which keeps what was chosen', async ({ meet, secrets }) => {
  test.setTimeout(150_000);
  const { origin, appUrl } = theServer();
  secrets.push(password);
  const { page } = await meet();
  await page.goto(appUrl);
  const used = lastCode;
  await passwordStep(page);
  // The code the last sign-in used: its step is spent.
  await codeStep(page, used);
  await expect(page.getByText('That code is not right. Type the code the app shows now.')).toBeVisible();
  await expect(statusLine(page)).toHaveCount(0);
  await codeStep(page, await freshCode());
  await expect(statusLine(page)).toContainText(operator);
  const token = (await tabToken(page)) ?? '';
  secrets.push(token);

  // Joining: pause the address (guests are removed, issue #1005, so the
  // change that asks for the proof is the pause). Save asks for the proof,
  // over the form.
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Joining' }).click();
  const region = page.getByRole('region', { name: 'Joining' });
  await expect(region.getByLabel('Allow guest visits')).toHaveCount(0);
  await region.getByLabel('Pause joining from the address').check();
  await region.getByRole('button', { name: 'Save joining' }).click();
  const dialog = page.getByRole('dialog', { name: 'Confirm it is you' });
  await expect(dialog).toBeVisible();
  await expect(region.getByLabel('Pause joining from the address')).toBeChecked();
  // Cancel first: nothing changes, and the choice is still there.
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(region.getByText('It needs your password first. Nothing was changed.')).toBeVisible();
  await expect(region.getByLabel('Pause joining from the address')).toBeChecked();
  expect(await joiningPaused(origin, token)).toBe(false);

  await region.getByRole('button', { name: 'Save joining' }).click();
  await expect(dialog).toBeVisible();
  await dialog.getByLabel('Your password').fill(password);
  await dialog.getByLabel('Code from your authenticator app').fill(await freshCode());
  await dialog.getByRole('button', { name: 'Confirm' }).click();
  await expect(dialog).toHaveCount(0);
  // The same change, sent again once the proof stood.
  await expect(region.getByText('Saved.')).toBeVisible();
  expect(await joiningPaused(origin, token)).toBe(true);
  // Within the proof's ten minutes the next change asks nothing.
  await region.getByLabel('Pause joining from the address').uncheck();
  await region.getByRole('button', { name: 'Save joining' }).click();
  await expect.poll(() => joiningPaused(origin, token)).toBe(false);
  await expect(dialog).toHaveCount(0);
});

async function joiningPaused(origin: string, token: string): Promise<boolean> {
  const reply = await fetch(`${origin}/v1/join/settings`, { headers: { Authorization: `Bearer ${token}` } });
  const body: unknown = await reply.json();
  expect(joinSettingsResponseV1(body)).toEqual([]);
  return (body as { settings: { joining_paused: boolean } }).settings.joining_paused;
}

test('G7a the code’s step runs out of time and goes back to the password, sending no code', async ({ meet, secrets }) => {
  const { appUrl } = theServer();
  secrets.push(password);
  const { page } = await meet();
  await page.clock.install();
  const codes: string[] = [];
  page.on('request', (request) => {
    if (request.url().endsWith('/v1/login/totp')) codes.push(request.url());
  });
  await page.goto(appUrl);
  await passwordStep(page);
  await page.clock.fastForward('05:01');
  await expect(page.getByText('That sign-in took too long, or had too many codes that were not right.')).toBeVisible();
  await expect(page.getByLabel('Password', { exact: true })).toBeVisible();
  expect(codes).toEqual([]);
});

test('G7a a session that ends under a draft keeps it for the same operator signing in again', async ({ meet, secrets }) => {
  test.setTimeout(150_000);
  const { origin, appUrl } = theServer();
  secrets.push(password);
  const { page } = await meet();
  await page.goto(appUrl);
  const token = await signInFully(page, secrets);
  const channel = `g7a-${String(Date.now())}`;
  const nav = page.getByRole('navigation', { name: 'Channels' });
  await nav.getByRole('button', { name: 'New channel' }).click();
  const form = page.getByRole('form', { name: 'New channel' });
  await form.getByLabel('Channel name').fill(channel);
  await form.getByRole('button', { name: 'Create channel' }).click();
  await expect(page.getByRole('heading', { name: channel, exact: true })).toBeVisible();
  const composer = page.getByLabel(`Message #${channel}`);
  await expect(composer).toBeEnabled();
  await composer.fill('g7a a thought half typed');

  // The session ends (as at its hour without use, or a revoke elsewhere).
  const revoked = await fetch(`${origin}/v1/credentials/current`, { method: 'DELETE', headers: { Authorization: `Bearer ${token}` } });
  expect(revoked.status).toBe(204);
  // Sending finds it ended, unless the page found out first (its socket is
  // closed when the credential goes): the draft is kept either way.
  await page
    .getByRole('button', { name: 'Send' })
    .click({ timeout: 2_000 })
    .catch(() => undefined);
  await expect(page.getByText(/Your operator sign-in has ended/)).toBeVisible();

  await signInFully(page, secrets);
  await page.getByRole('navigation', { name: 'Channels' }).getByRole('button', { name: channel, exact: true }).click();
  await expect(page.getByLabel(`Message #${channel}`)).toHaveValue('g7a a thought half typed');
});
