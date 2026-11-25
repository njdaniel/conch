// The packaged renderer with a connected helper transcript. The helper is
// replaced only in a private copy, so this needs neither LiveKit nor audio.
import { chmodSync, constants, cpSync, readdirSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { chromium, expect, test } from '@playwright/test';
import { fixture, scratch } from '../unit/voice-support.ts';
import { startConchd } from './conchd.ts';
import { APP, launch, livePage } from './launch.ts';

// WAYLAND_DISPLAY is resolved against XDG_RUNTIME_DIR unless it is absolute.
const waylandAt = (env: NodeJS.ProcessEnv): NodeJS.ProcessEnv => {
  const display = env['WAYLAND_DISPLAY'];
  const run = env['XDG_RUNTIME_DIR'];
  if (display === undefined || display === '' || display.startsWith('/') || run === undefined) return {};
  return { WAYLAND_DISPLAY: join(run, display) };
};

test('Voice options keeps the dock in reach in the packaged desktop strip (#2195)', async () => {
  test.setTimeout(240_000);
  const files = scratch('/tmp');
  try {
    const appDir = join(files.root, 'app');
    cpSync(dirname(APP), appDir, { recursive: true, mode: constants.COPYFILE_FICLONE });
    files.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6 });
    cpSync(join(files.resources, 'conch-voice'), join(appDir, 'resources', 'conch-voice'));
    // A build made under a group-writable umask is untrusted as a helper's
    // ancestor; close the copy's directories to group and others.
    const close = (dir: string): void => {
      chmodSync(dir, 0o755);
      for (const e of readdirSync(dir, { withFileTypes: true })) if (e.isDirectory()) close(join(dir, e.name));
    };
    close(appDir);
    const server = await startConchd();
    const app = launch({
      debugPort: true,
      executable: join(appDir, basename(APP)),
      // The helper's socket lives in a private runtime directory. A Wayland
      // display named relative to the real one (conch-headless) is passed by
      // its absolute path, so the app still finds it; CI runs under X11.
      env: { XDG_RUNTIME_DIR: files.run, ...waylandAt(process.env) },
    });
    try {
      const browser = await chromium.connectOverCDP(await app.cdpUrl());
      try {
        let page = await livePage(browser);
        await page.getByLabel('Server address').fill(server.origin);
        await page.getByRole('button', { name: 'Continue' }).click();
        page = await livePage(browser, page);
        await page.setViewportSize({ width: 1215, height: 900 });
        await page.getByLabel('Name', { exact: true }).fill(server.member.name);
        await page.getByLabel('Password', { exact: true }).fill(server.member.password);
        await page.getByRole('button', { name: 'Sign in', exact: true }).click();

        // Give the bridge a real click to start the fake session. The
        // disposable server supports chat without advertising LiveKit.
        const heading = page.getByRole('heading', { name: 'general', exact: true });
        await heading.evaluate((el) => {
          const joinVoice = window.conchDesktop?.voiceJoin;
          if (joinVoice === undefined) throw new Error('The packaged voice bridge is unavailable.');
          el.addEventListener('click', () => {
            void joinVoice({ channel: 'general', muted: false, deafened: false });
          }, { once: true });
        });
        await heading.click();
        const toggle = page.getByRole('button', { name: 'Voice options' });
        const leave = page.getByRole('button', { name: 'Leave voice' });
        const mute = page.getByRole('button', { name: 'Mute', exact: true });
        const panel = page.locator('#voice-options');
        await expect(mute).toBeVisible();

        await toggle.click();
        await expect(panel).toBeVisible();
        await expect(panel).toHaveCSS('position', 'fixed');
        await expect(panel.getByText('Listing devices…')).toBeHidden();
        await mute.click({ trial: true });
        await toggle.click({ trial: true });
        await leave.click({ trial: true });
        await page.keyboard.press('Escape');
        await expect(panel).toBeHidden();
        await expect(toggle).toBeFocused();

        await toggle.click();
        await panel.getByRole('button', { name: 'Close' }).click();
        await expect(panel).toBeHidden();
        await expect(toggle).toBeFocused();

        await toggle.click();
        await heading.click();
        await expect(panel).toBeHidden();
        await expect(toggle).toBeFocused();

        await page.setViewportSize({ width: 360, height: 480 });
        await page.getByRole('navigation', { name: 'Main navigation' }).getByRole('button', { name: 'Voice', exact: true }).click();
        await toggle.click();
        await expect(panel).toBeInViewport({ ratio: 1 });
        const box = await panel.boundingBox();
        const row = await page.locator('.voice-controls').boundingBox();
        if (box === null || row === null) throw new Error('Voice options or the dock controls are missing.');
        expect(box.y + box.height <= row.y || box.y >= row.y + row.height).toBe(true);
        await page.keyboard.press('Escape');
        await expect(panel).toBeHidden();
        await expect(toggle).toBeFocused();
      } finally {
        await browser.close();
      }
    } finally {
      await app.stop();
      await server.stop();
    }
  } finally {
    files.cleanup();
  }
});
