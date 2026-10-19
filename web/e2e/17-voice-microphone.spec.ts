// A microphone the browser refuses (issue #586, audit F27): Join voice stops
// and says why, the talk controls go, and nothing tries again by itself; Watch
// only still connects, and a refused Use microphone keeps that connection.
//
// The project's --use-fake-ui-for-media-stream flag answers every permission
// prompt and cannot be turned off for one test, so the refusal is made by an
// init script: getUserMedia rejects with the DOMException a browser gives for
// a refused permission. Against a conchd with LiveKit of its own
// (voice-world.ts): without Docker it fails when CI is set and is skipped
// otherwise.
import type { Page } from '@playwright/test';
import { channelButton, expect, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type VoiceWorld } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

const blocked = /Your browser blocked the microphone/;

// refuseMicrophone makes every microphone request fail as a refused
// permission does. Installed before the page's own scripts run.
const refuseMicrophone = () => {
  const devices = navigator.mediaDevices as MediaDevices | undefined;
  if (devices === undefined) return;
  devices.getUserMedia = () => Promise.reject(new DOMException('Permission denied', 'NotAllowedError'));
};

test.describe('with a refused microphone', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('browser voice needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the browser voice suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  test('Join voice stops with the reason and does not retry; Watch only still connects, and Use microphone says why', async ({ browser }) => {
    test.setTimeout(120_000);
    const context = await browser.newContext();
    const page = await context.newPage();
    try {
      const watch = await watchPage(page, vw);
      await page.addInitScript(refuseMicrophone);
      // Every voice session asked for, and every socket opened to LiveKit.
      const sessions: number[] = [];
      const livekit: number[] = [];
      page.on('request', (req) => {
        if (req.method() === 'POST' && new URL(req.url()).pathname.endsWith('/voice/session')) sessions.push(Date.now());
      });
      page.on('websocket', (ws) => {
        if (new URL(ws.url()).host === new URL(vw.livekitOrigin).host) livekit.push(Date.now());
      });
      await signIn(page, vw, vw.alice.token, vw.alice.name);
      await channelButton(page, vw.channel).click();
      const voice = voiceRegion(page);

      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText('Voice stopped', { timeout: 2_000 });
      await expect(voice).toContainText(blocked);
      await expect(voice.getByRole('button', { name: 'Join voice' })).toBeVisible();
      await expect(voice.getByRole('button', { name: 'Watch only' })).toBeVisible();
      for (const name of ['Hold to talk', 'Mute', 'Deafen']) await expect(voice.getByRole('button', { name, exact: true })).toHaveCount(0);

      // Nothing asks again by itself: no session request and no LiveKit
      // connection in the next 30 s.
      const askedBefore = sessions.length;
      const socketsBefore = livekit.length;
      expect(askedBefore).toBe(1);
      await new Promise((r) => setTimeout(r, 30_000));
      expect(sessions.length, 'voice session requests after the stop').toBe(askedBefore);
      expect(livekit.length, 'LiveKit connections after the stop').toBe(socketsBefore);
      await expect(voice).toContainText('Voice stopped');

      // Watch only needs no microphone.
      await voice.getByRole('button', { name: 'Watch only' }).click();
      await expect(voice).toContainText(/Connected in #/, { timeout: 30_000 });
      await expect(voice).toContainText('Watch only: your microphone is off.');

      // Use microphone is refused too: the connection stays, and says why.
      await voice.getByRole('button', { name: 'Use microphone' }).click();
      await expect(voice).toContainText(blocked);
      await expect(voice).toContainText(/Connected in #/);
      await expect(voice).toContainText('Watch only: your microphone is off.');

      // The reason names no token and no room.
      await watch.settled();
      const text = (await voice.textContent()) ?? '';
      for (const s of vw.secrets) expect(text).not.toContain(s);

      await voice.getByRole('button', { name: 'Leave voice' }).click();
      await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
    } finally {
      await context.close();
    }
  });
});
