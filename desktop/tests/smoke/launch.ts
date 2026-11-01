// Starts the packaged, fused app for the smoke tests (Q4 of #770).
//
// Why not Playwright's _electron.launch: it waits for the Node inspector
// ("Debugger listening on"), which the EnableNodeCliInspectArguments fuse
// turns off in the shipped build, and the shipped build is what is under test.
// So the app is started as a user would start it, with Chromium's own
// --remote-debugging-port=0 (an OS-chosen port on 127.0.0.1) added by the test
// that needs it, and driven with chromium.connectOverCDP. The app itself never
// turns that port on; a test that does not ask for it gets none.
import { spawn, type ChildProcess } from 'node:child_process';
import type { Browser, Page } from '@playwright/test';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

export const APP = resolve(import.meta.dirname, '../../dist/linux-unpacked/conch');

export interface Launched {
  readonly child: ChildProcess;
  readonly output: () => string;
  // The browser-level DevTools WebSocket URL; only with debugPort.
  readonly cdpUrl: () => Promise<string>;
  readonly exited: Promise<{ code: number | null; signal: NodeJS.Signals | null }>;
  readonly stop: () => Promise<void>;
}

export interface LaunchOptions {
  // Use this user data directory (it is not removed on stop), as a restart does.
  readonly userData?: string;
  readonly args?: readonly string[];
  readonly env?: Readonly<Record<string, string>>;
  readonly debugPort?: boolean;
}

export function launch(options: LaunchOptions = {}): Launched {
  if (!existsSync(APP)) {
    throw new Error(`${APP} does not exist; run make desktop-build first`);
  }
  // Under /tmp where it exists: the data root must have ancestors that others
  // cannot write to (main/instance.ts), and a developer's $TMPDIR may not.
  const userData = options.userData ?? mkdtempSync(join(existsSync('/tmp') ? '/tmp' : tmpdir(), 'conch-desktop-smoke-'));
  // CI runs under xvfb-run. CONCH_SMOKE_HEADLESS=1 is for a machine with no
  // X server to hand: Chromium's headless Ozone platform, no GPU, and no
  // display variable to find a real one by. The sandbox is the same.
  const headless = process.env['CONCH_SMOKE_HEADLESS'] === '1';
  const args = [
    `--user-data-dir=${userData}`,
    ...(headless ? ['--ozone-platform=headless', '--disable-gpu'] : []),
    ...(options.debugPort === true ? ['--remote-debugging-port=0'] : []),
    ...(options.args ?? []),
  ];
  // A packaged app refuses remote debugging unless told it is meant
  // (main/debugging.ts); a test that asks for the port is telling it.
  const env: NodeJS.ProcessEnv = { ...process.env, ...(options.debugPort === true ? { CONCH_DESKTOP_ALLOW_CDP: '1' } : {}), ...options.env };
  if (headless) {
    delete env['DISPLAY'];
    delete env['WAYLAND_DISPLAY'];
  }
  const child = spawn(APP, args, { env, stdio: ['ignore', 'pipe', 'pipe'] });

  let text = '';
  const append = (chunk: Buffer): void => {
    text += chunk.toString('utf8');
  };
  child.stdout.on('data', append);
  child.stderr.on('data', append);

  const exited = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((done) => {
    child.on('exit', (code, signal) => {
      done({ code, signal });
    });
  });

  const cdpUrl = async (): Promise<string> => {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const m = /DevTools listening on (ws:\/\/127\.0\.0\.1:\d+\/devtools\/browser\/[0-9a-f-]+)/.exec(text);
      if (m?.[1] !== undefined) return m[1];
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error(`no "DevTools listening on" line within 30 s; output:\n${text}`);
  };

  const stop = async (): Promise<void> => {
    if (child.exitCode === null && child.signalCode === null) {
      child.kill('SIGTERM');
      const timer = setTimeout(() => child.kill('SIGKILL'), 5_000);
      await exited;
      clearTimeout(timer);
    }
    if (options.userData === undefined) rmSync(userData, { recursive: true, force: true });
  };

  return { child, output: () => text, cdpUrl, exited, stop };
}

// livePage is the page of the app's current window. A switch, a sign-out and an
// add each replace the window (a new web contents in a new in-memory session),
// so a test that did one waits here for the page that took the old one's place.
export async function livePage(browser: Browser, not?: Page): Promise<Page> {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    for (const page of browser.contexts().flatMap((c) => c.pages())) {
      if (page !== not && !page.isClosed() && page.url().startsWith('conch-app://ui/')) {
        await page.waitForLoadState('load');
        return page;
      }
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error('no app window within 30 s');
}
