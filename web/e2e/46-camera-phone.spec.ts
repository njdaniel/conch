// The camera on a phone (ADR-015, issue #1268; design §11.6, the phone row),
// against the conchd and pinned LiveKit of voice-world.ts, on Chromium's fake
// camera in phone-sized windows (320×640 and 390×844, touch, mobile). The
// owner turns use_camera on for operators and members first, as 44-camera
// does.
//
// What it shows: under Chat a phone draws no call view and no camera tile,
// and the camera streams it received stop decoding, then come back with the
// Voice view and no other tap; the strip with the camera on is still one row
// of four at both widths, its state button saying "Camera on" with the
// camera glyph, and still opening Voice; a phone-sized page with no camera
// chosen asks for 640×360 at 24 fps and the front camera; a page hidden on a
// phone ends the camera as its button does (the track ends, the claim is
// released by name, `camera` leaves presence, and the audit trail says
// reason=self), and showing it again starts nothing; the same hidden page at
// 1280 px leaves the camera sending.
//
// What it is not: a real phone. alice and bob are separate principals on one
// machine; the hidden page is a simulated visibilitychange (document
// .visibilityState overridden, the event dispatched), not a phone locking or
// switching apps. It shows nothing about Safari on iPhone muting a
// backgrounded track, Chrome on Android, real front and back cameras, a
// phone's data use, or a LAN or mobile network: those are C8's by-hand rows.
// Without Docker the tests fail when CI is set and are skipped otherwise, as
// in 10-voice.
import type { Browser, BrowserContextOptions, Locator, Page } from '@playwright/test';
import type { VoicePresenceV1 } from '../src/schema/voice';
import { closeAll, expect, expectFits, openChannel, phoneSizes, rememberPushToTalk, showView, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type Member, type VoiceWorld, type Watch } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const callView = (page: Page) => page.getByRole('region', { name: 'Video call' });

const sizes = phoneSizes.filter(([size]) => size === '320×640' || size === '390×844');

// Installed before the page's own scripts: keeps every peer connection the
// SDK makes (for getStats), every camera track the browser gives, and what
// each request for a camera asked.
const instrument = () => {
  const w = window as unknown as { __pcs: RTCPeerConnection[]; __cams: MediaStreamTrack[]; __asks: string[] };
  w.__pcs = [];
  w.__cams = [];
  w.__asks = [];
  const Original = window.RTCPeerConnection;
  const Wrapped = function (this: unknown, ...args: ConstructorParameters<typeof RTCPeerConnection>) {
    const pc = new Original(...args);
    w.__pcs.push(pc);
    return pc;
  } as unknown as typeof RTCPeerConnection;
  Wrapped.prototype = Original.prototype;
  Object.setPrototypeOf(Wrapped, Original);
  window.RTCPeerConnection = Wrapped;
  const devices = navigator.mediaDevices;
  const user = devices.getUserMedia.bind(devices);
  devices.getUserMedia = async (constraints?: MediaStreamConstraints) => {
    if (constraints?.video !== undefined && constraints.video !== false) w.__asks.push(JSON.stringify(constraints.video));
    const stream = await user(constraints);
    w.__cams.push(...stream.getVideoTracks());
    return stream;
  };
};

// What each request for a camera asked, as the video constraints.
const asks = (page: Page) => page.evaluate(() => (window as unknown as { __asks: string[] }).__asks.map((a) => JSON.parse(a) as unknown));
// The readyState of every camera track this page captured.
const camStates = (page: Page) => page.evaluate(() => (window as unknown as { __cams: MediaStreamTrack[] }).__cams.map((t) => t.readyState));

