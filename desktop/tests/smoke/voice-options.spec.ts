// The packaged renderer with a connected helper transcript. The helper is
// replaced only in a private copy, so this needs neither LiveKit nor audio.
import { chmodSync, constants, cpSync, readdirSync, rmSync, statSync, symlinkSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { chromium, expect, test, type Locator } from '@playwright/test';
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

// The CI job makes dist/linux-unpacked/chrome-sandbox root-owned and setuid,
// which a copy made by this user cannot be. When the build's is so, the copy
// points at it (Chromium checks the file it opens, so a link is the same
// helper), and the copy runs with the Chromium sandbox on exactly as the main
// app does; a build whose helper is not (a developer's) keeps its own copy,
// where the namespace sandbox is used. No spec turns the Chromium sandbox off.
function shareSandboxHelper(appDir: string): void {
  const built = join(dirname(APP), 'chrome-sandbox');
  let st;
  try {
    st = statSync(built);
  } catch {
    return;
  }
  if (st.uid !== 0 || (st.mode & 0o4000) === 0) return;
  const copy = join(appDir, 'chrome-sandbox');
  rmSync(copy, { force: true });
  symlinkSync(built, copy);
}

// reachable: after the page scrolls the control into view (the phone strip is
// taller than 480 px), what is under the middle of it is the control.
async function reachable(control: Locator): Promise<boolean> {
  await control.scrollIntoViewIfNeeded();
  return control.evaluate((el) => {
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) return false;
    const x = r.left + r.width / 2;
    const y = r.top + r.height / 2;
    if (x < 0 || y < 0 || x > window.innerWidth || y > window.innerHeight) return false;
    const at = document.elementFromPoint(x, y);
    return at !== null && (at === el || el.contains(at));
  });
}

async function expectInReach(panel: Locator, controls: readonly Locator[]): Promise<void> {
  await expect(panel).toBeInViewport({ ratio: 1 });
  for (const control of controls) expect(await reachable(control)).toBe(true);
}

test('Voice options keeps the dock in reach in the packaged desktop strip (#2195)', async () => {
  test.setTimeout(240_000);
  const files = scratch('/tmp');
  try {
    const appDir = join(files.root, 'app');
    cpSync(dirname(APP), appDir, { recursive: true, mode: constants.COPYFILE_FICLONE });
    files.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6, devices: fixture('voice-devices/voice-devices-v1.json') });
    cpSync(join(files.resources, 'conch-voice'), join(appDir, 'resources', 'conch-voice'));
    // A build made under a group-writable umask is untrusted as a helper's
    // ancestor; close the copy's directories to group and others.
    const close = (dir: string): void => {
      chmodSync(dir, 0o755);
      for (const e of readdirSync(dir, { withFileTypes: true })) if (e.isDirectory()) close(join(dir, e.name));
    };
    close(appDir);
    shareSandboxHelper(appDir);
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
        // The helper's device list loads, so the panel is at its full height.
        await expect(panel.getByLabel('Microphone')).toBeVisible();
        await expect(panel.getByLabel('Speakers')).toBeVisible();
        await expect(panel.getByText('Listing devices…')).toBeHidden();
        const talkKey = page.getByRole('button', { name: 'Talk key', exact: true });
        await expect(talkKey).toBeVisible();
        const dock = [mute, toggle, leave, talkKey];
        await expectInReach(panel, dock);
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

        // A press outside on something that takes focus (the conversation
        // heading is focusable) closes the panel and leaves focus where the
        // press put it; a press on blank, unfocusable space (the dock's "In
        // voice in #general" line) closes it and returns focus to the toggle
        // (#2195 round 3).
        await toggle.click();
        await heading.click();
        await expect(panel).toBeHidden();
        await expect(heading).toBeFocused();

        await toggle.click();
        await page.getByText('In voice in #general', { exact: true }).click({ timeout: 10_000 });
        await expect(panel).toBeHidden();
        await expect(toggle).toBeFocused();

        await page.setViewportSize({ width: 360, height: 480 });
        await page.getByRole('navigation', { name: 'Main navigation' }).getByRole('button', { name: 'Voice', exact: true }).click();
        await toggle.click();
        await expect(panel.getByLabel('Microphone')).toBeVisible();
        const box = await panel.boundingBox();
        const row = await page.locator('.voice-controls').boundingBox();
        if (box === null || row === null) throw new Error('Voice options or the dock controls are missing.');
        expect(box.y + box.height <= row.y || box.y >= row.y + row.height).toBe(true);
        // The row as it stands is clear of the panel; now scroll to each control.
        await expectInReach(panel, dock);
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
