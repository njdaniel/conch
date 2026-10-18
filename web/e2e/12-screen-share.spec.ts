// Screen share in the browser (issue #444). Three people in three browser
// contexts against the conchd and pinned LiveKit of voice-world.ts, each on
// Chromium's fake devices: the screen picker is answered with a fake screen
// with a sound track (playwright.config.ts). The fake screen captures at the
// size and rate it is asked for, so at 3840x2160 and 60 fps, published with
// 720p and 1080p layers below it (issue #565). Without Docker the tests fail
// when CI is set and are skipped otherwise, as in 10-voice.
//
// A share carries on when the room is renewed under it (issue #516): another
// member disabled renews the room, and the sharer's page claims the share
// again and publishes the same capture in the new one, with no picker. It
// does so with LiveKit's signal reaching the sharer late (issue #525), the
// order a slow CI runner gave: conchd's answer to the claim is read before
// LiveKit tells the page of the grant. A room renewed again while the share
// waits for that grant in the new room is resumed once more (issue #535).
//
// What this cannot try: a real system's sound, macOS, Windows, Firefox,
// Safari, iOS and Android. Those cells of the plan's section 2 are for a
// person with the devices (#474).
import type { Browser, BrowserContextOptions, Page } from '@playwright/test';
import { channelButton, expect, expectFits, phones, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type Member, type VoiceWorld, type Watch } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const shareView = (page: Page) => page.getByRole('region', { name: 'Shared screen' });

// A narrow window, a phone's width, for the layer test: what decides the
// layer is the size of the view, not the device.
const narrow: BrowserContextOptions = { viewport: { width: 360, height: 740 } };
// A full-HD desktop window for the same test. The smallest layer is 720p
// (issue #565), so a 1280x720 window, Playwright's desktop default, needs no
// larger one than a phone does.
const fullHD: BrowserContextOptions = { viewport: { width: 1920, height: 1080 } };

// Installed before the page's own scripts: keeps every peer connection the
// SDK makes (for getStats), keeps the screen the picker gave (so a test can
// end it as the browser's stop control does), and counts microphone requests.
const instrument = () => {
  const w = window as unknown as { __pcs: RTCPeerConnection[]; __screen: MediaStream | null; __micAsks: number; __picks: number };
  w.__pcs = [];
  w.__screen = null;
  w.__micAsks = 0;
  w.__picks = 0;
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
  const display = devices.getDisplayMedia.bind(devices);
  devices.getDisplayMedia = async (options?: DisplayMediaStreamOptions) => {
    w.__picks++;
    const stream = await display(options);
    w.__screen = stream;
    return stream;
  };
  const user = devices.getUserMedia.bind(devices);
  devices.getUserMedia = (constraints?: MediaStreamConstraints) => {
    if (constraints?.audio !== undefined && constraints.audio !== false) w.__micAsks++;
    return user(constraints);
  };
};

// A switch for LiveKit's signal to a page: once on, every socket the page
// opens to LiveKit from then gets its frames from the server 500 ms late, in
// order. Installed before the page loads: a route reaches only the sockets
// made after it.
interface LateSignal {
  on: boolean;
}

async function routeSignal(page: Page, late: LateSignal): Promise<void> {
  await page.routeWebSocket(/\/rtc/, (ws) => {
    const server = ws.connectToServer();
    const slow = late.on;
    ws.onMessage((m) => {
      server.send(m);
    });
    server.onMessage((m) => {
      if (slow) {
        setTimeout(() => {
          ws.send(m);
        }, 500);
      } else {
        ws.send(m);
      }
    });
  });
}

interface Inbound {
  readonly frames: number;
  readonly width: number;
  readonly audioBytes: number;
}