// framesIn sums the video frames this page has decoded on the streams it
// still receives, and counts them.
async function videoIn(page: Page): Promise<{ readonly streams: number; readonly frames: number }> {
  return page.evaluate(async () => {
    const w = window as unknown as { __pcs: RTCPeerConnection[] };
    let frames = 0;
    let streams = 0;
    for (const pc of w.__pcs) {
      if (pc.signalingState === 'closed') continue;
      const stats = await pc.getStats();
      stats.forEach((r: { type: string; kind?: string; framesDecoded?: number }) => {
        if (r.type !== 'inbound-rtp' || r.kind !== 'video') return;
        streams++;
        frames += r.framesDecoded ?? 0;
      });
    }
    return { streams, frames };
  });
}

// setHidden simulates the page going out of sight, or coming back: the way
// readstate.test.tsx does, document.visibilityState is overridden and the
// event dispatched.
function setHidden(page: Page, hidden: boolean): Promise<void> {
  return page.evaluate((h) => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => (h ? 'hidden' : 'visible') });
    document.dispatchEvent(new Event('visibilitychange'));
  }, hidden);
}

async function boxOf(el: Locator, what: string): Promise<{ x: number; y: number; width: number; height: number }> {
  const found = await el.boundingBox();
  if (found === null) throw new Error(`${what} has no box`);
  return found;
}

