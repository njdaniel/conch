// Screen share in the browser (issue #444). Three people in three browser
// contexts against the conchd and pinned LiveKit of voice-world.ts, each on
// Chromium's fake devices: the screen picker is answered with a fake screen
// with a sound track (playwright.config.ts). The fake screen captures at the
// size and rate it is asked for, by default 1920x1080 at 30 fps, with a 720p
// layer below it. Without Docker the tests fail
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
import type { Browser, BrowserContextOptions, Locator, Page } from '@playwright/test';
import type { VoicePresenceV2 } from '../src/schema/voice';
import { expect, expectFits, openChannel, phones, showView, test } from './fixtures';
import { enter, expectClean as expectCleanPage, inbound, joinVoice, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type Member, type VoiceWorld } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const shareView = (page: Page) => page.getByRole('region', { name: 'Shared screen' });

// A narrow window, a phone's width, for the layer test: what decides the
// layer is the size of the view, not the device.
const narrow: BrowserContextOptions = { viewport: { width: 360, height: 740 } };
// A full-HD desktop window for the same test. The smallest layer is 720p
// (issue #565), so a 1280x720 window, Playwright's desktop default, needs no
// larger one than a phone does.
const fullHD: BrowserContextOptions = { viewport: { width: 1920, height: 1080 } };

