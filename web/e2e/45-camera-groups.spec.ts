// The call view in a small group (ADR-015, issue #1267; design §11.6, the
// C6b part), against the conchd and pinned LiveKit of voice-world.ts, on
// Chromium's fake camera and screen (playwright.config.ts). The owner turns
// use_camera on for operators and members first, through PUT
// /v1/permissions, as 44-camera does.
//
// What it shows: a camera and a share together, in carol's page: the share a
// tile with Watch, the cameras with Show video, and once the share is focused
// it is the main picture with both cameras in a strip that receives frames,
// on the right at 1280 px and below at 1000 px; enlarging a camera swapping
// it with the share, and Stop watching leaving the stream's card
// (voice-multi-share.md §7.1); the keyboard alone
// reaching a tile, enlarging it, Full screen, and Escape back with focus
// returned; no animation for someone who asked for less motion, and every
// control at least 44 px each way; and the owner turning the member cell of
// use_camera off, with bob's tile in carol's page going to "Camera off" and
// bob's own page ending its local track and saying why.
//
// alice, bob and carol are separate principals, on one machine. What it is
// not: a real camera, another browser engine, a phone, a screen reader, an
// automated accessibility scan, or a LAN or remote deployment: the role and
// name queries, focus checks and keyboard run here stand in for the scan,
// and C8 does the rest by hand. Without Docker the tests fail when CI is set
// and are skipped otherwise, as in 10-voice.
import { type Browser, type BrowserContextOptions, type Locator, type Page } from '@playwright/test';
import type { VoicePresenceV1 } from '../src/schema/voice';
import { expect, openChannel, rememberPushToTalk, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type Member, type VoiceWorld } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const callView = (page: Page) => page.getByRole('region', { name: 'Video call' });
const shareView = (page: Page) => page.getByRole('region', { name: 'Shared screen' });

// Installed before the page's own scripts: keeps every peer connection the
// SDK makes (for getStats), and every camera track the browser gives.
const instrument = () => {
  const w = window as unknown as { __pcs: RTCPeerConnection[]; __cams: MediaStreamTrack[] };
  w.__pcs = [];
  w.__cams = [];
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
    const stream = await user(constraints);
    w.__cams.push(...stream.getVideoTracks());
    return stream;
  };
};

// The readyState of every camera track this page captured.
const camStates = (page: Page) => page.evaluate(() => (window as unknown as { __cams: MediaStreamTrack[] }).__cams.map((t) => t.readyState));

// videoStreams is how many video streams this page receives that have
// decoded frames, and the frames decoded across them all.
async function videoStreams(page: Page): Promise<{ readonly decoding: number; readonly frames: number }> {
  return page.evaluate(async () => {
    const w = window as unknown as { __pcs: RTCPeerConnection[] };
    let decoding = 0;
    let frames = 0;
    for (const pc of w.__pcs) {
      if (pc.signalingState === 'closed') continue;
      const stats = await pc.getStats();
      stats.forEach((r: { type: string; kind?: string; framesDecoded?: number }) => {
        if (r.type !== 'inbound-rtp' || r.kind !== 'video') return;
        const n = r.framesDecoded ?? 0;
        if (n > 0) decoding++;
        frames += n;
      });
    }
    return { decoding, frames };
  });
}

// A tile's picture: its size, the frames it has shown, and whether it is
// drawn at all (never display: none while it is received, §7.3).
const picture = (video: Locator) =>
  video.evaluate((el) => {
    const v = el as HTMLVideoElement;
    const box = v.getBoundingClientRect();
    return { width: v.videoWidth, frames: v.getVideoPlaybackQuality().totalVideoFrames, display: getComputedStyle(v).display, drawn: box.width > 0 && box.height > 0 };
  });