// inbound sums what this page has received: video frames decoded, the width
// of the latest video frame, and audio bytes.
async function inbound(page: Page): Promise<Inbound> {
  return page.evaluate(async () => {
    const w = window as unknown as { __pcs: RTCPeerConnection[] };
    let frames = 0;
    let width = 0;
    let audioBytes = 0;
    for (const pc of w.__pcs) {
      // A connection closed (a room renewed, issue #516) has no stats to give.
      if (pc.signalingState === 'closed') continue;
      const stats = await pc.getStats();
      stats.forEach((r: { type: string; kind?: string; framesDecoded?: number; frameWidth?: number; bytesReceived?: number }) => {
        if (r.type !== 'inbound-rtp') return;
        if (r.kind === 'video') {
          frames += r.framesDecoded ?? 0;
          width = Math.max(width, r.frameWidth ?? 0);
        } else if (r.kind === 'audio') {
          audioBytes += r.bytesReceived ?? 0;
        }
      });
    }
    return { frames, width, audioBytes };
  });
}

// expectComposerShown checks, as the page is (nothing is scrolled to it), that
// the composer's box and Send lie wholly inside the window and that a tap on
// each reaches it: nothing is drawn over them.
async function expectComposerShown(page: Page, what: string): Promise<void> {
  const window = page.viewportSize();
  if (window === null) throw new Error('no window size');
  for (const element of [page.getByLabel('Message #voice-room'), page.getByRole('button', { name: 'Send' })]) {
    const box = await element.boundingBox();
    if (box === null) throw new Error(`${what}: the composer has no box`);
    const where = `${what}: at y ${String(box.y)}, ${String(box.height)} high, in a ${String(window.height)} px window`;
    expect(box.y, where).toBeGreaterThanOrEqual(0);
    expect(box.y + box.height, where).toBeLessThanOrEqual(window.height);
    const reached = await element.evaluate((el) => {
      const r = el.getBoundingClientRect();
      const at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      return at !== null && (at === el || el.contains(at));
    });
    expect(reached, `${what}: something is drawn over the composer`).toBe(true);
  }
}

const micAsks = (page: Page) => page.evaluate(() => (window as unknown as { __micAsks: number }).__micAsks);

