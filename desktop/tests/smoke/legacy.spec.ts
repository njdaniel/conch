// The start sweep of sign-ins an older build saved with Chromium's built-in key
// (issue #2217), in the packaged app, against a disposable conchd and an
// isolated Secret Service (./keyring.ts), never the developer's own keyring,
// bus or profile.
//
// A build before #2077, on a Linux desktop whose keyring did not answer,
// sealed a member's credential with the built-in key (`v10`), which anyone who
// can read the file can open. Each test plants such a file in a throwaway data
// directory, as that build left it, and starts the app. What it proves: the
// file is gone after start, whatever the active server's state; when the
// server answers, the credential was ended at the server (it no longer
// works); when it does not, the app names the server and says the sign-in
// could not be ended there. The credential is never in the app's output.
//
// It needs dbus-daemon, gnome-keyring-daemon and busctl, as persistence.spec.ts
// does: skipped and said so without them on a developer's machine, a failure
// with CI set.
import { createCipheriv, pbkdf2Sync, randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { join } from 'node:path';

import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { startConchd, type Disposable } from './conchd.ts';
import { secretBusMissing, startSecretBus, type SecretBus } from './keyring.ts';
import { launch, livePage, type Launched } from './launch.ts';

const missing = secretBusMissing();

const bar = (page: Page) => page.getByRole('banner', { name: 'Server' });
const signInForm = (page: Page) => page.locator('form#password-sign-in');

// What a build before #2077 wrote with no keyring: Chromium's Linux `v10`
// seal, AES-128-CBC under its public built-in key (PBKDF2-SHA1 of "peanuts",
// salt "saltysalt", one round, 16 bytes; an IV of 16 spaces), after the
// three-byte mark. The key is public; that is the problem #2077 and #2217 fix.
function sealV10(text: string): Buffer {
  const key = pbkdf2Sync('peanuts', 'saltysalt', 1, 16, 'sha1');
  const cipher = createCipheriv('aes-128-cbc', key, Buffer.alloc(16, 0x20));
  return Buffer.concat([Buffer.from('v10', 'latin1'), cipher.update(text, 'utf8'), cipher.final()]);
}

// Plants one profile, active, and its v10 vault entry, with the modes the app
// itself uses (directories 0700, files 0600).
function plant(userData: string, origin: string, principal: { id: number; name: string }, token: string): string {
  const id = randomBytes(16).toString('hex');
  const profiles = { version: 1, profiles: [{ id, label: new URL(origin).host, origin, principal }], active: id };
  writeFileSync(join(userData, 'profiles.json'), JSON.stringify(profiles), { mode: 0o600 });
  mkdirSync(join(userData, 'profiles', id), { recursive: true, mode: 0o700 });
  const entry = join(userData, 'vault', 'profiles', id);
  mkdirSync(entry, { recursive: true, mode: 0o700 });
  writeFileSync(join(entry, 'credential.bin'), sealV10(token), { mode: 0o600 });
  return id;
}

function vaultFiles(userData: string): string[] {
  const vault = join(userData, 'vault');
  return existsSync(vault) ? readdirSync(vault, { recursive: true, withFileTypes: true }).filter((e) => e.isFile()).map((e) => join(e.parentPath, e.name)) : [];
}

// A loopback port that nothing listens on.
function closedPort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const probe = createServer();
    probe.on('error', reject);
    probe.listen(0, '127.0.0.1', () => {
      const address = probe.address();
      const port = typeof address === 'object' && address !== null ? address.port : 0;
      probe.close(() => {
        resolvePort(port);
      });
    });
  });
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

test.describe('a sign-in an older build saved with the built-in key (issue #2217)', () => {
  test.skip(missing !== null && process.env['CI'] === undefined, missing ?? '');

  let server: Disposable;
  let userData: string;
  let bus: SecretBus | null = null;
  let run: Run | null = null;

  test.beforeAll(async () => {
    test.setTimeout(240_000);
    if (missing !== null) throw new Error(`CI must run this spec: ${missing}`);
    server = await startConchd();
  });

  test.afterAll(async () => {
    await server.stop();
  });

  test.beforeEach(() => {
    userData = mkdtempSync(join(existsSync('/tmp') ? '/tmp' : '.', 'conch-desktop-legacy-'));
  });

  test.afterEach(async () => {
    if (run !== null) {
      await run.browser.close().catch(() => undefined);
      await run.app.stop().catch(() => undefined);
    }
    run = null;
    await bus?.stop();
    bus = null;
    rmSync(userData, { recursive: true, force: true });
  });

  test('when the server answers: the entry is ended at the server and deleted, and the app starts signed out', async () => {
    test.setTimeout(120_000);
    const token = server.member.token;
    const who = await server.api(token, 'GET', '/v1/whoami');
    expect(who.status).toBe(200);
    const principal = who.body as { id: number; name: string };
    plant(userData, server.origin, { id: principal.id, name: principal.name }, token);
    bus = await startSecretBus({ keyring: 'unlocked' });
    run = await start(userData, bus);

    await expect.poll(() => vaultFiles(userData), { timeout: 30_000 }).toEqual([]);
    // Ended at the server: the copy an older build left no longer works.
    await expect.poll(async () => (await server.api(token, 'GET', '/v1/whoami')).status, { timeout: 30_000 }).toBe(401);
    // Never used as a sign-in: the window asks for a password.
    await expect(signInForm(run.page)).toBeVisible();
    const out = run.app.output();
    expect(out).toContain('a sign-in saved by an older build (v10): ended');
    expect(out).not.toContain(token);
    await expect(bar(run.page)).not.toContainText('older Conch');
  });

  test('when the server does not answer: the entry is deleted unopened, and the app names the server it could not be ended at', async () => {
    test.setTimeout(120_000);
    const origin = `http://127.0.0.1:${String(await closedPort())}`;
    const token = 'cnch_legacy_smoke_canary_2217_not_a_real_token';
    plant(userData, origin, { id: 7, name: 'ada' }, token);
    bus = await startSecretBus({ keyring: 'unlocked' });
    run = await start(userData, bus);

    await expect.poll(() => vaultFiles(userData), { timeout: 30_000 }).toEqual([]);
    const host = new URL(origin).host;
    // A server that does not answer gets the compatibility page, with no strip:
    // the notice is in its server list, on that profile's entry.
    await expect(run.page.getByRole('region', { name: 'Servers' })).toContainText(
      `A saved sign-in for ${host} may still be active. Sign in to that server with the same account and open Settings → Your sign-ins.`,
      { timeout: 30_000 },
    );
    const out = run.app.output();
    expect(out).toContain('a sign-in saved by an older build (v10): deleted-unreachable');
    expect(out).not.toContain(token);
  });
});