async function expectReceiving(video: Locator): Promise<void> {
  await expect.poll(async () => (await picture(video)).width, { timeout: 30_000 }).toBeGreaterThan(0);
  const before = await picture(video);
  expect(before.display).not.toBe('none');
  expect(before.drawn).toBe(true);
  await expect.poll(async () => (await picture(video)).frames, { timeout: 15_000 }).toBeGreaterThan(before.frames);
}

async function boxOf(l: Locator): Promise<{ x: number; y: number; width: number; height: number }> {
  const box = await l.boundingBox();
  if (box === null) throw new Error('not drawn');
  return box;
}

// Every control in the call view, buttons, sliders and other inputs alike,
// is at least 44 px each way (§7.7).
async function expectTargets(page: Page, inputs = 0): Promise<void> {
  const controls = callView(page).locator('button, input, select, textarea');
  const sizes = await controls.evaluateAll((els) =>
    els.map((el) => ({ name: `${el.tagName} ${el.getAttribute('aria-label') ?? el.closest('label')?.textContent.trim() ?? el.textContent.trim()}`, ...el.getBoundingClientRect().toJSON() as { width: number; height: number } })),
  );
  expect(sizes.length).toBeGreaterThan(0);
  expect(sizes.filter((s) => s.name.startsWith('INPUT')).length, 'inputs measured').toBeGreaterThanOrEqual(inputs);
  for (const s of sizes) {
    expect(s.width, `${s.name} wide`).toBeGreaterThanOrEqual(44);
    expect(s.height, `${s.name} high`).toBeGreaterThanOrEqual(44);
  }
}