test.describe('screen share with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('screen share needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the screen share suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  interface Someone {
    readonly page: Page;
    readonly watch: Watch;
    readonly member: Member;
    readonly close: () => Promise<void>;
  }

  async function person(browser: Browser, who: 'alice' | 'bob' | 'carol' | Member, options?: BrowserContextOptions, late?: LateSignal): Promise<Someone> {
    const context = await browser.newContext(options);
    const page = await context.newPage();
    const watch = watchPage(page, vw);
    await page.addInitScript(instrument);
    if (late !== undefined) await routeSignal(page, late);
    const member = typeof who === 'string' ? vw[who] : who;
    await signIn(page, vw, member.token, member.name);
    await channelButton(page, vw.channel).click();
    return { page, watch, member, close: () => context.close() };
  }

  async function join(p: Someone, how: 'Join voice' | 'Watch only'): Promise<void> {
    await voiceRegion(p.page).getByRole('button', { name: how }).click();
    await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
  }

  async function share(p: Someone): Promise<void> {
    const voice = voiceRegion(p.page);
    // Said before the picker opens. Which sound depends on the platform the
    // browser reports (Playwright's desktop Chrome says Windows).
    await expect(voice).toContainText(/Sound: .*tab shares its (own )?sound/);
    await voice.getByRole('button', { name: 'Share screen' }).click();
    await expect(voice).toContainText('You are sharing your screen with its sound.', { timeout: 30_000 });
  }

  // How many times the picker was opened on this page.
  const picks = (page: Page) => page.evaluate(() => (window as unknown as { __picks: number }).__picks);

  async function watching(p: Someone, who = 'alice'): Promise<void> {
    const voice = voiceRegion(p.page);
    await voice.getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
    await expect(shareView(p.page)).toContainText(`${who} is sharing`);
    // Frames arrive and keep arriving.
    await expect.poll(async () => (await inbound(p.page)).frames, { timeout: 30_000 }).toBeGreaterThan(10);
    const before = (await inbound(p.page)).frames;
    await expect.poll(async () => (await inbound(p.page)).frames, { timeout: 15_000 }).toBeGreaterThan(before);
  }

  // Nothing secret in what the page said, and no policy refused anything: the
  // page's Permissions-Policy lets it capture a screen.
  //
  // One report is expected, once per share on the sharer's page: Chromium
  // checks the camera's policy too when it opens the screen picker, and
  // reports camera=() as a violation, though the capture goes ahead (seen on
  // the pinned Chromium with and without its fake devices). The camera stays
  // off; the report is counted, so any other one fails.
  const cameraReport = 'error: Permissions policy violation: camera is not allowed in this document.';
  async function expectClean(p: Someone, shares = 0): Promise<void> {
    await p.watch.settled();
    const said = p.watch.console.join('\n');
    for (const s of vw.secrets) expect(said, 'a secret in the console').not.toContain(s);
    const reports = p.watch.console.filter((l) => /Content Security Policy|Refused to|[Pp]ermissions policy/.test(l));
    expect(reports.filter((l) => l !== cameraReport)).toEqual([]);
    expect(reports.length, 'camera policy reports, one per share').toBe(shares);
  }

  async function shareRows(): Promise<{ action: string; detail: string }[]> {
    return (await vw.audit()).filter((e) => e.action.startsWith('voice_share_')).map(({ action, detail }) => ({ action, detail }));
  }

  test('a member shares a screen with sound; another watches it without a microphone; a third is told who is sharing; stopping closes the view', async ({ browser }) => {
    const before = (await shareRows()).length;
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    const c = await person(browser, 'carol');
    try {
      await join(a, 'Join voice');
      await join(b, 'Watch only');
      await join(c, 'Join voice');
      await share(a);

      // B watches, having asked for no microphone, and the share's sound
      // arrives while A's microphone is shut.
      await expect(voiceRegion(b.page)).toContainText('alice is sharing a screen');
      await watching(b);
      expect(await micAsks(b.page), 'a watch-only page asks for no microphone').toBe(0);
      await expect(shareView(b.page)).toContainText('with sound', { timeout: 15_000 });
      const sound = (await inbound(b.page)).audioBytes;
      await expect.poll(async () => (await inbound(b.page)).audioBytes, { timeout: 15_000 }).toBeGreaterThan(sound + 2000);
      await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('alice (sharing)');

      // C tries to share and is told A is.
      await voiceRegion(c.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voiceRegion(c.page)).toContainText('alice is sharing. One person shares at a time.');

      // A stops; B's view closes.
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(voiceRegion(a.page)).toContainText('You stopped sharing.');
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });
      await expect(voiceRegion(b.page).getByRole('button', { name: 'Watch' })).toHaveCount(0);
      await expect.poll(async () => (await shareRows()).slice(before).map((r) => r.action), { timeout: 15_000 }).toEqual(['voice_share_started', 'voice_share_stopped']);
      expect((await shareRows()).at(-1)?.detail).toContain('reason=self');

      // B chooses to talk: only now is the microphone asked for.
      await voiceRegion(b.page).getByRole('button', { name: 'Use microphone' }).click();
      await expect(voiceRegion(b.page)).not.toContainText('Watch only');
      await expect.poll(() => micAsks(b.page)).toBe(1);
      await expectClean(a, 1);
      for (const p of [b, c]) await expectClean(p);
    } finally {
      for (const p of [a, b, c]) await p.close();
    }
  });

  test("the browser's own stop control, leaving voice and an operator each end the share for the viewer, and the sharer is told why", async ({ browser }) => {
    const before = (await shareRows()).length;
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await join(a, 'Join voice');
      await join(b, 'Watch only');

      // The browser's stop control: the capture ends.
      await share(a);
      await watching(b);
      await a.page.evaluate(() => {
        const screen = (window as unknown as { __screen: MediaStream | null }).__screen;
        // What the browser does when its own stop control is pressed.
        screen?.getVideoTracks()[0]?.dispatchEvent(new Event('ended'));
      });
      await expect(voiceRegion(a.page)).toContainText('Sharing stopped from the browser.');
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });

      // An operator stops it. The sharer is told it was an operator (issue
      // #487), from the reason in their own presence; the viewer is told
      // nothing of why.
      await share(a);
      await watching(b);
      expect(await vw.stopShareAsOperator()).toBe(204);
      await expect(voiceRegion(a.page)).toContainText('An operator stopped your share.', { timeout: 15_000 });
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });
      await expect(voiceRegion(b.page)).not.toContainText(/operator|ended your share/);

      // Leaving voice.
      await share(a);
      await watching(b);
      await voiceRegion(a.page).getByRole('button', { name: 'Leave voice' }).click();
      await expect(voiceRegion(a.page)).toContainText('Not in voice', { timeout: 10_000 });
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });

      await expect
        .poll(async () => (await shareRows()).slice(before).map((r) => `${r.action} ${/reason=(\w+)/.exec(r.detail)?.[1] ?? ''}`.trim()), { timeout: 15_000 })
        .toEqual([
          'voice_share_started',
          'voice_share_stopped self',
          'voice_share_started',
          'voice_share_stopped operator',
          'voice_share_started',
          'voice_share_stopped self',
        ]);
      await expectClean(a, 3);
      await expectClean(b);
    } finally {
      for (const p of [a, b]) await p.close();
    }
  });

  // Issue #516. Disabling a member renews every voice room they are in, which
  // ends the share in progress (reason=rotated). The sharer's page claims it
  // again in the new room and publishes the same capture: the audit shows the
  // share stopped by the renewal and started again. A sharer who is the one
  // disabled gets no session, and the share does not come back.
  test('a share carries on when another member is disabled, and ends for good when the sharer is', async ({ browser }) => {
    const before = (await shareRows()).length;
    const dave = await vw.addMember('dave');
    const erin = await vw.addMember('erin');
    const late: LateSignal = { on: false };
    const a = await person(browser, 'alice', undefined, late);
    const b = await person(browser, 'bob');
    const d = await person(browser, dave);
    const e = await person(browser, erin);
    try {
      await join(a, 'Join voice');
      await join(b, 'Watch only');
      await join(d, 'Join voice');
      await share(a);
      await watching(b);

      // Another member is disabled: the room is renewed, every page
      // reconnects, and the share is back without anyone doing anything.
      // The sharer hears from the new room's LiveKit late, so the claim's
      // grant reaches the page after conchd's answer (issue #525).
      late.on = true;
      await vw.disable(dave);
      await expect(voiceRegion(a.page)).toContainText('The voice room was renewed, and your share carried on in the new one.', { timeout: 30_000 });
      await expect(voiceRegion(a.page)).toContainText('You are sharing your screen with its sound.');
      expect(await picks(a.page), 'the picker is not opened again').toBe(1);
      await expect(shareView(b.page)).toContainText('alice is sharing', { timeout: 30_000 });
      await expect.poll(async () => (await inbound(b.page)).frames, { timeout: 30_000 }).toBeGreaterThan(10);
      const frames = (await inbound(b.page)).frames;
      await expect.poll(async () => (await inbound(b.page)).frames, { timeout: 15_000 }).toBeGreaterThan(frames);
      await expect
        .poll(async () => (await shareRows()).slice(before).map((r) => `${r.action} ${/reason=(\w+)/.exec(r.detail)?.[1] ?? ''}`.trim()), { timeout: 15_000 })
        .toEqual(['voice_share_started', 'voice_share_stopped rotated', 'voice_share_started']);

      // The sharer is the one disabled: the share ends, and does not come
      // back; the capture is stopped.
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(voiceRegion(a.page)).toContainText('You stopped sharing.');
      await join(e, 'Join voice');
      await share(e);
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });
      await watching(b, 'erin');
      const middle = (await shareRows()).length;
      await vw.disable(erin);
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 30_000 });
      await expect
        .poll(() => e.page.evaluate(() => (window as unknown as { __screen: MediaStream | null }).__screen?.getVideoTracks()[0]?.readyState ?? 'gone'), { timeout: 30_000 })
        .toBe('ended');
      // Long enough for a resume to have been claimed (it takes a reconnect).
      await b.page.waitForTimeout(10_000);
      expect((await shareRows()).slice(middle).map((r) => `${r.action} ${/reason=(\w+)/.exec(r.detail)?.[1] ?? ''}`.trim())).toEqual(['voice_share_stopped rotated']);
      await expect(voiceRegion(b.page).getByRole('button', { name: 'Watch' })).toHaveCount(0);
      await expectClean(a, 1);
      for (const p of [b, d]) await expectClean(p);
    } finally {
      for (const p of [a, b, d, e]) await p.close();
    }
  });

  // Issue #535. Two members disabled one after the other: the second renews
  // the room while the sharer's page is publishing the share again in the
  // first new room, waiting for LiveKit's grant (its signal is late, as in
  // the test above). The share is not ended: it is resumed once more, in the
  // room after, and the viewer still follows it.
  test('a share carries on when a second member is disabled while it is being resumed', async ({ browser }) => {
    const before = (await shareRows()).length;
    const gina = await vw.addMember('gina');
    const hugo = await vw.addMember('hugo');
    const late: LateSignal = { on: false };
    const a = await person(browser, 'alice', undefined, late);
    const b = await person(browser, 'bob');
    const g = await person(browser, gina);
    const h = await person(browser, hugo);
    try {
      await join(a, 'Join voice');
      await join(b, 'Watch only');
      await join(g, 'Join voice');
      await join(h, 'Join voice');
      await share(a);
      await watching(b);

      // The resume's claim answered: the page now waits for LiveKit's grant,
      // which comes late, and the second member is disabled meanwhile.
      late.on = true;
      const claimed = a.page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname.endsWith('/voice/share') && r.status() === 200, { timeout: 30_000 });
      await vw.disable(gina);
      await claimed;
      await vw.disable(hugo);

      await expect(voiceRegion(a.page)).toContainText('The voice room was renewed, and your share carried on in the new one.', { timeout: 45_000 });
      await expect(voiceRegion(a.page)).toContainText('You are sharing your screen with its sound.');
      expect(await picks(a.page), 'the picker is not opened again').toBe(1);
      // Both renewals ended a share, the second the one being resumed when
      // it came, and the share started again after each.
      await expect
        .poll(async () => (await shareRows()).slice(before).map((r) => `${r.action} ${/reason=(\w+)/.exec(r.detail)?.[1] ?? ''}`.trim()), { timeout: 15_000 })
        .toEqual(['voice_share_started', 'voice_share_stopped rotated', 'voice_share_started', 'voice_share_stopped rotated', 'voice_share_started']);
      await expect(shareView(b.page)).toContainText('alice is sharing', { timeout: 30_000 });
      await expect.poll(async () => (await inbound(b.page)).frames, { timeout: 30_000 }).toBeGreaterThan(10);
      const frames = (await inbound(b.page)).frames;
      await expect.poll(async () => (await inbound(b.page)).frames, { timeout: 15_000 }).toBeGreaterThan(frames);

      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(voiceRegion(a.page)).toContainText('You stopped sharing.');
      await expectClean(a, 1);
      await expectClean(b);
    } finally {
      for (const p of [a, b, g, h]) await p.close();
    }
  });

  // Issue #536: the watch view is drawn on the stage, apart from the voice
  // strip. Folding the members column, opening Settings and the members
  // panel, and narrowing and widening the window restart nothing: the same
  // video element keeps receiving frames, on the same connections.
  test('watching and voice carry on while panels open and close and the window narrows and widens', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob', { viewport: { width: 1440, height: 900 } });
    try {
      await join(a, 'Join voice');
      await join(b, 'Join voice');
      await share(a);
      await watching(b);
      const kept = () =>
        b.page.evaluate(() => {
          const w = window as unknown as { __kept?: Element | null; __pcs: RTCPeerConnection[] };
          const video = document.querySelector('[aria-label="Shared screen"] video');
          w.__kept ??= video;
          return { same: video !== null && w.__kept === video, connections: w.__pcs.length };
        });
      const before = await kept();
      expect(before.same).toBe(true);

      await b.page.getByRole('button', { name: 'Members' }).click();
      await b.page.getByRole('button', { name: 'Settings' }).click();
      await b.page.keyboard.press('Escape');
      await voiceRegion(b.page).getByRole('button', { name: 'Voice options' }).click();
      await b.page.setViewportSize({ width: 390, height: 844 });
      await b.page.getByRole('button', { name: 'Members' }).click();
      await expect(b.page.getByRole('region', { name: 'Members list' })).toBeFocused();
      await b.page.keyboard.press('Escape');
      await expectFits(b.page, 'watching at phone width');
      // With Voice options closed again, the composer is on the screen while
      // watching, without scrolling to it.
      await voiceRegion(b.page).getByRole('button', { name: 'Voice options' }).click();
      await expectComposerShown(b.page, 'watching at 390×844');
      await b.page.setViewportSize({ width: 360, height: 740 });
      await expectFits(b.page, 'watching at 360×740');
      await expectComposerShown(b.page, 'watching at 360×740');
      await b.page.setViewportSize({ width: 768, height: 1024 });
      await b.page.setViewportSize({ width: 1440, height: 900 });

      expect(await kept()).toEqual(before);
      const frames = (await inbound(b.page)).frames;
      await expect.poll(async () => (await inbound(b.page)).frames, { timeout: 15_000 }).toBeGreaterThan(frames);
      await expect(voiceRegion(b.page)).toContainText('Connected in #voice-room');
      await expect(voiceRegion(a.page)).toContainText('You are sharing your screen with its sound.');
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });
      await expectClean(a, 1);
      await expectClean(b);
    } finally {
      for (const p of [a, b]) await p.close();
    }
  });

  test('a phone-wide viewer receives a smaller layer than a full-screen desktop viewer, and the panel fits it', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob', fullHD);
    const c = await person(browser, 'carol', narrow);
    try {
      await join(a, 'Join voice');
      await join(b, 'Watch only');
      await join(c, 'Watch only');
      await share(a);
      await watching(b);
      await watching(c);
      await shareView(b.page).getByRole('button', { name: 'Full screen' }).click();
      await expect.poll(() => b.page.evaluate(() => document.fullscreenElement !== null)).toBe(true);
      // LiveKit moves each viewer to the layer its view needs as bandwidth
      // allows: the full-screen desktop to a larger one than the phone.
      await expect
        .poll(
          async () => {
            const [wide, narrow] = [(await inbound(b.page)).width, (await inbound(c.page)).width];
            return wide > narrow && narrow > 0 && narrow < 1920 ? 'smaller' : `desktop ${String(wide)}, phone ${String(narrow)}`;
          },
          { timeout: 60_000, intervals: [1000] },
        )
        .toBe('smaller');
      await expectFits(c.page, 'the phone watching a share');
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(shareView(c.page)).toHaveCount(0, { timeout: 15_000 });
      await expectClean(a, 1);
      for (const p of [b, c]) await expectClean(p);
    } finally {
      for (const p of [a, b, c]) await p.close();
    }
  });

  test('on a phone the share and watch controls fit the screen', async ({ browser }) => {
    for (const [size, options] of phones) {
      const c = await person(browser, 'carol', options);
      try {
        await expectFits(c.page, `${size}: before joining`);
        await join(c, 'Watch only');
        await expect(voiceRegion(c.page).getByRole('button', { name: 'Share screen' })).toBeVisible();
        await expectFits(c.page, `${size}: watching only, with the share controls`);
      } finally {
        await c.close();
      }
    }
  });
});
