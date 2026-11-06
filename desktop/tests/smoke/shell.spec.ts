// The packaged smoke test (issue #940): the fused app built by
// `make desktop-build`, started as a user would start it, driven over
// Chromium's remote-debugging port. See launch.ts for why that route.
//
// Needs a display (CI: xvfb-run) and the unpacked build in dist/. It proves
// what ran here: this machine's Electron, sandbox and window; it does not
// certify an installed package, a keyring or a picker (#774, #922).
import { readFileSync } from 'node:fs';

import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { launch, type Launched } from './launch.ts';

// The pid of a process in each pid namespace it is in, outermost first.
function nspid(pid: number): string[] {
  const status = readFileSync(`/proc/${String(pid)}/status`, 'utf8');
  const line = /^NSpid:\s*(.+)$/m.exec(status)?.[1];
  if (line === undefined) throw new Error(`/proc/${String(pid)}/status has no NSpid line`);
  return line.trim().split(/\s+/);
}

async function attach(app: Launched): Promise<{ browser: Browser; page: Page }> {
  const browser = await chromium.connectOverCDP(await app.cdpUrl());
  const context = browser.contexts()[0];
  if (context === undefined) throw new Error('the app has no browser context');
  const page = context.pages()[0] ?? (await context.waitForEvent('page'));
  await page.waitForLoadState('load');
  return { browser, page };
}