test.describe('the call view in a small group, with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  // The member cell of use_camera, as the owner sets it; the operator cell
  // stays on.
  async function memberCamera(on: boolean): Promise<void> {
    const read = await vw.api(vw.operator, 'GET', '/v1/permissions');
    expect(read.status).toBe(200);
    const { revision } = JSON.parse(read.text) as { revision: number };
    const changes = on
      ? [
          { permission: 'use_camera', role: 'operator', granted: true },
          { permission: 'use_camera', role: 'member', granted: true },
        ]
      : [{ permission: 'use_camera', role: 'member', granted: false }];
    const put = await vw.api(vw.operator, 'PUT', '/v1/permissions', { revision, changes });
    expect(put.status, put.text).toBe(200);
  }

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the camera suite needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the camera suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
    await memberCamera(true);
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  interface Someone {
    readonly page: Page;
    readonly close: () => Promise<void>;
  }

  async function person(browser: Browser, who: 'alice' | 'bob' | 'carol', options?: BrowserContextOptions): Promise<Someone> {
    const ctx = await browser.newContext(options);
    await rememberPushToTalk(ctx);
    const page = await ctx.newPage();
    await watchPage(page, vw);
    await page.addInitScript(instrument);
    const member: Member = vw[who];
    await signIn(page, vw, member.token, member.name);
    await openChannel(page, vw.channel);
    return { page, close: () => ctx.close() };
  }

  async function join(p: Someone): Promise<void> {
    await voiceRegion(p.page).getByRole('button', { name: 'Join voice' }).click();
    await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
  }

  // Waits until the room is these people alone, as conchd's presence says:
  // someone an earlier test closed may be listed for a few seconds more.
  async function roomIs(who: readonly Member[]): Promise<void> {
    const want = who.map((m) => m.id).sort((x, y) => x - y);
    await expect
      .poll(
        async () => {
          const r = await vw.api(vw.carol.token, 'GET', `/v1/channels/${vw.channel}/voice`);
          const doc = JSON.parse(r.text) as VoicePresenceV1;
          return doc.rooms
            .filter((room) => room.audience === undefined)
            .flatMap((room) => room.participants.map((x) => x.principal_id))
            .sort((x, y) => x - y);
        },
        { timeout: 30_000 },
      )
      .toEqual(want);
  }

  async function cameraOn(p: Someone): Promise<void> {
    const button = voiceRegion(p.page).getByRole('button', { name: 'Camera', exact: true });
    await button.click();
    await expect(voiceRegion(p.page)).toContainText('Sending video', { timeout: 30_000 });
    await expect(button).toHaveAttribute('aria-pressed', 'true');
  }

  // Where the strip is: all of it on the right of the main picture, all of
  // it below, or neither (mixed, or mid-reflow). Polled, never thrown.
  async function stripPlace(main: Locator, strip: readonly Locator[]): Promise<'right' | 'below' | 'mixed'> {
    const m = await boxOf(main);
    let right = 0;
    let below = 0;
    for (const s of strip) {
      const b = await boxOf(s);
      if (b.x >= m.x + m.width - 1) right++;
      if (b.y >= m.y + m.height - 1) below++;
    }
    return right === strip.length ? 'right' : below === strip.length ? 'below' : 'mixed';
  }

  // A camera and a share in one call (voice-multi-share.md §7.1, §8; webcam
  // §7.4 "Streams share the grid"). Where the single-share design of #444
  // said otherwise, each changed assertion says why, by section.
  test('a camera and a share together: the stream a tile with Watch, the cameras with Show video; Focus puts the share on the stage and the cameras in a strip that receives; enlarging swaps; Stop watching leaves the camera where it is', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    const c = await person(browser, 'carol', { viewport: { width: 1280, height: 900 } });
    try {
      for (const p of [a, b, c]) await join(p);
      await voiceRegion(a.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voiceRegion(a.page)).toContainText('You are sharing your screen', { timeout: 30_000 });
      await cameraOn(a);
      await cameraOn(b);

      // alice's stream is a tile on carol's stage with Watch, and nothing is
      // received until carol asks: no stream before Watch, no camera before
      // Show video (§7.1 "Nothing is received until Watch"; O8). Was: Watch
      // in the voice strip, and no stage at all before it.
      const stream = c.page.locator('.call-share').filter({ hasText: 'alice · LIVE' });
      await expect(stream.getByRole('button', { name: 'Watch' })).toBeVisible({ timeout: 30_000 });
      await expect(c.page.getByLabel("alice's camera")).toHaveCount(0);
      expect((await videoStreams(c.page)).decoding, 'nothing received before Watch').toBe(0);
      await stream.getByRole('button', { name: 'Watch' }).click();
      await expect(shareView(c.page)).toContainText('alice · LIVE', { timeout: 30_000 });
      // Watching opens no video (O8): the cameras are not drawn yet.
      await expect(c.page.getByLabel("alice's camera")).toHaveCount(0);
      await expect(c.page.getByLabel("bob's camera")).toHaveCount(0);
      await voiceRegion(c.page).getByRole('button', { name: 'Show video (2)' }).click({ timeout: 15_000 });
      const call = callView(c.page);
      const share = call.getByRole('region', { name: 'Shared screen' });
      await expect(share).toBeVisible();
      const aliceCam = call.getByLabel("alice's camera");
      const bobCam = call.getByLabel("bob's camera");
      // Both cameras decode frames, and so does the share.
      await expectReceiving(aliceCam);
      await expectReceiving(bobCam);
      await expectReceiving(share.getByLabel("alice's screen", { exact: true }));
      await expect.poll(async () => (await videoStreams(c.page)).decoding, { timeout: 30_000 }).toBeGreaterThanOrEqual(3);
      const before = (await videoStreams(c.page)).frames;
      await expect.poll(async () => (await videoStreams(c.page)).frames, { timeout: 15_000 }).toBeGreaterThan(before);

      // The share is in the grid until carol focuses it; then it is the main
      // picture and the cameras the strip (§7.1 "Clicking a watched tile
      // focuses it"). Was: the main picture as soon as it was watched.
      await expect(call.locator('ul')).toHaveClass(/call-grid cols-2 rows-2/);
      await share.getByRole('button', { name: "Focus alice's screen" }).click();
      await expect(call).toContainText("Watching alice's screen");
      // At 1280 px the strip is on the right of the share.
      await expect.poll(() => stripPlace(share, [aliceCam, bobCam])).toBe('right');
      // The share keeps its own controls.
      for (const name of ['Mute share', 'Full screen', 'Stop watching']) await expect(share.getByRole('button', { name })).toBeVisible();
      await expect(share.getByLabel('Share volume')).toBeVisible();
      await expectTargets(c.page, 1);

      // Enlarging bob swaps him with the share: the share goes in the strip,
      // still receiving, with its controls.
      await call.getByRole('button', { name: "Enlarge bob's video" }).click();
      await expect(c.page.locator(':focus')).toHaveAttribute('aria-label', "bob's video is enlarged");
      await expect(call).toContainText("bob's video is enlarged");
      await expect.poll(() => stripPlace(bobCam, [share, aliceCam])).toBe('right');
      expect((await boxOf(bobCam)).width).toBeGreaterThan((await boxOf(share)).width);
      // The head stays one row (the title gives way to the controls), so the
      // strip is not pushed down and out of view.
      expect((await boxOf(c.page.locator('.call-head'))).height).toBeLessThan(60);
      await expectReceiving(share.getByLabel("alice's screen", { exact: true }));
      await expectReceiving(aliceCam);
      for (const name of ['Mute share', 'Full screen', 'Stop watching']) await expect(share.getByRole('button', { name })).toBeAttached();
      // The share's volume slider in the strip is still 44 px each way.
      await expectTargets(c.page, 1);
      // Back to grid, and Focus to put the share back on the stage (§7.1
      // "Escape or Back to grid returns"; webcam §7.4 Enlarging). Was: "Back
      // to the share", when the share was the view's resting picture.
      await call.getByRole('button', { name: 'Back to grid' }).click();
      await expect(c.page.locator(':focus')).toHaveAttribute('aria-label', "Enlarge bob's video");
      await expect(call.locator('ul')).toHaveClass(/call-grid cols-2 rows-2/);
      await share.getByRole('button', { name: "Focus alice's screen" }).click();
      await expect.poll(() => stripPlace(share, [aliceCam, bobCam])).toBe('right');

      // Under 1100 px the strip is below the share.
      await c.page.setViewportSize({ width: 1000, height: 900 });
      await expect.poll(() => stripPlace(share, [aliceCam, bobCam])).toBe('below');
      await call.getByRole('button', { name: "Enlarge alice's video" }).click();
      await expect.poll(() => stripPlace(aliceCam, [share, bobCam])).toBe('below');
      // A short window puts the strip below the fold of the view, which
      // scrolls; adaptive stream pauses a tile out of view, so it is brought
      // into view first.
      await bobCam.scrollIntoViewIfNeeded();
      await expectReceiving(bobCam);

      // Stop watching, with alice enlarged and the share in the strip, takes
      // the share's player away and leaves its tile with Watch, focus on it;
      // the enlarged camera stays where it is (§7.1 "Stop watching on a tile
      // unsubscribes it and returns the card"). Was: the share leaving the
      // view and the grid coming back, focus on alice's tile.
      await share.getByRole('button', { name: 'Stop watching' }).click();
      await expect(shareView(c.page)).toHaveCount(0);
      await expect(call).toBeVisible();
      const card = call.locator('.call-share').filter({ hasText: 'alice · LIVE' });
      await expect(card.getByRole('button', { name: 'Watch' })).toBeFocused();
      await expect(call).toContainText("alice's video is enlarged");
      // Back to grid then leaves the cameras and the card a grid, focus on
      // alice's tile, as the old Stop watching did.
      await call.getByRole('button', { name: 'Back to grid' }).click();
      await expect(call.locator('ul')).toHaveClass(/call-grid cols-2 rows-2/);
      await expect(call).not.toContainText('enlarged');
      await expect(c.page.locator(':focus')).toHaveAttribute('aria-label', "Enlarge alice's video");
      await expectReceiving(aliceCam);
      await expectReceiving(bobCam);
    } finally {
      await a.close();
      await b.close();
      await c.close();
    }
  });

  test('the keyboard alone: Tab to a tile, Enter to enlarge, Full screen reachable, Escape back with focus returned; no animation, and 44 px targets', async ({ browser }) => {
    const b = await person(browser, 'bob');
    const c = await person(browser, 'carol', { viewport: { width: 1280, height: 900 }, reducedMotion: 'reduce' });
    try {
      await join(b);
      await join(c);
      await roomIs([vw.bob, vw.carol]);
      await cameraOn(b);
      const show = voiceRegion(c.page).getByRole('button', { name: 'Show video (1)' });
      await expect(show).toBeVisible({ timeout: 15_000 });
      await show.focus();
      await c.page.keyboard.press('Enter');
      const call = callView(c.page);
      await expect(call).toBeVisible();
      // The live region says bob's camera only when it changes while the
      // view is open: it was on before.
      await expect(call.getByRole('status')).toHaveText('');
      // Tab from Hide video, the view's last control, to bob's tile.
      await call.getByRole('button', { name: 'Hide video' }).focus();
      const focused = c.page.locator(':focus');
      let found = false;
      for (let i = 0; i < 10 && !found; i++) {
        await c.page.keyboard.press('Tab');
        found = (await focused.getAttribute('aria-label')) === "Enlarge bob's video";
      }
      expect(found, "Tab reaches bob's tile").toBe(true);
      await c.page.keyboard.press('Enter');
      await expect(focused).toHaveAttribute('aria-label', "bob's video is enlarged");
      // No animation runs for the layout change, and none is set.
      expect(await call.evaluate((el) => el.getAnimations({ subtree: true }).length)).toBe(0);
      const motion = await call.locator('li, figure').evaluateAll((els) => els.map((el) => [getComputedStyle(el).transitionDuration, getComputedStyle(el).animationName]));
      for (const [duration, animation] of motion) {
        expect(duration).toBe('0s');
        expect(animation).toBe('none');
      }
      await expectTargets(c.page);
      // Full screen is the next stop, and works.
      await c.page.keyboard.press('Tab');
      await expect(focused).toHaveText('Full screen');
      await c.page.keyboard.press('Enter');
      await expect.poll(() => c.page.evaluate(() => document.fullscreenElement?.getAttribute('aria-label') ?? null)).toBe("bob's video is enlarged");
      await c.page.evaluate(() => document.exitFullscreen());
      await expect.poll(() => c.page.evaluate(() => document.fullscreenElement)).toBeNull();
      // Focus comes back to Full screen, so the next Escape is the view's.
      await expect(focused).toHaveText('Full screen');
      // Escape returns to the grid, with focus on bob's tile.
      await c.page.keyboard.press('Escape');
      await expect(call).not.toContainText('enlarged');
      await expect(focused).toHaveAttribute('aria-label', "Enlarge bob's video");
      await expect(call.locator('ul')).toHaveClass(/call-grid cols-2 rows-1/);
    } finally {
      await b.close();
      await c.close();
    }
  });

  test('Hide non-video participants keeps only cameras, persists across a reload, and an empty grid closes without opening itself again', async ({ browser }) => {
    const b = await person(browser, 'bob');
    const c = await person(browser, 'carol', { viewport: { width: 1280, height: 900 } });
    try {
      await join(b);
      await join(c);
      await roomIs([vw.bob, vw.carol]);
      await cameraOn(b);
      await voiceRegion(c.page).getByRole('button', { name: 'Show video (1)' }).click({ timeout: 15_000 });
      let call = callView(c.page);
      const toggle = call.getByRole('button', { name: 'Hide non-video participants' });
      await expect(toggle).toHaveAttribute('aria-pressed', 'false');
      await expect(call.getByRole('listitem')).toHaveCount(2);
      await expect(call.getByRole('listitem').filter({ hasText: 'carol' })).toContainText('Camera off');
      await toggle.click();
      await expect(toggle).toHaveAttribute('aria-pressed', 'true');
      await expect(call.getByRole('listitem')).toHaveCount(1);
      await expect(call.getByLabel("bob's camera")).toBeVisible();
      await expect(call.getByRole('listitem').filter({ hasText: 'carol' })).toHaveCount(0);

      await c.page.reload();
      await openChannel(c.page, vw.channel);
      await join(c);
      await expect(callView(c.page)).toHaveCount(0);
      await voiceRegion(c.page).getByRole('button', { name: 'Show video (1)' }).click({ timeout: 15_000 });
      call = callView(c.page);
      await expect(call.getByRole('button', { name: 'Hide non-video participants' })).toHaveAttribute('aria-pressed', 'true');
      await expect(call.getByRole('listitem')).toHaveCount(1);

      await voiceRegion(b.page).getByRole('button', { name: 'Camera', exact: true }).click();
      await expect(callView(c.page)).toHaveCount(0, { timeout: 15_000 });
      await expect(voiceRegion(c.page).getByRole('button', { name: /Show video/ })).toHaveCount(0);
      await cameraOn(b);
      const show = voiceRegion(c.page).getByRole('button', { name: 'Show video (1)' });
      await expect(show).toBeVisible({ timeout: 15_000 });
      await expect(show).toBeFocused();
      await expect(callView(c.page)).toHaveCount(0);
    } finally {
      await b.close();
      await c.close();
    }
  });

  test("the owner turns use_camera off for members: bob's tile on carol's page goes to Camera off, and bob's page ends its track and says why", async ({ browser }) => {
    const b = await person(browser, 'bob');
    const c = await person(browser, 'carol', { viewport: { width: 1280, height: 900 } });
    try {
      await join(b);
      await join(c);
      await roomIs([vw.bob, vw.carol]);
      await cameraOn(b);
      await voiceRegion(c.page).getByRole('button', { name: 'Show video (1)' }).click({ timeout: 15_000 });
      const call = callView(c.page);
      const bobCam = call.getByLabel("bob's camera");
      await expectReceiving(bobCam);
      const tile = call.getByRole('listitem').filter({ hasText: 'bob' });
      // carol has bob enlarged and full screen when it happens.
      await call.getByRole('button', { name: "Enlarge bob's video" }).click();
      await tile.getByRole('button', { name: 'Full screen' }).click();
      await expect.poll(() => c.page.evaluate(() => document.fullscreenElement?.getAttribute('aria-label') ?? null)).toBe("bob's video is enlarged");

      await memberCamera(false);
      try {
        // Within the server's bound: at once, and one pass and the removal
        // timeout if LiveKit refuses (§4.6).
        await expect(tile).toContainText('Camera off', { timeout: 10_000 });
        await expect(bobCam).toHaveCount(0);
        // The tile is no longer enlarged: it leaves full screen, and focus
        // goes to Hide video, back in the grid.
        await expect.poll(() => c.page.evaluate(() => document.fullscreenElement === null)).toBe(true);
        await expect(call).not.toContainText('enlarged');
        await expect(call.getByRole('button', { name: 'Hide video' })).toBeFocused();
        await expect(call.getByRole('status')).toHaveText("bob's camera is off");
        await expect.poll(() => camStates(b.page), { timeout: 10_000 }).toEqual(['ended']);
        await expect(voiceRegion(b.page)).toContainText('Camera is not allowed for you.', { timeout: 10_000 });
        await expect(voiceRegion(b.page)).not.toContainText('Sending video');
      } finally {
        await memberCamera(true);
      }
    } finally {
      await b.close();
      await c.close();
    }
  });
});