test.describe('the camera on a phone with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the phone camera suite needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the phone camera suite needs it (it fails, rather than skips, when CI is set)');
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
    readonly member: Member;
    readonly size: string;
    // Every camera request this page made, as its method and query.
    readonly cameraCalls: string[];
    readonly close: () => Promise<void>;
  }

  async function person(browser: Browser, member: Member, size: string, options: BrowserContextOptions, font?: string): Promise<Someone> {
    const ctx = await browser.newContext(options);
    await rememberPushToTalk(ctx);
    const page = await ctx.newPage();
    const watch = await watchPage(page, vw);
    await page.addInitScript(instrument);
    // A wider font than the developer's, through a constructed stylesheet (style-src
    // 'self' allows it), so that a font difference cannot hide a clipped strip: CI
    // has DejaVu Sans where a developer has Noto Sans. Without the font on
    // this machine the browser falls back, and the run is the default's.
    if (font !== undefined) {
      await page.addInitScript((f) => {
        const sheet = new CSSStyleSheet();
        sheet.replaceSync(`html:root { --font: '${f}', sans-serif; }`);
        document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
      }, font);
    }
    const cameraCalls: string[] = [];
    page.on('request', (req) => {
      const url = new URL(req.url());
      if (url.pathname.endsWith('/voice/camera')) cameraCalls.push(`${req.method()} ${url.search}`);
    });
    await signIn(page, vw, member.token, member.name);
    await openChannel(page, vw.channel);
    const close = () => closeAll(ctx);
    return { page, watch, member, size, cameraCalls, close };
  }

  async function join(p: Someone): Promise<void> {
    await showView(p.page, 'Voice');
    await voiceRegion(p.page).getByRole('button', { name: 'Join voice' }).click();
    await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
  }

  // The Camera button is in the Voice view.
  async function cameraOn(p: Someone): Promise<void> {
    await showView(p.page, 'Voice');
    const button = voiceRegion(p.page).getByRole('button', { name: 'Camera', exact: true });
    await expect(button).toHaveAttribute('aria-pressed', 'false');
    await button.click();
    await expect(voiceRegion(p.page)).toContainText('Sending video', { timeout: 30_000 });
    await expect(button).toHaveAttribute('aria-pressed', 'true');
  }

  async function cameraOf(member: Member): Promise<boolean> {
    const r = await vw.api(member.token, 'GET', `/v1/channels/${vw.channel}/voice`);
    expect(r.status).toBe(200);
    const doc = JSON.parse(r.text) as VoicePresenceV1;
    return doc.rooms.some((room) => room.audience === undefined && room.participants.some((x) => x.principal_id === member.id && x.camera === true));
  }

  async function stoppedBy(member: Member): Promise<(string | undefined)[]> {
    const rows = (await vw.audit()).filter((e) => e.action === 'voice_camera_stopped' && e.detail.includes(`principal=${String(member.id)}`));
    return rows.map((e) => /reason=(\w+)/.exec(e.detail)?.[1]);
  }

  // The phone's strip with the camera on: one row of the controls, each 44 px
  // each way, the state button saying "Camera on" with the camera glyph.
  async function expectCameraStrip(p: Someone, where: string): Promise<void> {
    const voice = voiceRegion(p.page);
    const names = [/^Camera on in #voice-room/, /^Hold to talk$/, /^Open mic$/, /^Mute$/, /^Leave voice$/];
    const dock = await boxOf(p.page.locator('.dock'), `${where}: the strip`);
    expect.soft(dock.height, `${where}: the strip is ${String(dock.height)} px high`).toBeLessThanOrEqual(64);
    expect.soft(await voice.getByRole('button').filter({ visible: true }).count(), `${where}: the buttons the strip shows`).toBe(names.length);
    const screen = p.page.viewportSize()?.width ?? 0;
    const middles: number[] = [];
    for (const name of names) {
      const control = voice.getByRole('button', { name });
      const box = await boxOf(control, `${where}: ${String(name)}`);
      expect.soft(box.x, `${where}: ${String(name)} starts at ${String(box.x)}`).toBeGreaterThanOrEqual(0);
      expect.soft(box.x + box.width, `${where}: ${String(name)} ends at ${String(box.x + box.width)} of ${String(screen)}`).toBeLessThanOrEqual(screen - 8);
      expect.soft(box.width, `${where}: ${String(name)} is ${String(box.width)} px wide`).toBeGreaterThanOrEqual(44);
      expect.soft(box.height, `${where}: ${String(name)} is ${String(box.height)} px high`).toBeGreaterThanOrEqual(44);
      middles.push(box.y + box.height / 2);
    }
    expect.soft(Math.max(...middles) - Math.min(...middles), `${where}: the controls are one row`).toBeLessThanOrEqual(4);
    const sideways = await p.page.locator('.dock').evaluate((el) => el.scrollWidth - el.clientWidth);
    expect.soft(sideways, `${where}: the strip is ${String(sideways)} px wider than itself`).toBeLessThanOrEqual(0);
    // The state button: the words and the glyph fit inside it, and are not clipped.
    const state = voice.getByRole('button', { name: names[0] ?? /./ });
    await expect(state.locator('.voice-open-phase.camera-on svg'), `${where}: the camera glyph`).toBeVisible();
    await expect(state).toContainText('Camera on');
    const clipped = await state.evaluate((el) => {
      const words = el.querySelector('.voice-open-phase');
      return words === null ? true : words.scrollWidth > words.clientWidth + 1 || words.getBoundingClientRect().right > el.getBoundingClientRect().right + 0.5;
    });
    expect.soft(clipped, `${where}: "Camera on" is cut off`).toBe(false);
    await expectFits(p.page, where);
  }

  // Each size in the page's own font and in a wider one.
  const fonts: readonly (string | undefined)[] = [undefined, 'DejaVu Sans'];
  for (const [size, options, font] of sizes.flatMap((s) => fonts.map((f) => [s[0], s[1], f] as const))) {
    test(`at ${size}${font === undefined ? '' : ` in ${font}`}: under Chat no camera is drawn or decoded, Voice brings it back; the strip says Camera on in one row; the capture is a phone's`, async ({ browser }) => {
      test.setTimeout(240_000);
      const a = await person(browser, vw.alice, size, options, font);
      const b = await person(browser, vw.bob, size, options, font);
      try {
        await join(a);
        await join(b);
        await cameraOn(a);
        // alice captured as a phone does, with no camera chosen: 640×360 at
        // 24 fps and the front camera. A bare value in a constraint is an
        // ideal, as the browser reads it; none is exact, min or max. (The
        // SDK adds deviceId "default", also bare, when none is chosen, as on
        // a desktop; no video device has that id, so it prefers none.)
        const asked = await asks(a.page);
        expect(asked).toEqual([{ width: 640, height: 360, frameRate: 24, facingMode: 'user', deviceId: 'default' }]);

        // bob opens the call view in Voice, and receives alice.
        await showView(b.page, 'Voice');
        const show = voiceRegion(b.page).getByRole('button', { name: 'Show video (1)' });
        await expect(show).toBeVisible({ timeout: 15_000 });
        await show.click();
        const tile = callView(b.page).getByLabel("alice's camera");
        await expect.poll(() => tile.evaluate((v) => (v as HTMLVideoElement).videoWidth), { timeout: 30_000 }).toBeGreaterThan(0);
        await expect.poll(async () => (await videoIn(b.page)).frames, { timeout: 30_000 }).toBeGreaterThan(10);
        const receiving = await videoIn(b.page);
        await expect.poll(async () => (await videoIn(b.page)).frames, { timeout: 15_000 }).toBeGreaterThan(receiving.frames);

        // Under Chat: no call view, no tile, no camera video element, and the
        // streams stop decoding (or are gone).
        await showView(b.page, 'Chat');
        await expect(callView(b.page)).toHaveCount(0);
        await expect(b.page.getByLabel("alice's camera")).toHaveCount(0);
        await expect(b.page.locator('video')).toHaveCount(0);
        await expect
          .poll(
            async () => {
              const before = (await videoIn(b.page)).frames;
              await b.page.waitForTimeout(1500);
              return (await videoIn(b.page)).frames - before;
            },
            { timeout: 30_000, intervals: [250] },
          )
          .toBeLessThanOrEqual(0);
        // Not merely paused: the camera is unsubscribed, so the page holds no
        // video receiver at all (an element-less track alone would keep one).
        await expect.poll(async () => (await videoIn(b.page)).streams, { timeout: 15_000 }).toBe(0);
        // Under Channels as well.
        await showView(b.page, 'Channels');
        await expect(callView(b.page)).toHaveCount(0);
        await expect(b.page.locator('video')).toHaveCount(0);
        const stalled = (await videoIn(b.page)).frames;
        await b.page.waitForTimeout(1500);
        expect((await videoIn(b.page)).frames, 'no frames decoded under Channels').toBeLessThanOrEqual(stalled);

        // Back in Voice: the view shows again with no other tap, and the
        // camera is received again.
        await showView(b.page, 'Voice');
        await expect(callView(b.page)).toBeVisible();
        const back = callView(b.page).getByLabel("alice's camera");
        await expect.poll(() => back.evaluate((v) => (v as HTMLVideoElement).videoWidth), { timeout: 30_000 }).toBeGreaterThan(0);
        const resumed = await videoIn(b.page);
        await expect.poll(async () => (await videoIn(b.page)).frames, { timeout: 30_000 }).toBeGreaterThan(resumed.frames);

        // alice's own camera kept sending while she read Chat; her strip says so.
        await showView(a.page, 'Chat');
        await expectCameraStrip(a, `${size}, Chat`);
        await showView(a.page, 'Channels');
        await expectCameraStrip(a, `${size}, Channels`);
        expect(await camStates(a.page)).toEqual(['live']);
        expect(await cameraOf(vw.alice)).toBe(true);
        // The state button opens Voice, where the Camera button is.
        await voiceRegion(a.page).getByRole('button', { name: /^Camera on in #voice-room/ }).tap();
        await expect(voiceRegion(a.page).getByRole('button', { name: 'Camera', exact: true })).toBeVisible();
        await expect(a.page.getByRole('button', { name: 'Voice', exact: true })).toHaveAttribute('aria-current', 'page');
        // And once it is off, the strip says In voice again.
        await voiceRegion(a.page).getByRole('button', { name: 'Camera', exact: true }).click();
        await expect(voiceRegion(a.page)).not.toContainText('Sending video');
        await showView(a.page, 'Chat');
        await expect(voiceRegion(a.page).getByRole('button', { name: /^In voice in #voice-room/ })).toBeVisible();
        await expect(voiceRegion(a.page)).not.toContainText('Camera on');
        for (const p of [a, b]) await p.watch.settled();
      } finally {
        await a.close();
        await b.close();
      }
    });
  }

  test('a page hidden on a phone ends the camera as the button does; showing it again starts nothing', async ({ browser }) => {
    test.setTimeout(240_000);
    const member = await vw.addMember('camera-phone-hidden');
    const [size, options] = sizes[0] ?? ['320×640', { viewport: { width: 320, height: 640 } }];
    const a = await person(browser, member, size, options);
    const b = await person(browser, vw.bob, size, options);
    try {
      await join(a);
      await join(b);
      await cameraOn(a);
      await expect.poll(() => cameraOf(member), { timeout: 10_000 }).toBe(true);
      await expect(voiceRegion(b.page)).toContainText('camera', { timeout: 15_000 });
      expect(a.cameraCalls).toEqual(['POST ']);
      expect(await camStates(a.page)).toEqual(['live']);

      await setHidden(a.page, true);
      // The track ended, the claim released by name, and camera is gone from
      // presence: the button's stop, not a pause.
      await expect.poll(() => camStates(a.page)).toEqual(['ended']);
      await expect.poll(() => a.cameraCalls, { timeout: 10_000 }).toHaveLength(2);
      expect(a.cameraCalls[1]).toMatch(/^DELETE \?started_at=/);
      await expect.poll(() => cameraOf(member), { timeout: 10_000 }).toBe(false);
      await expect.poll(() => stoppedBy(member), { timeout: 10_000 }).toEqual(['self']);

      // Showing the page again restarts nothing: only the button does.
      await setHidden(a.page, false);
      await a.page.waitForTimeout(3000);
      expect(await asks(a.page)).toHaveLength(1);
      expect(a.cameraCalls).toHaveLength(2);
      expect(await camStates(a.page)).toEqual(['ended']);
      expect(await cameraOf(member)).toBe(false);
      await expect(voiceRegion(a.page).getByRole('button', { name: 'Camera', exact: true })).toHaveAttribute('aria-pressed', 'false');
      await expect(voiceRegion(a.page)).not.toContainText('Sending video');
      // The microphone and the call carry on.
      await expect(voiceRegion(a.page)).toContainText(/Connected in #voice-room/);
      // A tap turns it on again.
      await voiceRegion(a.page).getByRole('button', { name: 'Camera', exact: true }).click();
      await expect(voiceRegion(a.page)).toContainText('Sending video', { timeout: 30_000 });
      expect(await asks(a.page)).toHaveLength(2);
      for (const p of [a, b]) await p.watch.settled();
    } finally {
      await a.close();
      await b.close();
    }
  });

  test('the same hidden page at 1280 px leaves the camera sending, and the capture is the desktop\'s', async ({ browser }) => {
    test.setTimeout(240_000);
    const member = await vw.addMember('camera-desktop-hidden');
    const a = await person(browser, member, '1280×900', { viewport: { width: 1280, height: 900 } });
    try {
      await voiceRegion(a.page).getByRole('button', { name: 'Join voice' }).click();
      await expect(voiceRegion(a.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
      await voiceRegion(a.page).getByRole('button', { name: 'Camera', exact: true }).click();
      await expect(voiceRegion(a.page)).toContainText('Sending video', { timeout: 30_000 });
      // 1280×720 at 30 fps, as ideals, and no facingMode.
      expect(await asks(a.page)).toEqual([{ width: 1280, height: 720, frameRate: 30, deviceId: 'default' }]);
      await setHidden(a.page, true);
      await a.page.waitForTimeout(3000);
      expect(await camStates(a.page)).toEqual(['live']);
      expect(await cameraOf(member)).toBe(true);
      expect(a.cameraCalls).toEqual(['POST ']);
      await expect(voiceRegion(a.page)).toContainText('Sending video');
      await setHidden(a.page, false);
      expect(await camStates(a.page)).toEqual(['live']);
      expect(await stoppedBy(member)).toEqual([]);
      await a.watch.settled();
    } finally {
      await a.close();
    }
  });
});
