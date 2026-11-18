// Picture-in-picture for someone else's camera (issue #1954; a watched share's
// is 12-screen-share's, #1839), against the conchd and pinned LiveKit of
// voice-world.ts, on Chromium's fake camera (--use-fake-device-for-media-stream,
// playwright.config.ts). The owner turns use_camera on for operators and
// members first, as 44-camera does.
//
// What it shows: bob's tile of alice's camera offers Pop out from the
// keyboard (Tab from the tile's enlarge button, Enter), the same element goes
// into the browser's picture-in-picture window and keeps decoding, and Enter
// on Return puts it back; alice's own camera tile never offers it. While it
// is out, the layer bob receives is the one for the PiP window, not for his
// tile. Headless Chromium opens the window at the picture's own size, so the
// test widens the tile first (the top layer, 640 px wide from the fake
// camera), pops it out, and then shrinks the tile to 120 px: without the PiP
// sizing the SDK would read the tile's size and drop to the lowest layer
// (320 px); with it the layer stays at the window's, and drops only on
// Return, when the tile's size is asked for again. alice
// turning her camera off, and bob leaving voice, each close the window.
//
// What it is not: a real camera, a real PiP window a person drags or resizes,
// another browser engine, a phone, or a dense screen (the scaling for one is
// sdk.ts's, unit-tested in pictureInPicture.test.ts). Without Docker the
// tests fail when CI is set and are skipped otherwise, as in 10-voice.
import type { Browser, Page } from '@playwright/test';
import { closeAll, expect, openChannel, rememberPushToTalk, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type Member, type VoiceWorld, type Watch } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const callView = (page: Page) => page.getByRole('region', { name: 'Video call' });

// Installed before the page's own scripts: keeps every peer connection the
// SDK makes, for getStats, and the size of each PiP window opened.
const instrument = () => {
  const w = window as unknown as { __pcs: RTCPeerConnection[]; __pip: { width: number; height: number } | null };
  w.__pcs = [];
  w.__pip = null;
  const Original = window.RTCPeerConnection;
  const Wrapped = function (this: unknown, ...args: ConstructorParameters<typeof RTCPeerConnection>) {
    const pc = new Original(...args);
    w.__pcs.push(pc);
    return pc;
  } as unknown as typeof RTCPeerConnection;
  Wrapped.prototype = Original.prototype;
  Object.setPrototypeOf(Wrapped, Original);
  window.RTCPeerConnection = Wrapped;
  document.addEventListener('enterpictureinpicture', (e) => {
    const pip = (e as PictureInPictureEvent).pictureInPictureWindow;
    w.__pip = { width: pip.width, height: pip.height };
  }, true);
};

// What this page receives of the video it still decodes: the frames decoded,
// and the widest picture among them (the layer the SFU forwards).
async function videoIn(page: Page): Promise<{ readonly frames: number; readonly width: number }> {
  return page.evaluate(async () => {
    const w = window as unknown as { __pcs: RTCPeerConnection[] };
    let frames = 0;
    let width = 0;
    for (const pc of w.__pcs) {
      if (pc.signalingState === 'closed') continue;
      const stats = await pc.getStats();
      stats.forEach((r: { type: string; kind?: string; framesDecoded?: number; frameWidth?: number }) => {
        if (r.type !== 'inbound-rtp' || r.kind !== 'video') return;
        frames += r.framesDecoded ?? 0;
        width = Math.max(width, r.frameWidth ?? 0);
      });
    }
    return { frames, width };
  });
}

const inPip = (page: Page) => page.evaluate(() => {
  const el = document.pictureInPictureElement;
  return el === null ? null : el.getAttribute('aria-label');
});