test.describe('the packaged shell', () => {
  let app: Launched;
  let browser: Browser;
  let page: Page;

  test.beforeAll(async () => {
    app = launch({ debugPort: true });
    ({ browser, page } = await attach(app));
    // The first receipt: what this build really is, into the job log.
    console.log(`receipt: ${app.output().split('\n').filter((l) => l.startsWith('conch-desktop: versions')).join('')}`);
    console.log(`receipt: browser ${browser.version()}`);
  });

  test.afterAll(async () => {
    await browser.close();
    await app.stop();
  });

  test('opens a window titled Conch with the client rendered', async () => {
    expect(await page.title()).toBe('Conch');
    expect(page.url()).toMatch(/^conch-app:\/\/ui\//);
    await expect(page.locator('#root > *').first()).toBeAttached();
  });

  test('serves the document with the CSP, nosniff and the manifest type', async () => {
    const [response] = await Promise.all([page.waitForResponse((r) => r.url() === 'conch-app://ui/'), page.reload()]);
    expect(response.status()).toBe(200);
    const h = response.headers();
    expect(h['content-security-policy']).toBe(
      "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' blob: data:; connect-src 'none'; object-src 'none'; base-uri 'none'; frame-src 'none'; frame-ancestors 'none'; form-action 'none'; worker-src 'none'",
    );
    expect(h['x-content-type-options']).toBe('nosniff');
    expect(h['content-type']).toBe('text/html; charset=utf-8');
    await expect(page.locator('#root > *').first()).toBeAttached();
  });

  test('gives the renderer no Node and a conchDesktop of named functions only', async () => {
    expect(await page.evaluate('[typeof require, typeof process, typeof Buffer, typeof module]')).toEqual(['undefined', 'undefined', 'undefined', 'undefined']);
    expect(await page.evaluate('[typeof conchDesktop, Object.keys(conchDesktop).sort(), Object.values(conchDesktop).every((v) => typeof v === "function")]')).toEqual([
      'object',
      [
        'createAccount', 'joinAsGuest', 'keepCredential', 'redeemInvite', 'redeemInviteAsGuest', 'redeemSetupToken', 'redeemSignInLink', 'request', 'restore', 'resetPassword',
        'serverAddress', 'signInWithCode', 'signInWithCredential', 'signInWithPassword', 'signOut', 'subscribe', 'uploadFile', 'downloadFile', 'saveFile',
        'profiles', 'profileAdd', 'profileRename', 'profileRemove', 'profileSwitch', 'storageRetry', 'profilesRecover',
      ].sort(),
      true,
    ]);
    // No ipcRenderer, no event object, no way to name a channel.
    expect(await page.evaluate('[typeof ipcRenderer, typeof electron, typeof require, "send" in conchDesktop, "invoke" in conchDesktop]')).toEqual(['undefined', 'undefined', 'undefined', false, false]);
  });

  test('refuses to navigate away', async () => {
    const before = page.url();
    await page.evaluate("location.href = 'https://example.com/'");
    await page.waitForTimeout(1_000);
    expect(page.url()).toBe(before);
    expect(page.url()).toMatch(/^conch-app:\/\/ui\//);
  });

  test('opens no window', async () => {
    expect(await page.evaluate("window.open('https://example.com/') === null")).toBe(true);
    expect(await page.evaluate("window.open('conch-app://ui/') === null")).toBe(true);
    await page.waitForTimeout(500);
    expect(browser.contexts().flatMap((c) => c.pages())).toHaveLength(1);
  });

  test('serves only what the manifest lists', async () => {
    const load = (urls: string[]): Promise<boolean[]> =>
      page.evaluate(
        (list) =>
          Promise.all(
            list.map(
              (u) =>
                new Promise<boolean>((done) => {
                  const img = new Image();
                  img.onload = () => {
                    done(true);
                  };
                  img.onerror = () => {
                    done(false);
                  };
                  img.src = u;
                }),
            ),
          ),
        urls,
      );
    expect(await load(['conch-app://ui/favicon-32.png'])).toEqual([true]);
    const refused = [
      'conch-app://other/favicon-32.png',
      'conch-app://ui/favicon-32.png?x=1',
      'conch-app://ui/favicon-32.png%00',
      'conch-app://ui/%66avicon-32.png',
      'conch-app://ui/assets%2f..%2ffavicon-32.png',
      'conch-app://ui/assets%5c..%5cfavicon-32.png',
      'conch-app://ui/not-in-the-manifest.png',
      'conch-app://ui/ui-manifest.json',
      'https://example.com/favicon-32.png',
    ];
    expect(await load(refused)).toEqual(refused.map(() => false));
  });

  test('never sees a port or userinfo: Chromium drops them from this scheme before main is asked', async () => {
    // The scheme is host-only, so conch-app://ui:8080/x and conch-app://u@ui/x
    // are parsed as conch-app://ui/x. The handler's refusal of both (unit
    // tests) is a second line, not the first.
    expect(await page.evaluate("[new URL('conch-app://ui:8080/x').href, new URL('conch-app://u:p@ui/x').href]")).toEqual(['conch-app://ui/x', 'conch-app://ui/x']);
  });

  test('makes no request that is not to its own files', async () => {
    await page.waitForTimeout(1_000);
    // Main logs one line for each request it cancels; the renderer's
    // connect-src is 'none', so a fetch never gets as far as main.
    expect(app.output()).not.toContain('blocked a request');
    expect(await page.evaluate("fetch('https://example.com/').then(() => 'sent', () => 'refused')")).toBe('refused');
  });

  test('runs the renderer in the Chromium sandbox', async () => {
    const session = await browser.newBrowserCDPSession();
    const info = await session.send('SystemInfo.getProcessInfo');
    const renderers = info.processInfo.filter((p) => p.type === 'renderer');
    expect(renderers.length).toBeGreaterThan(0);
    // /proc/<pid>/status is readable for any process; /proc/<pid>/ns/* is not
    // for a non-dumpable one, which the setuid sandbox makes the renderer.
    // Its NSpid line lists the pid in each pid namespace the process is in:
    // one value outside a sandbox, more than one inside the sandbox's own.
    const pid = app.child.pid;
    if (pid === undefined) throw new Error('the app has no pid');
    expect(nspid(pid), 'main is not in a nested pid namespace').toHaveLength(1);
    for (const renderer of renderers) {
      expect(nspid(renderer.id).length, `renderer ${String(renderer.id)} is in no pid namespace of its own`).toBeGreaterThan(1);
    }
  });
});

test.describe('remote debugging', () => {
  for (const flag of ['--remote-debugging-port=0', '--remote-debugging-pipe']) {
    test(`${flag} is refused without CONCH_DESKTOP_ALLOW_CDP=1`, async () => {
      const app = launch({ args: [flag], env: { CONCH_DESKTOP_ALLOW_CDP: '' } });
      try {
        const done = await Promise.race([app.exited, new Promise<null>((r) => {
          setTimeout(() => {
            r(null);
          }, 15_000);
        }),
        ]);
        expect(done, 'the app kept running').not.toBeNull();
        expect(done?.code).toBe(1);
        expect(app.output()).toContain('conch-desktop: remote debugging is ON');
        expect(app.output()).toContain('refusing to start');
        // A refused launch never opens the port (the main script runs before
        // Chromium's DevTools handler starts).
        expect(app.output()).not.toContain('DevTools listening on');
      } finally {
        await app.stop();
      }
    });
  }

  test('is announced when allowed', async () => {
    const app = launch({ debugPort: true });
    let browser: Browser | undefined;
    try {
      ({ browser } = await attach(app));
      expect(app.output()).toContain('conch-desktop: remote debugging is ON');
    } finally {
      await browser?.close();
      await app.stop();
    }
  });
});

test.describe('the fuses, by behaviour', () => {
  test('ELECTRON_RUN_AS_NODE does not turn the app into node', async () => {
    const app = launch({ env: { ELECTRON_RUN_AS_NODE: '1' }, args: ['-e', 'process.exit(7)'] });
    try {
      const early = await Promise.race([app.exited, new Promise<null>((r) => {
          setTimeout(() => {
            r(null);
          }, 4_000);
        })]);
      // Run as node, it would have evaluated the script and exited 7. It
      // starts the app instead and keeps running.
      expect(early).toBeNull();
    } finally {
      await app.stop();
    }
  });

  test('NODE_OPTIONS is ignored', async () => {
    const app = launch({ debugPort: true, env: { NODE_OPTIONS: '--require=/nonexistent/conch-smoke.js' } });
    let browser: Browser | undefined;
    try {
      ({ browser } = await attach(app));
      expect(app.output()).not.toContain('Cannot find module');
    } finally {
      await browser?.close();
      await app.stop();
    }
  });

  test('--inspect starts no Node inspector', async () => {
    const app = launch({ debugPort: true, args: ['--inspect=0'] });
    let browser: Browser | undefined;
    try {
      ({ browser } = await attach(app));
      await new Promise((r) => setTimeout(r, 1_500));
      expect(app.output()).not.toContain('Debugger listening');
    } finally {
      await browser?.close();
      await app.stop();
    }
  });
});
