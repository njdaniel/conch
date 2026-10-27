// The private microphone test (issue #846), in a real Chromium on its fake
// devices (the suite's tone as the microphone). A member starts the test
// before joining: the level meter moves, and the page makes no peer
// connection. Stop releases the microphone (every track ended). Choosing
// another microphone moves the capture and releases the old one; a device
// that goes says so and releases it; joining voice with the test running
// takes the test away, with its capture released. Once on a desktop window,
// once on a phone's.
//
// An init script keeps every capture the page asks the browser for, and hides
// devices from the list on request, as 19-voice-devices.spec.ts does. Against
// a conchd with LiveKit of its own (voice-world.ts), because the strip shows
// only where voice is set up: without Docker it fails when CI is set and is
// skipped otherwise.
import type { Page } from '@playwright/test';
import { channelButton, expect, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type VoiceWorld } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

const devices = () => {
  const w = window as unknown as { __tracks: MediaStreamTrack[]; __hidden: string[]; __peers: number };
  w.__tracks = [];
  w.__hidden = [];
  w.__peers = 0;
  const media = navigator.mediaDevices as MediaDevices | undefined;
  if (media === undefined) return;
  const capture = media.getUserMedia.bind(media);
  const list = media.enumerateDevices.bind(media);
  media.getUserMedia = async (constraints?: MediaStreamConstraints) => {
    const stream = await capture(constraints);
    w.__tracks.push(...stream.getAudioTracks());
    return stream;
  };
  media.enumerateDevices = async () => (await list()).filter((d) => !w.__hidden.includes(d.deviceId));
  // Nothing may reach a room: count the peer connections the page makes.
  const Original = window.RTCPeerConnection;
  const Counted = function (this: unknown, ...args: ConstructorParameters<typeof RTCPeerConnection>) {
    w.__peers++;
    return new Original(...args);
  } as unknown as typeof RTCPeerConnection;
  Counted.prototype = Original.prototype;
  window.RTCPeerConnection = Counted;
};

// The state of every capture the page asked for, in order.
const states = (page: Page): Promise<string[]> =>
  page.evaluate(() => (window as unknown as { __tracks: MediaStreamTrack[] }).__tracks.map((t) => t.readyState));

const peers = (page: Page): Promise<number> => page.evaluate(() => (window as unknown as { __peers: number }).__peers);

const unplug = (page: Page, id: string): Promise<void> =>
  page.evaluate((deviceId) => {
    (window as unknown as { __hidden: string[] }).__hidden.push(deviceId);
    navigator.mediaDevices.dispatchEvent(new Event('devicechange'));
  }, id);

const sizes = [
  { name: 'desktop', width: 1280, height: 800 },
  { name: 'phone', width: 390, height: 844 },
];

test.describe('the private microphone test', () => {
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

  for (const size of sizes) {
    test(`checks the microphone without joining, on a ${size.name}`, async ({ browser }) => {
      test.setTimeout(120_000);
      const context = await browser.newContext({ viewport: { width: size.width, height: size.height } });
      const page = await context.newPage();
      try {
        await watchPage(page, vw);
        await page.addInitScript(devices);
        await signIn(page, vw, vw.alice.token, vw.alice.name);
        await channelButton(page, vw.channel).click();
        const voice = voiceRegion(page);
        const start = voice.getByRole('button', { name: 'Test microphone' });
        await expect(start).toBeVisible();

        // The meter moves on the fake microphone's tone; Stop releases it.
        await start.click();
        const meter = voice.getByLabel('Microphone test level');
        await expect(meter).toBeVisible();
        await expect.poll(async () => Number(await meter.getAttribute('value'))).toBeGreaterThan(0.05);
        expect(await states(page)).toEqual(['live']);
        expect(await peers(page)).toBe(0);
        await voice.getByRole('button', { name: 'Stop test' }).click();
        await expect.poll(() => states(page)).toEqual(['ended']);
        await expect(start).toBeVisible();

        // Another microphone: the capture moves, the old one is released.
        await start.click();
        const list = voice.getByRole('combobox', { name: 'Microphone' });
        await expect(list.locator('option')).toHaveText(['System default', 'Fake Default Audio Input', 'Fake Audio Input 1', 'Fake Audio Input 2']);
        const second = await list.locator('option', { hasText: 'Fake Audio Input 2' }).getAttribute('value');
        if (second === null) throw new Error('no entry for the second microphone');
        await list.selectOption(second);
        await expect.poll(() => states(page)).toEqual(['ended', 'ended', 'live']);

        // The device goes: the test stops, says so, and releases it.
        await unplug(page, second);
        await expect(voice.getByRole('status')).toContainText('Your microphone was disconnected');
        await expect.poll(() => states(page)).toEqual(['ended', 'ended', 'ended']);
        await expect(start).toBeVisible();
        expect(await peers(page)).toBe(0);

        // Joining voice with the test running takes the test away first.
        await start.click();
        await expect.poll(async () => (await states(page)).at(-1)).toBe('live');
        await voice.getByRole('button', { name: 'Join voice' }).click();
        await expect(voice.getByRole('button', { name: /test microphone|stop test/i })).toHaveCount(0);
        await expect(voice).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
        const after = await states(page);
        expect(after[after.length - 2]).toBe('ended');
        await voice.getByRole('button', { name: 'Leave voice' }).click();
      } finally {
        await context.close();
      }
    });
  }
});