// Installed before the page's own scripts: keeps the screen the picker gave (so
// a test can end it as the browser's stop control does), and counts microphone
// requests and picker openings. (The peer connections are kept by enter, for
// inbound.)
const instrument = () => {
  const w = window as unknown as { __screen: MediaStream | null; __micAsks: number; __picks: number };
  w.__screen = null;
  w.__micAsks = 0;
  w.__picks = 0;
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
//
// The server's close is as late as its frames, and comes after them, as on a
// slow network (issue #781). LiveKit closes the socket straight after the
// leave that says the room was deleted: a close passed on at once overtook
// that leave, which never reached the page, and the page took the deleted
// room for a lost connection.
interface LateSignal {
  on: boolean;
}

async function routeSignal(page: Page, late: LateSignal): Promise<void> {
  await page.routeWebSocket(/\/rtc/, (ws) => {
    const server = ws.connectToServer();
    const slow = late.on;
    const after = (send: () => void) => {
      if (slow) setTimeout(send, 500);
      else send();
    };
    ws.onMessage((m) => {
      server.send(m);
    });
    server.onMessage((m) => {
      after(() => {
        ws.send(m);
      });
    });
    // Handled here, closes are no longer passed on by Playwright: each side's
    // is passed on to the other, the server's in its turn.
    ws.onClose((code, reason) => {
      void server.close({ code, reason });
    });
    server.onClose((code, reason) => {
      after(() => {
        void ws.close({ code, reason });
      });
    });
  });
}

// addToRoom makes the page's voice presence, on its socket and on the read it
// makes after one closes, list some people more in the channel's room than
// the server does: the world has three accounts and a phone's strip is only
// tested with many in voice. The people exist in this test alone; they have no
// connection to LiveKit, and the real ones are left as they are.
async function addToRoom(page: Page, extra: number): Promise<void> {
  const crowded = (text: string): string => {
    const doc = JSON.parse(text) as VoicePresenceV2;
    const rooms = doc.rooms.map((room) =>
      room.audience !== undefined
        ? room
        : {
            ...room,
            participants: [
              ...room.participants,
              ...Array.from({ length: extra }, (_, i) => ({ principal_id: 900_001 + i, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' })),
            ],
          },
    );
    return JSON.stringify({ ...doc, rooms });
  };
  await page.routeWebSocket(/\/v1\/voice\/ws/, (ws) => {
    const server = ws.connectToServer();
    ws.onMessage((m) => { server.send(m); });
    server.onMessage((m) => { ws.send(typeof m === 'string' ? crowded(m) : m); });
    ws.onClose((code, reason) => { void server.close({ code, reason }); });
    server.onClose((code, reason) => { void ws.close({ code, reason }); });
  });
  await page.route(/\/v1\/channels\/[^/]+\/voice$/, async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    const reply = await route.fetch();
    return route.fulfill({ response: reply, body: crowded(await reply.text()) });
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

  // A person of this spec: enter (people.ts) with the instrument script, and
  // optionally the late signal and the crowded room.
  function person(browser: Browser, who: 'alice' | 'bob' | 'carol' | Member, options?: BrowserContextOptions, late?: LateSignal, crowd = 0): Promise<Someone> {
    return enter(browser, vw, typeof who === 'string' ? vw[who] : who, {
      ...(options === undefined ? {} : { context: options }),
      before: async (page) => {
        await page.addInitScript(instrument);
        if (late !== undefined) await routeSignal(page, late);
        if (crowd > 0) await addToRoom(page, crowd);
      },
    });
  }

  async function share(p: Someone): Promise<void> {
    const voice = voiceRegion(p.page);
    // Said before the picker opens. Which sound depends on the platform the
    // browser reports (Playwright's desktop Chrome says Windows).
    await expect(voice).toContainText(/Sound: .*tab shares its (own )?sound/);
    await voice.getByRole('button', { name: 'Share screen' }).click();
    await expect(voice).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
  }

  // How many times the picker was opened on this page.
  const picks = (page: Page) => page.evaluate(() => (window as unknown as { __picks: number }).__picks);

  async function watching(p: Someone, who = 'alice'): Promise<void> {
    await p.page.getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
    const view = shareView(p.page);
    await expect(view).toContainText(`${who} · LIVE`);
    await expect(view.locator('audio')).toHaveJSProperty('muted', false);
    await expect(view.getByRole('button', { name: 'Mute share' })).toBeVisible();
    await expectPlaying(p.page);
    // Frames arrive and keep arriving.
    await expect.poll(async () => (await inbound(p.page)).frames, { timeout: 30_000 }).toBeGreaterThan(10);
    const before = (await inbound(p.page)).frames;
    await expect.poll(async () => (await inbound(p.page)).frames, { timeout: 15_000 }).toBeGreaterThan(before);
  }

  // The share's sound is really playing with no further click: not paused
  // (autoplay policy may have refused it) and its clock moving. Unmuted alone
  // proves nothing. Only the viewer's earlier Watch click is a gesture; the
  // config sets no autoplay flag.
  async function expectPlaying(page: Page): Promise<void> {
    const audio = shareView(page).locator('audio');
    await expect.poll(() => audio.evaluate((a: HTMLAudioElement) => !a.paused && a.readyState >= 2), { timeout: 15_000, message: 'the share audio element plays' }).toBe(true);
    const t0 = await audio.evaluate((a: HTMLAudioElement) => a.currentTime);
    await expect.poll(() => audio.evaluate((a: HTMLAudioElement) => a.currentTime), { timeout: 10_000, message: 'the share audio clock advances' }).toBeGreaterThan(t0);
    expect(await audio.evaluate((a: HTMLAudioElement) => a.paused)).toBe(false);
  }

  // Nothing secret anywhere on the page, and no policy refused anything: the
  // page's Permissions-Policy lets it capture a screen.
  //
  // Until the camera was the page's own (ADR-015, #1242), Chromium reported
  // camera=() once per share, from the picker call itself (a bare
  // getDisplayMedia() made it). With camera=(self) it reports nothing, and
  // any report fails. A stopped share's 204 is read to its end, as a transmit
  // report's is, so no request to conchd counts as failed.
  const expectClean = (p: Someone) => expectCleanPage(vw, p);

  async function shareRows(): Promise<{ action: string; detail: string }[]> {
    return (await vw.audit()).filter((e) => e.action.startsWith('voice_share_')).map(({ action, detail }) => ({ action, detail }));
  }

  // Issue #1084 (browser UX audit 4, F7): starting a share leaves no console
  // error. Until #1242 it left one, the camera report that Chromium made by
  // itself when getDisplayMedia opened the picker under camera=(); with the
  // camera the page's own there is none. The share asks for no request that
  // is known to fail.
  test('F7 starting a share at 1440×900 logs no console error', async ({ browser }) => {
    const a = await person(browser, 'alice', { viewport: { width: 1440, height: 900 } });
    try {
      await joinVoice(a, 'Join voice');
      await share(a);
      await a.watch.settled();
      const errors = a.watch.console.filter((l) => l.startsWith('error: '));
      expect(errors).toEqual([]);
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(voiceRegion(a.page)).toContainText('You stopped sharing.');
      await expectClean(a);
    } finally {
      await a.close();
    }
  });

  test('a live quality change appears in the sent and received readouts, and the choice survives a reload', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');
      await share(a);
      await watching(b);
      // Focused, so bob takes the top layer: an unfocused watched tile is held
      // to the lowest (voice-multi-share.md §8).
      await shareView(b.page).getByRole('button', { name: "Focus alice's screen" }).click();
      await expect(shareView(b.page)).toContainText(/\d+×\d+ · \d+ fps · \d+\.\d Mbps/, { timeout: 30_000 });

      const voice = voiceRegion(a.page);
      await voice.getByRole('button', { name: 'Voice options' }).click();
      await voice.getByLabel('Share resolution').selectOption('720p');
      await voice.getByLabel('Share frame rate').selectOption('15');
      await expect(voice).toContainText('Sending: 1280×720 · 15 fps', { timeout: 30_000 });
      await expect(shareView(b.page)).toContainText(/1280×720 · 15 fps · \d+\.\d Mbps/, { timeout: 30_000 });
      expect(await picks(a.page), 'a quality change opens no new picker').toBe(1);
      await expect(voice).toContainText('You are sharing your screen with sound.');

      // End the share before the reload, and wait for its row: a share that
      // ends with the page logs its stop late, in the next test's window.
      await voice.getByRole('button', { name: 'Stop sharing' }).click();
      await expect.poll(async () => (await shareRows()).at(-1)?.action, { timeout: 15_000 }).toBe('voice_share_stopped');
      await a.page.reload();
      await openChannel(a.page, vw.channel);
      await joinVoice(a, 'Join voice');
      await voiceRegion(a.page).getByRole('button', { name: 'Voice options' }).click();
      await expect(voiceRegion(a.page).getByLabel('Share resolution')).toHaveValue('720p');
      await expect(voiceRegion(a.page).getByLabel('Share frame rate')).toHaveValue('15');
      await expectClean(a);
      await expectClean(b);
    } finally {
      for (const p of [a, b]) await p.close();
    }
  });

  // Issue #1660: the sharer's line says what was asked beside what is sent.
  // The fake screen on a runner may or may not reach 60 fps; either outcome is
  // accepted and recorded. A reason must never show while on target.
  test('the sharer\'s readout shows what was asked beside what is sent', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');
      await share(a);
      // The top layer sends only while a viewer takes it: bob watches it
      // focused, as in the live quality change test.
      await watching(b);
      await shareView(b.page).getByRole('button', { name: "Focus alice's screen" }).click();
      const voice = voiceRegion(a.page);
      await voice.getByRole('button', { name: 'Voice options' }).click();
      await voice.getByLabel('Share resolution').selectOption('1080p');
      await voice.getByLabel('Share frame rate').selectOption('60');
      const line = voice.getByRole('status', { name: 'Share quality' });
      await expect(line).toContainText('Asked 1080p · 60 fps', { timeout: 30_000 });
      await expect(line).toContainText(/Sending: \d+×\d+ · \d+ fps/, { timeout: 30_000 });
      // Sample for a while: past the 5 s hold, a reason may appear if the
      // runner falls short, and only then.
      let outcome = 'on target';
      // Samples in a row on target (the rate at 80% of 60 or more, at full
      // size; the component compares the unrounded rate, so 49 shown is
      // safely above 47.99): the reason clears 5 s after recovery and the
      // sampling is about a second apart, so a run of 9 on target must show
      // none.
      let steady = 0;
      for (let i = 0; i < 16; i++) {
        await a.page.waitForTimeout(1000);
        const text = (await line.textContent()) ?? '';
        const sent = /Sending: (\d+)×(\d+) · (\d+) fps/.exec(text);
        const reason = /Limited by (CPU|bandwidth)|smaller than|doesn't say why/.exec(text)?.[0];
        if (reason !== undefined) outcome = `reason: ${reason}`;
        const onTarget = sent !== null && Number(sent[3]) >= 49 && Number(sent[1]) === 1920 && Number(sent[2]) === 1080;
        steady = onTarget ? steady + 1 : 0;
        if (steady >= 9) expect(reason, 'a reason never shows while on target').toBeUndefined();
      }
      test.info().annotations.push({ type: 'readout', description: `${outcome}: ${(await line.textContent()) ?? ''}` });
      await voice.getByRole('button', { name: 'Stop sharing' }).click();
      await expect.poll(async () => (await shareRows()).at(-1)?.action, { timeout: 15_000 }).toBe('voice_share_stopped');
      await expectClean(a);
      await expectClean(b);
    } finally {
      for (const p of [a, b]) await p.close();
    }
  });

  // Issue #1660, review: an unfocused viewer holds the top layer to the lowest,
  // dynacast stops it, and the sent picture is a lower layer. That is the
  // sender doing what viewers asked, not a shortfall: no reason shows.
  test('a share every viewer holds unfocused shows no reason for the smaller layer', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');
      await share(a);
      await watching(b);
      const line = voiceRegion(a.page).getByRole('status', { name: 'Share quality' });
      await expect(line).toContainText('Asked 1080p · 30 fps', { timeout: 30_000 });
      await expect(line).toContainText(/Sending: \d+×\d+ · \d+ fps/, { timeout: 30_000 });
      // Past the 5 s hold and well beyond.
      for (let i = 0; i < 12; i++) {
        await a.page.waitForTimeout(1000);
        const text = (await line.textContent()) ?? '';
        expect(text, 'no reason for a layer nobody takes').not.toMatch(/Limited by|smaller than|doesn't say why/);
      }
      test.info().annotations.push({ type: 'readout', description: (await line.textContent()) ?? '' });
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect.poll(async () => (await shareRows()).at(-1)?.action, { timeout: 15_000 }).toBe('voice_share_stopped');
      await expectClean(a);
      await expectClean(b);
    } finally {
      for (const p of [a, b]) await p.close();
    }
  });

  test('raising the quality during a share reaches the viewer', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');
      await share(a);
      await watching(b);
      const voice = voiceRegion(a.page);
      await voice.getByRole('button', { name: 'Voice options' }).click();
      await voice.getByLabel('Share resolution').selectOption('720p');
      await expect(shareView(b.page)).toContainText(/1280×720/, { timeout: 30_000 });
      await voice.getByLabel('Share resolution').selectOption('1080p');
      await expect(voice).toContainText('Sending: 1920×1080', { timeout: 30_000 });
      // The viewer's layer follows the size of its pane (adaptive stream), so
      // it need not be the top: the picture keeps flowing after the raise.
      await expect(shareView(b.page)).toContainText(/\d+×\d+ · \d+ fps · \d+\.\d Mbps/, { timeout: 30_000 });
      expect(await picks(a.page), 'a quality change opens no new picker').toBe(1);
      await expectClean(a);
      await expectClean(b);
      // End the share and wait for its row, so its stop is not logged late
      // in the next test's window.
      await voice.getByRole('button', { name: 'Stop sharing' }).click();
      await expect.poll(async () => (await shareRows()).at(-1)?.action, { timeout: 15_000 }).toBe('voice_share_stopped');
    } finally {
      for (const p of [a, b]) await p.close();
    }
  });

  // Issue #1340: presence lingers a few seconds after a page closes without
  // leaving, and reads as the person's voice in another client. Watch only on
  // a new page at once is not refused as a viewer of a voice that is gone.
  test('Watch only at once on a new page, after the old one closed in voice, is connected', async ({ browser }) => {
    const first = await person(browser, 'alice');
    await joinVoice(first, 'Join voice');
    await first.close();
    const second = await person(browser, 'alice');
    try {
      await showView(second.page, 'Voice');
      await voiceRegion(second.page).getByRole('button', { name: 'Watch only' }).click();
      await expect(voiceRegion(second.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
      await expect(voiceRegion(second.page)).not.toContainText('Your voice is not connected');
    } finally {
      await second.close();
    }
  });

  test('a member shares a screen with sound; another watches it without a microphone; a third is told who is sharing; stopping closes the view', async ({ browser }) => {
    const before = (await shareRows()).length;
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    const c = await person(browser, 'carol');
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');
      await joinVoice(c, 'Join voice');
      await share(a);

      // B watches, having asked for no microphone, and the share's sound
      // plays at once while A's microphone is shut.
      await expect(b.page.locator('.call-share').filter({ hasText: 'alice · LIVE' })).toBeVisible();
      await watching(b);
      expect(await micAsks(b.page), 'a watch-only page asks for no microphone').toBe(0);
      await expect(shareView(b.page)).toContainText('with sound', { timeout: 15_000 });
      const sound = (await inbound(b.page)).audioBytes;
      await expect.poll(async () => (await inbound(b.page)).audioBytes, { timeout: 15_000 }).toBeGreaterThan(sound + 2000);
      await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('alice (sharing)');

      // C may share beside A; nobody's stream locks the button.
      const cShare = voiceRegion(c.page).getByRole('button', { name: 'Share screen' });
      await expect(cShare).toBeEnabled();
      await expect(cShare).toHaveAccessibleDescription('');

      // A stops; B's view closes, and B is told. A's own note is gone within
      // a few seconds (issue #642, N03).
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(voiceRegion(a.page)).toContainText('You stopped sharing.');
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });
      await expect(voiceRegion(b.page)).toContainText('alice stopped sharing.');
      await expect(voiceRegion(b.page).getByRole('button', { name: 'Watch' })).toHaveCount(0);
      await expect(voiceRegion(a.page)).not.toContainText('You stopped sharing.', { timeout: 8000 });
      await expect.poll(async () => (await shareRows()).slice(before).map((r) => r.action), { timeout: 15_000 }).toEqual(['voice_share_started', 'voice_share_stopped']);
      expect((await shareRows()).at(-1)?.detail).toContain('reason=self');

      // B chooses to talk: only now is the microphone asked for.
      await voiceRegion(b.page).getByRole('button', { name: 'Use microphone' }).click();
      await expect(voiceRegion(b.page)).not.toContainText('Watch only');
      await expect.poll(() => micAsks(b.page)).toBe(1);
      await expectClean(a);
      for (const p of [b, c]) await expectClean(p);
    } finally {
      for (const p of [a, b, c]) await p.close();
    }
  });

  // Issue #1485: the viewer's share volume is kept across shares, and the
  // next share's sound plays (not just unmuted) at that volume.
  test('a second share plays at the volume the viewer set on the first', async ({ browser }) => {
    const before = (await shareRows()).length;
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');
      await share(a);
      await watching(b);
      await shareView(b.page).getByLabel('Share volume').fill('0.25');
      await expect(shareView(b.page).locator('audio')).toHaveJSProperty('volume', 0.25);
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });

      await share(a);
      await watching(b);
      await expect(shareView(b.page).locator('audio')).toHaveJSProperty('volume', 0.25);
      await expect(shareView(b.page).getByLabel('Share volume')).toHaveValue('0.25');
      await expectPlaying(b.page);
      // End it cleanly: a share left running closes later as an idle stop,
      // into the next test's audit window.
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });
      await expect.poll(async () => (await shareRows()).length, { timeout: 15_000 }).toBeGreaterThanOrEqual(before + 4);
    } finally {
      for (const p of [a, b]) await p.close();
    }
  });

  test("the browser's own stop control, leaving voice and an operator each end the share for the viewer, and the sharer is told why", async ({ browser }) => {
    const before = (await shareRows()).length;
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');

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
      expect(await vw.stopShareAsOperator(vw.alice)).toBe(204);
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
      await expectClean(a);
      await expectClean(b);
    } finally {
      for (const p of [a, b]) await p.close();
    }
  });

  // Stop watching says nothing; bob stopping a share alice watches is said
  // for a few seconds. Sharing stays available beside bob's stream.
  test('a watched share that ends is said to have stopped, and Share screen stays available throughout', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Join voice');
      await share(b);
      const alice = voiceRegion(a.page);
      const button = alice.getByRole('button', { name: 'Share screen' });
      await expect(button).toBeEnabled();
      await expect(button).toHaveAccessibleDescription('');
      await expect(a.page.locator('.call-share').filter({ hasText: 'bob · LIVE' })).toBeVisible();

      // Alice stops watching herself: nothing is said of bob's share.
      await watching(a, 'bob');
      await shareView(a.page).getByRole('button', { name: 'Stop watching' }).click();
      await expect(shareView(a.page)).toHaveCount(0);
      await a.page.waitForTimeout(1000);
      await expect(alice).not.toContainText('stopped sharing');
      await expect(button).toBeEnabled();

      await watching(a, 'bob');
      const note = alice.getByRole('status').filter({ hasText: 'bob stopped sharing.' });
      const stoppedAt = Date.now();
      await voiceRegion(b.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(note).toBeVisible({ timeout: 3000 });
      await expect(shareView(a.page)).toHaveCount(0);
      await expect(button).toBeEnabled({ timeout: Math.max(1, stoppedAt + 3000 - Date.now()) });
      await expect(button).toHaveAccessibleDescription('');
      await expect(alice).not.toContainText('One person shares at a time.');
      await expect(note).toHaveCount(0, { timeout: Math.max(1, stoppedAt + 8000 - Date.now()) });
      // Said for a while, not a flash: it went no sooner than it should.
      expect(Date.now() - stoppedAt, 'how long the note was up').toBeGreaterThan(4000);
      await expectClean(b);
      await expectClean(a);

      // The sharer's own note goes at once when another's share starts
      // (issue #642, N03), not after its few seconds.
      await share(b);
      await voiceRegion(b.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(voiceRegion(b.page)).toContainText('You stopped sharing.');
      await share(a);
      await expect(voiceRegion(b.page)).not.toContainText('You stopped sharing.');
      await expect(b.page.locator('.call-share').filter({ hasText: 'alice · LIVE' })).toBeVisible();
      // Ended here, and its audit row waited for, so a test after this one
      // counts only its own rows.
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect.poll(async () => (await shareRows()).at(-1)?.action, { timeout: 15_000 }).toBe('voice_share_stopped');
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
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');
      await joinVoice(d, 'Join voice');
      await share(a);
      await watching(b);

      // Another member is disabled: the room is renewed, every page
      // reconnects, and the share is back without anyone doing anything.
      // The sharer hears from the new room's LiveKit late, so the claim's
      // grant reaches the page after conchd's answer (issue #525).
      late.on = true;
      await vw.disable(dave);
      await expect(voiceRegion(a.page)).toContainText('The voice room was renewed, and your share carried on in the new one.', { timeout: 30_000 });
      await expect(voiceRegion(a.page)).toContainText('You are sharing your screen with sound.');
      expect(await picks(a.page), 'the picker is not opened again').toBe(1);
      await expect(shareView(b.page)).toContainText('alice · LIVE', { timeout: 30_000 });
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
      await joinVoice(e, 'Join voice');
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
      await expectClean(a);
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
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');
      await joinVoice(g, 'Join voice');
      await joinVoice(h, 'Join voice');
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
      await expect(voiceRegion(a.page)).toContainText('You are sharing your screen with sound.');
      expect(await picks(a.page), 'the picker is not opened again').toBe(1);
      // Both renewals ended a share, the second the one being resumed when
      // it came, and the share started again after each.
      await expect
        .poll(async () => (await shareRows()).slice(before).map((r) => `${r.action} ${/reason=(\w+)/.exec(r.detail)?.[1] ?? ''}`.trim()), { timeout: 15_000 })
        .toEqual(['voice_share_started', 'voice_share_stopped rotated', 'voice_share_started', 'voice_share_stopped rotated', 'voice_share_started']);
      await expect(shareView(b.page)).toContainText('alice · LIVE', { timeout: 30_000 });
      await expect.poll(async () => (await inbound(b.page)).frames, { timeout: 30_000 }).toBeGreaterThan(10);
      const frames = (await inbound(b.page)).frames;
      await expect.poll(async () => (await inbound(b.page)).frames, { timeout: 15_000 }).toBeGreaterThan(frames);

      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(voiceRegion(a.page)).toContainText('You stopped sharing.');
      await expectClean(a);
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
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Join voice');
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
      // Voice options, left open, are the Voice view's: under Chat the strip
      // is one row (issue #1028), and the composer is on the screen while
      // watching, without scrolling to it.
      await expect(voiceRegion(b.page).getByRole('button', { name: 'Voice options' })).toBeHidden();
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
      await expect(voiceRegion(a.page)).toContainText('You are sharing your screen with sound.');
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 15_000 });
      await expectClean(a);
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
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Watch only');
      await joinVoice(c, 'Watch only');
      await share(a);
      await watching(b);
      await watching(c);
      // Both are focused, the phone by its Watch: each may take the top
      // layer, and adaptive stream sizes it to the element.
      await shareView(b.page).getByRole('button', { name: "Focus alice's screen" }).click();
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
      await expectClean(a);
      for (const p of [b, c]) await expectClean(p);
    } finally {
      for (const p of [a, b, c]) await p.close();
    }
  });

  // Issue #641, revised by #1028: eight in voice on a phone. The Voice view
  // shows all of them; the strip under Chat is one row of four controls, at
  // most 64 px, with no roster. While a share is watched, who shares is the
  // stage's title and not drawn again under Chat, the message log has at
  // least 200 px at 390×844 (120 px at 360×740) and the composer is on the
  // screen.
  test('on a phone with eight in voice the strip stays compact, joined and while watching', async ({ browser }) => {
    const a = await person(browser, 'alice');
    try {
      await joinVoice(a, 'Join voice');
      for (const [size, options] of phones) {
        const b = await person(browser, 'bob', options, undefined, 6);
        try {
          await joinAndMeasure(size, a, b);
        } finally {
          await b.close();
        }
      }
    } finally {
      await a.close();
    }
  });

  async function joinAndMeasure(size: string, a: Someone, b: Someone): Promise<void> {
    await joinVoice(b, 'Join voice');
    const voice = voiceRegion(b.page);
    // Eight, or a ninth if an earlier page of bob's has not yet left the room on the server.
    const count = voice.getByRole('heading', { name: /^[89] in voice$/ });
    await expect(count).toBeVisible();
    // Nobody shares (the earlier size's share has ended on the server too).
    await expect(voice.getByRole('listitem').filter({ hasText: 'sharing' })).toHaveCount(0, { timeout: 15_000 });
    // The Voice view, where Join voice was, shows everyone (issue #929).
    const chips = voice.getByRole('list', { name: /^[89] in voice$/ }).locator('li:visible');
    await expect.poll(() => chips.count()).toBeGreaterThanOrEqual(8);
    await expect(chips.filter({ hasText: b.member.name })).toHaveCount(1);
    // The strip is under Chat: one row of four, with no roster, however many
    // are in voice (issue #1028).
    await showView(b.page, 'Chat');
    const box = async (el: Locator, what: string) => {
      const found = await el.boundingBox();
      if (found === null) throw new Error(`${size}: ${what} has no box`);
      return found;
    };
    const dock = await box(b.page.locator('.dock'), 'the dock');
    expect(dock.height, `${size}: the dock joined with eight in voice`).toBeLessThanOrEqual(64);
    await expect(voice.getByRole('list')).toBeHidden();
    await expect(count).toBeHidden();
    await expectFits(b.page, `${size}: eight in voice`);

    await share(a);
    await showView(b.page, 'Voice');
    await watching(b);
    await showView(b.page, 'Chat');
    // The focused stream stays on the stage under Chat.
    await expect(shareView(b.page)).toBeVisible();
    const log = await box(b.page.getByRole('log', { name: 'Messages' }), 'the message log');
    // 200 px at 390×844 (issue #1028); #641's 120 px at the smaller phone,
    // where the picture of a share takes the room.
    expect(log.height, `${size}: the message log while watching with eight in voice`).toBeGreaterThanOrEqual(size === '390×844' ? 200 : 120);
    await expectComposerShown(b.page, `${size}: watching with eight in voice`);
    await expectFits(b.page, `${size}: watching with eight in voice`);
    await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
    await expect(voiceRegion(a.page)).toContainText('You stopped sharing.');
  }

  // A phone's Watch focuses the stream (voice-multi-share.md §7.1 phone
  // layout), and the Watch pressed goes with the still card: focus moves onto
  // the stream's stage, so a keyboard's next Escape is the view's and returns
  // to the grid (webcam §7.4 "Escape, or Back to grid, returns").
  test('on a phone, Watch from the keyboard puts focus on the stream, and Escape then returns to the grid', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const c = await person(browser, 'carol', narrow);
    try {
      await joinVoice(a, 'Join voice');
      await joinVoice(c, 'Watch only');
      await share(a);
      const card = c.page.locator('.call-share').filter({ hasText: 'alice · LIVE' });
      const watch = card.getByRole('button', { name: 'Watch' });
      await expect(watch).toBeVisible({ timeout: 30_000 });
      await watch.focus();
      await c.page.keyboard.press('Enter');
      await expect(shareView(c.page)).toBeVisible({ timeout: 30_000 });
      await expect(card).toHaveClass(/main/);
      await expect(card).toBeFocused();
      await expect(c.page.getByRole('button', { name: 'Back to grid' })).toBeVisible();
      await c.page.keyboard.press('Escape');
      await expect(card).not.toHaveClass(/main/);
      await expect(c.page.getByRole('button', { name: 'Back to grid' })).toHaveCount(0);
      // Still watched, unfocused, and focus back on the tile's Focus.
      await expect(shareView(c.page)).toBeVisible();
      await expect(shareView(c.page).getByRole('button', { name: "Focus alice's screen" })).toBeFocused();
      await voiceRegion(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(shareView(c.page)).toHaveCount(0, { timeout: 15_000 });
    } finally {
      await a.close();
      await c.close();
    }
  });

  test('on a phone the share and watch controls fit the screen', async ({ browser }) => {
    for (const [size, options] of phones) {
      const c = await person(browser, 'carol', options);
      try {
        await expectFits(c.page, `${size}: before joining`);
        await joinVoice(c, 'Watch only');
        await expect(voiceRegion(c.page).getByRole('button', { name: 'Share screen' })).toBeVisible();
        await expectFits(c.page, `${size}: watching only, with the share controls`);
      } finally {
        await c.close();
      }
    }
  });
});