test.describe('picture-in-picture for a camera with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the camera PiP suite needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the camera PiP suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
    const read = await vw.api(vw.operator, 'GET', '/v1/permissions');
    expect(read.status).toBe(200);
    const { revision } = JSON.parse(read.text) as { revision: number };
    const put = await vw.api(vw.operator, 'PUT', '/v1/permissions', {
      revision,
      changes: [
        { permission: 'use_camera', role: 'operator', granted: true },
        { permission: 'use_camera', role: 'member', granted: true },
      ],
    });
    expect(put.status, put.text).toBe(200);
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  interface Someone {
    readonly page: Page;
    readonly watch: Watch;
    readonly close: () => Promise<void>;
  }

  async function person(browser: Browser, who: 'alice' | 'bob'): Promise<Someone> {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    await rememberPushToTalk(ctx);
    const page = await ctx.newPage();
    const watch = await watchPage(page, vw);
    await page.addInitScript(instrument);
    const member: Member = vw[who];
    await signIn(page, vw, member.token, member.name);
    await openChannel(page, vw.channel);
    return { page, watch, close: () => closeAll(ctx) };
  }

  async function join(p: Someone): Promise<void> {
    await voiceRegion(p.page).getByRole('button', { name: 'Join voice' }).click();
    await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
  }

  async function camera(p: Someone, on: boolean): Promise<void> {
    const button = voiceRegion(p.page).getByRole('button', { name: 'Camera', exact: true });
    await expect(button).toHaveAttribute('aria-pressed', on ? 'false' : 'true');
    await button.click();
    if (on) await expect(voiceRegion(p.page)).toContainText('Sending video', { timeout: 30_000 });
    else await expect(voiceRegion(p.page)).not.toContainText('Sending video');
  }

  // bob opens the view, if it is not open, and waits for alice's picture.
  async function seeAlice(b: Someone): Promise<void> {
    const show = voiceRegion(b.page).getByRole('button', { name: 'Show video (1)' });
    await expect(show.or(callView(b.page).getByLabel("alice's camera"))).toBeVisible({ timeout: 15_000 });
    if (await show.isVisible()) await show.click();
    const tile = callView(b.page).getByLabel("alice's camera");
    await expect.poll(() => tile.evaluate((v) => (v as HTMLVideoElement).videoWidth), { timeout: 30_000 }).toBeGreaterThan(0);
  }

  async function expectClean(p: Someone): Promise<void> {
    await p.watch.settled();
    const said = p.watch.console.join('\n');
    for (const s of vw.secrets) expect(said, 'a secret in the console').not.toContain(s);
    expect(p.watch.console.filter((l) => /Content Security Policy|Refused to|[Pp]ermissions policy/.test(l))).toEqual([]);
    expect(p.watch.policy, 'Permissions-Policy reports').toEqual([]);
  }

  test("someone else's camera pops out and returns from the keyboard, sized to the window; never the own camera; camera off and leaving voice close it", async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await join(a);
      await join(b);
      await camera(a, true);
      await seeAlice(b);

      // alice's own tile offers no Pop out.
      await expect(callView(a.page).getByLabel('Your camera')).toBeVisible();
      await expect(callView(a.page).getByRole('button', { name: /Pop out|Return/ })).toHaveCount(0);

      // bob's tile, 1200 px wide: the SDK asks for more than the lowest
      // layer. Set through the CSSOM, which the page's style-src 'self'
      // allows.
      const tile = callView(b.page).getByLabel("alice's camera");
      const widen = (width: string) => tile.evaluate((v, w) => {
        const frame = v.closest('figure') as HTMLElement;
        frame.style.width = w;
        frame.style.maxHeight = 'none';
      }, width);
      await widen('1200px');
      await expect.poll(async () => (await videoIn(b.page)).width, { timeout: 30_000 }).toBeGreaterThan(320);

      // Tab from the tile's enlarge button reaches Pop out; Enter opens it.
      await callView(b.page).getByRole('button', { name: "Enlarge alice's video" }).focus();
      await b.page.keyboard.press('Tab');
      await expect(callView(b.page).getByRole('button', { name: "Pop out alice's video", pressed: false })).toBeFocused();
      await b.page.keyboard.press('Enter');
      await expect.poll(() => inPip(b.page)).toBe("alice's camera");
      const back = callView(b.page).getByRole('button', { name: "Return alice's video to the stage", pressed: true });
      await expect(back).toBeFocused();

      // The layer follows the window, not the tile: the tile shrunk to 120 px
      // while it is out, the layer stays. The window must be wider than the
      // lowest layer, or this proves nothing.
      const pip = await b.page.evaluate(() => (window as unknown as { __pip: { width: number; height: number } | null }).__pip);
      expect(pip?.width ?? 0, 'the PiP window is wider than the lowest layer').toBeGreaterThan(320);
      await widen('120px');
      for (let i = 0; i < 10; i++) {
        await b.page.waitForTimeout(500);
        expect((await videoIn(b.page)).width, 'the layer while out, the tile at 120 px').toBeGreaterThan(320);
      }
      const frames = (await videoIn(b.page)).frames;
      await expect.poll(async () => (await videoIn(b.page)).frames, { timeout: 15_000 }).toBeGreaterThan(frames);

      // Enter on Return puts it back, in the same element, sized to the
      // 120 px tile again: the lowest layer, within the 5 s the layer was
      // watched for above, so a tile size read while out would have shown.
      await tile.evaluate((v) => { v.dataset.before = 'pip'; });
      await b.page.keyboard.press('Enter');
      await expect.poll(() => inPip(b.page)).toBeNull();
      await expect(callView(b.page).getByRole('button', { name: "Pop out alice's video", pressed: false })).toBeFocused();
      await expect(tile).toHaveAttribute('data-before', 'pip');
      await expect.poll(async () => (await videoIn(b.page)).width, { timeout: 5_000 }).toBeLessThanOrEqual(320);

      // Out again, alice's camera ending closes it.
      await b.page.keyboard.press('Enter');
      await expect.poll(() => inPip(b.page)).toBe("alice's camera");
      await camera(a, false);
      await expect.poll(() => inPip(b.page), { timeout: 15_000 }).toBeNull();

      // On again and out again, bob leaving voice closes it.
      await camera(a, true);
      await seeAlice(b);
      await callView(b.page).getByRole('button', { name: "Pop out alice's video" }).click();
      await expect.poll(() => inPip(b.page)).toBe("alice's camera");
      await voiceRegion(b.page).getByRole('button', { name: 'Leave voice' }).click();
      await expect(voiceRegion(b.page)).toContainText('Not in voice', { timeout: 15_000 });
      await expect.poll(() => inPip(b.page)).toBeNull();

      await camera(a, false);
      for (const p of [a, b]) await expectClean(p);
    } finally {
      await a.close();
      await b.close();
    }
  });
});
